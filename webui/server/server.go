package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/tarm/serial"
)

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type ProfileHandler struct {
	Filename   string
	Profiles   map[string][]int
	CurProfile string
	NumSensors int
	Mutex      sync.Mutex
}

type SerialHandler struct {
	Port       string
	BaudRate   int
	SerialPort *serial.Port
	WriteQueue chan string
	Profile    *ProfileHandler
	NoSerial   bool
	NumSensors int
	Mutex      sync.Mutex
}

// Clients holds all currently connected websocket clients
type Client struct {
	conn *websocket.Conn
	send chan []interface{}
}

var (
	// Global list of all active clients
	clients    = make(map[*Client]bool)
	clientsMux sync.Mutex

	// List of active websocket connections
	activeWebSockets []*websocket.Conn
	wsLock           sync.Mutex

	// Channel for broadcasting messages to all clients
	broadcast = make(chan []interface{}, 256)

	// Shutdown signal channel
	shutdownSignal = make(chan struct{})

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
		EnableCompression: true,
	}

	// Global handlers for WebSocket handlers to access
	profileHandler *ProfileHandler
	serialHandler  *SerialHandler

	// Build directory for static files
	buildDir = filepath.Join(filepath.Dir(filepath.Dir(os.Args[0])), "build")
)

func onStartup() {
	profileHandler.LoadProfiles()

	go serialHandler.WriteLoop()
	go serialHandler.ReadLoop()
}

func onShutdown() {
	log.Println("Cleaning up connections...")

	// Close all websocket connections
	wsLock.Lock()
	for _, ws := range activeWebSockets {
		ws.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "Server shutdown"))
		ws.Close()
	}
	activeWebSockets = nil
	wsLock.Unlock()

	// Close all client connections
	clientsMux.Lock()
	for client := range clients {
		client.conn.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseGoingAway, "Server shutdown"))
		client.conn.Close()
		delete(clients, client)
	}
	clientsMux.Unlock()

	// Close serial connection
	if serialHandler.SerialPort != nil {
		serialHandler.SerialPort.Close()
	}

	// Signal shutdown to all goroutines
	close(shutdownSignal)
}

func NewProfileHandler(filename string, numSensors int) *ProfileHandler {
	return &ProfileHandler{
		Filename:   filename,
		Profiles:   make(map[string][]int),
		CurProfile: "",
		NumSensors: numSensors,
	}
}

func (p *ProfileHandler) LoadProfiles() {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if _, err := os.Stat(p.Filename); os.IsNotExist(err) {
		file, _ := os.Create(p.Filename)
		file.Close()
		return
	}

	data, err := ioutil.ReadFile(p.Filename)
	if err != nil {
		log.Println("Error reading profiles file:", err)
		return
	}

	lines := strings.Split(string(data), "\n")
	for _, line := range lines {
		parts := strings.Fields(line)
		if len(parts) == p.NumSensors+1 {
			name := parts[0]
			thresholds := make([]int, p.NumSensors)
			for i := 0; i < p.NumSensors; i++ {
				thresholds[i], _ = strconv.Atoi(parts[i+1])
			}
			p.Profiles[name] = thresholds
			if p.CurProfile == "" {
				p.CurProfile = name
			}
		}
	}
}

func (p *ProfileHandler) GetCurrentThresholds() []int {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if thresholds, ok := p.Profiles[p.CurProfile]; ok {
		return thresholds
	}
	return make([]int, p.NumSensors)
}

func (p *ProfileHandler) UpdateThreshold(index int, value int) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if p.CurProfile != "" {
		p.Profiles[p.CurProfile][index] = value
		p.saveProfiles()
		broadcastMessage([]interface{}{"thresholds", map[string]interface{}{
			"thresholds": p.GetCurrentThresholds(),
		}})
	}
}

func (p *ProfileHandler) AddProfile(name string, thresholds []int) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	p.Profiles[name] = thresholds
	if p.CurProfile == "" {
		p.Profiles[""] = make([]int, p.NumSensors)
	}
	p.CurProfile = name
	p.saveProfiles()

	broadcastMessage([]interface{}{"thresholds", map[string]interface{}{
		"thresholds": p.GetCurrentThresholds(),
	}})
	broadcastMessage([]interface{}{"get_profiles", map[string]interface{}{
		"profiles": p.GetProfileNames(),
	}})
}

func (p *ProfileHandler) RemoveProfile(name string) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if name == "" {
		return
	}

	delete(p.Profiles, name)
	if name == p.CurProfile {
		p.CurProfile = ""
	}
	p.saveProfiles()

	broadcastMessage([]interface{}{"thresholds", map[string]interface{}{
		"thresholds": p.GetCurrentThresholds(),
	}})
	broadcastMessage([]interface{}{"get_profiles", map[string]interface{}{
		"profiles": p.GetProfileNames(),
	}})
}

func (p *ProfileHandler) ChangeProfile(name string) {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	if _, ok := p.Profiles[name]; ok {
		p.CurProfile = name
		broadcastMessage([]interface{}{"thresholds", map[string]interface{}{
			"thresholds": p.GetCurrentThresholds(),
		}})
		broadcastMessage([]interface{}{"get_cur_profile", map[string]interface{}{
			"cur_profile": name,
		}})
	}
}

func (p *ProfileHandler) GetProfileNames() []string {
	p.Mutex.Lock()
	defer p.Mutex.Unlock()

	var names []string
	for name := range p.Profiles {
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (p *ProfileHandler) saveProfiles() {
	f, err := os.Create(p.Filename)
	if err != nil {
		log.Println("Error creating profiles file:", err)
		return
	}
	defer f.Close()

	for name, thresholds := range p.Profiles {
		if name != "" {
			line := name
			for _, t := range thresholds {
				line += " " + strconv.Itoa(t)
			}
			line += "\n"
			if _, err := f.WriteString(line); err != nil {
				log.Println("Error writing to profiles file:", err)
				return
			}
		}
	}
}

func NewSerialHandler(port string, baudRate int, profile *ProfileHandler, numSensors int, noSerial bool) *SerialHandler {
	return &SerialHandler{
		Port:       port,
		BaudRate:   baudRate,
		Profile:    profile,
		WriteQueue: make(chan string, 100),
		NoSerial:   noSerial,
		NumSensors: numSensors,
	}
}

func (s *SerialHandler) Open() bool {
	if s.NoSerial {
		return true
	}

	s.Mutex.Lock()
	defer s.Mutex.Unlock()

	if s.SerialPort != nil {
		s.SerialPort.Close()
		s.SerialPort = nil
	}

	config := &serial.Config{Name: s.Port, Baud: s.BaudRate, ReadTimeout: time.Second}
	port, err := serial.OpenPort(config)
	if err != nil {
		log.Println("Error opening serial port:", err)
		return false
	}
	s.SerialPort = port
	log.Printf("[SERIAL] Device detected on %s", s.Port)
	thresholds := s.Profile.GetCurrentThresholds()
	log.Printf("[SERIAL] Current thresholds: %v", thresholds)
	return true
}

func (s *SerialHandler) WriteLoop() {
	for command := range s.WriteQueue {
		if s.NoSerial {
			log.Println("Simulated write:", command)
			continue
		}

		if s.SerialPort != nil {
			_, err := s.SerialPort.Write([]byte(command))
			if err != nil {
				log.Println("Error writing to serial port:", err)
			}
		}
	}
}

func (s *SerialHandler) ReadLoop() {
	buffer := make([]byte, 1024)
	lastValues := make([]int, s.NumSensors)

	for {
		if s.NoSerial {
			// Simulate sensor values with normal distribution for more realistic changes
			values := make([]int, s.NumSensors)
			for i := range values {
				// Add random offset based on normal distribution
				offset := int(rand.NormFloat64() * float64(s.NumSensors+1))
				newVal := lastValues[i] + offset
				// Clamp between 0 and 1023
				values[i] = max(0, min(newVal, 1023))
				lastValues[i] = values[i]
			}
			broadcastMessage([]interface{}{"values", map[string]interface{}{
				"values": values,
			}})
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if s.SerialPort == nil {
			if !s.Open() {
				time.Sleep(time.Second)
				continue
			}
			// Apply current thresholds when reconnecting
			thresholds := s.Profile.GetCurrentThresholds()
			for i, t := range thresholds {
				s.WriteQueue <- fmt.Sprintf("%d %d\n", i, t)
			}
		}

		// Send command to fetch values
		s.WriteQueue <- "v\n"

		n, err := s.SerialPort.Read(buffer)
		if err != nil {
			log.Printf("Error reading from serial port: %v, attempting to reconnect", err)
			s.SerialPort = nil
			time.Sleep(time.Second)
			continue
		}

		line := string(buffer[:n])
		parts := strings.Fields(line)
		if len(parts) != s.NumSensors+1 {
			continue
		}

		cmd := parts[0]
		rawValues := make([]int, s.NumSensors)
		normalizedValues := make([]int, s.NumSensors)
		for i := 0; i < s.NumSensors; i++ {
			val, err := strconv.Atoi(parts[i+1])
			if err != nil {
				continue
			}
			rawValues[i] = val
			// Normalize the sensor ordering
			normalizedValues[i] = rawValues[i]
		}

		switch cmd {
		case "v":
			broadcastMessage([]interface{}{"values", map[string]interface{}{
				"values": normalizedValues,
			}})
		case "t":
			// Process threshold updates from device
			curThresholds := s.Profile.GetCurrentThresholds()
			for i, val := range normalizedValues {
				if cur := curThresholds[i]; cur != val {
					s.Profile.UpdateThreshold(i, val)
				}
			}
		case "p":
			broadcastMessage([]interface{}{"thresholds_persisted", map[string]interface{}{
				"thresholds": normalizedValues,
			}})
			log.Printf("Saved thresholds to device: %v", s.Profile.GetCurrentThresholds())
		}

		time.Sleep(10 * time.Millisecond)
	}
}

// broadcast sends a message to all connected clients
func broadcastMessage(message []interface{}) {
	clientsMux.Lock()
	for client := range clients {
		select {
		case client.send <- message:
		default:
			close(client.send)
			delete(clients, client)
		}
	}
	clientsMux.Unlock()
}

// handleWS handles websocket connections
func handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Error upgrading to websocket:", err)
		return
	}

	// Add to active WebSockets list
	wsLock.Lock()
	activeWebSockets = append(activeWebSockets, conn)
	wsLock.Unlock()

	client := &Client{
		conn: conn,
		send: make(chan []interface{}, 256),
	}

	clientsMux.Lock()
	clients[client] = true
	clientsMux.Unlock()

	log.Println("Client connected")

	// Send initial state (immediately, not via goroutine)
	initialMsgs := [][]interface{}{
		{"thresholds", map[string]interface{}{
			"thresholds": profileHandler.GetCurrentThresholds(),
		}},
		{"get_profiles", map[string]interface{}{
			"profiles": profileHandler.GetProfileNames(),
		}},
		{"get_cur_profile", map[string]interface{}{
			"cur_profile": profileHandler.CurProfile,
		}},
	}

	for _, msg := range initialMsgs {
		err = conn.WriteJSON(msg)
		if err != nil {
			log.Printf("Error sending initial state: %v", err)
			conn.Close()
			return
		}
	}

	// Request current thresholds from device
	serialHandler.WriteQueue <- "t\n"

	// Reader goroutine
	go func() {
		defer func() {
			wsLock.Lock()
			for i, ws := range activeWebSockets {
				if ws == conn {
					activeWebSockets = append(activeWebSockets[:i], activeWebSockets[i+1:]...)
					break
				}
			}
			wsLock.Unlock()

			conn.Close()
			clientsMux.Lock()
			delete(clients, client)
			clientsMux.Unlock()

			log.Println("Client disconnected")
		}()

		for {
			var message []interface{}
			err := conn.ReadJSON(&message)
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					log.Printf("error reading websocket message: %v", err)
				}
				break
			}

			if len(message) > 0 {
				action, ok := message[0].(string)
				if !ok {
					continue
				}

				switch action {
				case "update_threshold":
					if len(message) >= 3 {
						if values, ok := message[1].([]interface{}); ok {
							if index, ok := message[2].(float64); ok {
								intValues := make([]int, len(values))
								for i, v := range values {
									if fv, ok := v.(float64); ok {
										intValues[i] = int(fv)
									}
								}
								if idx := int(index); idx >= 0 && idx < len(intValues) {
									profileHandler.UpdateThreshold(idx, intValues[idx])
									serialHandler.WriteQueue <- fmt.Sprintf("%d %d\n", idx, intValues[idx])
								}
							}
						}
					}
				case "save_thresholds":
					serialHandler.WriteQueue <- "s\n"
				case "add_profile":
					if len(message) >= 3 {
						if name, ok := message[1].(string); ok {
							if thresholds, ok := message[2].([]interface{}); ok {
								intThresholds := make([]int, len(thresholds))
								for i, t := range thresholds {
									if ft, ok := t.(float64); ok {
										intThresholds[i] = int(ft)
									}
								}
								profileHandler.AddProfile(name, intThresholds)
							}
						}
					}
				case "remove_profile":
					if len(message) >= 2 {
						if name, ok := message[1].(string); ok {
							profileHandler.RemoveProfile(name)
						}
					}
				case "change_profile":
					if len(message) >= 2 {
						if name, ok := message[1].(string); ok {
							profileHandler.ChangeProfile(name)
							thresholds := profileHandler.GetCurrentThresholds()
							for i, t := range thresholds {
								serialHandler.WriteQueue <- fmt.Sprintf("%d %d\n", i, t)
							}
						}
					}
				}
			}
		}
	}()

	// Writer goroutine
	go func() {
		for message := range client.send {
			err := conn.WriteJSON(message)
			if err != nil {
				return
			}
		}
	}()
}

func getIndex(w http.ResponseWriter, r *http.Request) {
	http.ServeFile(w, r, filepath.Join(buildDir, "index.html"))
}

func main() {
	gamepad := flag.String("gamepad", "/dev/ttyACM0", "Serial port to use (e.g., COM5 on Windows, /dev/ttyACM0 on Linux)")
	port := flag.String("port", "5000", "Port for the server to listen on")
	flag.Parse()

	buildDir = filepath.Clean(buildDir)

	// Setup signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-sigChan
		log.Println("Shutting down...")
		onShutdown()
		os.Exit(0)
	}()

	profileHandler = NewProfileHandler("profiles.txt", 4)
	profileHandler.LoadProfiles()

	serialHandler = NewSerialHandler(*gamepad, 115200, profileHandler, 4, false)
	serialHandler.Open()
	go serialHandler.WriteLoop()
	go serialHandler.ReadLoop()

	r := mux.NewRouter()

	// API routes first (most specific)
	apiRouter := r.PathPrefix("/api").Subrouter()
	apiRouter.HandleFunc("/ws", handleWS)
	apiRouter.HandleFunc("/defaults", func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"profiles":    profileHandler.GetProfileNames(),
			"cur_profile": profileHandler.CurProfile,
			"thresholds":  profileHandler.GetCurrentThresholds(),
		}
		json.NewEncoder(w).Encode(response)
	}).Methods("GET")

	// Static file routes (only when not in NO_SERIAL mode)
	if !serialHandler.NoSerial {
		// Serve index.html for root and /plot
		r.HandleFunc("/", getIndex)
		r.HandleFunc("/plot", getIndex)

		// Serve static files
		staticFileServer := http.FileServer(http.Dir(buildDir))
		r.PathPrefix("/static/").Handler(staticFileServer)
		r.PathPrefix("/favicon.ico").Handler(staticFileServer)
		r.PathPrefix("/manifest.json").Handler(staticFileServer)
		r.PathPrefix("/logo").Handler(staticFileServer)
	}

	http.Handle("/", r)

	// Run startup hooks
	onStartup()

	// Find a non-loopback IPv4 address for display
	ipToShow := "localhost"
	ifaces, err := net.Interfaces()
	if err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				continue
			}
			for _, addr := range addrs {
				var ip net.IP
				switch v := addr.(type) {
				case *net.IPNet:
					ip = v.IP
				case *net.IPAddr:
					ip = v.IP
				}
				if ip == nil || ip.IsLoopback() {
					continue
				}
				ip = ip.To4()
				if ip == nil {
					continue // not an ipv4 address
				}
				ipToShow = ip.String()
				break
			}
			if ipToShow != "localhost" {
				break
			}
		}
	}
	fmt.Printf(" * WebUI can be found at: http://%s:%s\n", ipToShow, *port)

	if err := http.ListenAndServe(":"+*port, nil); err != nil {
		log.Fatal(err)
	}
}
