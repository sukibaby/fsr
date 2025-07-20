package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/tarm/serial"
)

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

	// Channel for broadcasting messages to all clients
	broadcast = make(chan []interface{}, 256)

	upgrader = websocket.Upgrader{
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// Global handlers for WebSocket handlers to access
	profileHandler *ProfileHandler
	serialHandler  *SerialHandler
)

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

func (s *SerialHandler) Open() {
	if s.NoSerial {
		return
	}

	config := &serial.Config{Name: s.Port, Baud: s.BaudRate}
	port, err := serial.OpenPort(config)
	if err != nil {
		log.Println("Error opening serial port:", err)
		return
	}
	s.SerialPort = port
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
	for {
		if s.NoSerial {
			// Simulate sensor values when in no-serial mode
			values := make([]int, s.NumSensors)
			for i := range values {
				values[i] = rand.Intn(1024)
			}
			broadcastMessage([]interface{}{"values", map[string]interface{}{
				"values": values,
			}})
			time.Sleep(10 * time.Millisecond)
			continue
		}

		if s.SerialPort == nil {
			s.Open()
			if s.SerialPort == nil {
				time.Sleep(time.Second)
				continue
			}
		}

		// Send command to fetch values
		s.WriteQueue <- "v\n"

		n, err := s.SerialPort.Read(buffer)
		if err != nil {
			log.Println("Error reading from serial port:", err)
			s.SerialPort = nil
			continue
		}

		line := string(buffer[:n])
		parts := strings.Fields(line)
		if len(parts) != s.NumSensors+1 {
			continue
		}

		cmd := parts[0]
		values := make([]int, s.NumSensors)
		for i := 0; i < s.NumSensors; i++ {
			values[i], _ = strconv.Atoi(parts[i+1])
		}

		switch cmd {
		case "v":
			broadcastMessage([]interface{}{"values", map[string]interface{}{
				"values": values,
			}})
		case "t":
			// Process threshold updates from device
			for i, val := range values {
				if cur := s.Profile.GetCurrentThresholds()[i]; cur != val {
					// TODO: Update thresholds
				}
			}
		case "p":
			broadcastMessage([]interface{}{"thresholds_persisted", map[string]interface{}{
				"thresholds": values,
			}})
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

	client := &Client{
		conn: conn,
		send: make(chan []interface{}, 256),
	}

	clientsMux.Lock()
	clients[client] = true
	clientsMux.Unlock()

	// Send initial thresholds
	client.send <- []interface{}{"thresholds", map[string]interface{}{
		"thresholds": profileHandler.GetCurrentThresholds(),
	}}

	go func() {
		defer func() {
			conn.Close()
			clientsMux.Lock()
			delete(clients, client)
			clientsMux.Unlock()
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

			// Handle incoming messages based on their type
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
							// Apply thresholds to device
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

func main() {
	gamepad := flag.String("gamepad", "/dev/ttyACM0", "Serial port to use (e.g., COM5 on Windows, /dev/ttyACM0 on Linux)")
	port := flag.String("port", "5000", "Port for the server to listen on")
	flag.Parse()

	profileHandler = NewProfileHandler("profiles.txt", 4)
	profileHandler.LoadProfiles()

	serialHandler = NewSerialHandler(*gamepad, 115200, profileHandler, 4, false)
	serialHandler.Open()
	go serialHandler.WriteLoop()
	go serialHandler.ReadLoop()

	r := mux.NewRouter()
	r.HandleFunc("/ws", handleWS)
	r.HandleFunc("/defaults", func(w http.ResponseWriter, r *http.Request) {
		response := map[string]interface{}{
			"profiles":    profileHandler.Profiles,
			"cur_profile": profileHandler.CurProfile,
			"thresholds":  profileHandler.GetCurrentThresholds(),
		}
		json.NewEncoder(w).Encode(response)
	}).Methods("GET")

	http.Handle("/", r)
	log.Printf("Server running on port %s", *port)
	http.ListenAndServe(":"+*port, nil)
}
