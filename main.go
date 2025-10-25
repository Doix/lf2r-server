package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sort" // <-- IMPORTED SORT
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// --- Constants ---

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 1024 * 10 // 10KB, adjust as needed for large achievement strings

	// Game version to check against the client
	gameVersion = 2143
)

// --- Globals ---

var (
	// upgrader specifies parameters for upgrading an HTTP connection to a WebSocket connection.
	upgrader = websocket.Upgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		// Note: CheckOrigin should be properly configured in production!
		// For this example, we'll allow all origins.
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	// nextClientID is an atomic counter for unique client IDs.
	nextClientID = 1
	idMutex      sync.Mutex
)

// --- Structs ---

// RoomConfig defines the structure for a room in the JSON config file.
type RoomConfig struct {
	ID             int    `json:"id"`
	Name           string `json:"name"`
	MaxPlayers     int    `json:"maxPlayers"`
	DefaultLatency int    `json:"defaultLatency"`
}

// Message is a wrapper for messages sent from a client to the hub.
type Message struct {
	data   []byte
	client *Client
}

// Client is a middleman between the websocket connection and the hub.
type Client struct {
	hub *Hub

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan []byte

	// Client's unique ID.
	id int

	// The room this client is currently in.
	room *Room

	// Client's player data.
	name         string
	p1, p2, p3, p4 string
	achievements string
	status       string // For AWAY command
}

// Room represents a single game lobby or in-progress game.
type Room struct {
	id         int
	name       string
	maxPlayers int
	latency    int    // This was the field we mistook for maxPlayers
	status     string // "VACANT", "INGAME"

	// Registered clients in this room.
	clients map[*Client]bool

	// Mutex to protect concurrent access to the clients map.
	mu sync.RWMutex
}

// Hub maintains the set of active clients, rooms, and broadcasts messages.
type Hub struct {
	// Registered clients.
	clients map[*Client]bool

	// Inbound messages from the clients.
	broadcast chan *Message

	// Register requests from the clients.
	register chan *Client

	// Unregister requests from clients.
	unregister chan *Client

	// All active rooms on the server, mapped by ID.
	rooms map[int]*Room
}

// --- Hub Methods ---

// newHub creates a new Hub by loading configuration from a file.
func newHub(configFile string) *Hub {
	// Read the configuration file
	file, err := os.ReadFile(configFile)
	if err != nil {
		log.Fatalf("Failed to read config file %s: %v", configFile, err)
	}

	var roomConfigs []RoomConfig
	if err := json.Unmarshal(file, &roomConfigs); err != nil {
		log.Fatalf("Failed to parse config file %s: %v", configFile, err)
	}

	// Initialize rooms from the config
	rooms := make(map[int]*Room)
	for _, cfg := range roomConfigs {
		rooms[cfg.ID] = &Room{
			id:         cfg.ID,
			name:       cfg.Name,
			maxPlayers: cfg.MaxPlayers,
			latency:    cfg.DefaultLatency,
			status:     "VACANT",
			clients:    make(map[*Client]bool),
		}
		log.Printf("Loaded Room %d: '%s' (Max Players: %d, Default Latency: %d)", cfg.ID, cfg.Name, cfg.MaxPlayers, cfg.DefaultLatency)
	}

	return &Hub{
		broadcast:  make(chan *Message),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
		rooms:      rooms,
	}
}

// run starts the hub's main loop.
func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			// Register client and send them their ID
			h.clients[client] = true
			log.Printf("Client %d connected.", client.id)
			// Send the YOUR_ID message
			// YOUR_ID\n2\n2143\n-999\n-999\n-999
			// We'll just send the ID and the game version.
			msg := fmt.Sprintf("YOUR_ID\n%d\n%d\n-999\n-999\n-999", client.id, gameVersion)
			client.send <- []byte(msg)

		case client := <-h.unregister:
			// Unregister client and clean up
			if _, ok := h.clients[client]; ok {
				log.Printf("Client %d disconnected.", client.id)
				delete(h.clients, client)
				close(client.send)

				// If client was in a room, remove them and notify others
				if client.room != nil {
					client.room.removeClient(client)
				}
			}

		case message := <-h.broadcast:
			// Handle the incoming message
			h.handleMessage(message)
		}
	}
}

// handleMessage parses and routes messages from clients.
func (h *Hub) handleMessage(message *Message) {
	msgStr := string(message.data)
	client := message.client

	// Refactor to use HasPrefix for more robust command parsing
	// This handles commands with no args ("LIST"), commands with space args ("LEAVE 2"),
	// and commands with newline args ("JOIN\n...")

	if msgStr == "LIST" {
		log.Printf("Received command 'LIST' from client %d", client.id)
		// Client requests the room list.
		// "LIST\n\n¶\nRoom\n1\nVACANT\n3\n6071\n0\n\n¶\nRoom..."
		var sb strings.Builder
		sb.WriteString("LIST\n\n") // Start with LIST and the two newlines from log

		roomItems := []string{}

		// --- FIX 1: Sort the rooms by ID ---
		// Get all room IDs
		roomIDs := make([]int, 0, len(h.rooms))
		for id := range h.rooms {
			roomIDs = append(roomIDs, id)
		}
		// Sort the IDs
		sort.Ints(roomIDs)

		// Iterate over the *sorted* IDs
		for _, id := range roomIDs {
			room := h.rooms[id] // Get room from map
			room.mu.RLock()
			// --- FIX 2: Change format from `room.name` to literal "Room" ---
			// Real format: "Room\n<ID>\n<Status>\n<Latency>\n<Unknown>\n<Players>"
			roomStr := fmt.Sprintf("Room\n%d\n%s\n%d\n%d\n%d",
				// "Room" (literal string)
				room.id,           // 1
				room.status,       // "VACANT"
				room.latency,      // e.g., 3 (Default Latency)
				rand.Intn(4000000), // <-- FIX 1: Use random number, not 6071
				len(room.clients), // CurrentPlayers
			)
			room.mu.RUnlock()
			roomItems = append(roomItems, roomStr)
		}

		// Prepend the first separator (as seen in the log)
		if len(roomItems) > 0 {
			sb.WriteString("¶\n")
		}
		// Join the rest, separated by the same marker
		sb.WriteString(strings.Join(roomItems, "\n\n¶\n"))

		// Send the list back to *only* the requesting client.
		client.send <- []byte(sb.String())

	} else if strings.HasPrefix(msgStr, "JOIN\n") {
		log.Printf("Received command 'JOIN' from client %d", client.id)
		parts := strings.Split(msgStr, "\n")
		// Client wants to join a room.
		// "JOIN\n1\ndoix\nP1\nP2\nP3\nP4\n<achievements...>"
		if len(parts) < 8 {
			log.Printf("Client %d sent invalid JOIN message", client.id)
			return
		}

		roomID, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}

		room, ok := h.rooms[roomID]
		if !ok {
			log.Printf("Client %d tried to join non-existent room %d", client.id, roomID)
			// Send UNABLE_TO_JOIN message
			client.send <- []byte(fmt.Sprintf("UNABLE_TO_JOIN\n%d\nNOT_EXIST", roomID))
			return
		}

		// Check room status and capacity
		room.mu.RLock()
		if len(room.clients) >= room.maxPlayers {
			room.mu.RUnlock()
			log.Printf("Client %d failed to join room %d: ROOM_FULL", client.id, roomID)
			client.send <- []byte(fmt.Sprintf("UNABLE_TO_JOIN\n%d\nROOM_FULL", roomID))
			return
		}
		if room.status == "INGAME" {
			room.mu.RUnlock()
			log.Printf("Client %d failed to join room %d: INVALID_STATUS (game started)", client.id, roomID)
			client.send <- []byte(fmt.Sprintf("UNABLE_TO_JOIN\n%d\nINVALID_STATUS", roomID))
			return
		}
		room.mu.RUnlock()

		// If client is already in a room, leave it first.
		if client.room != nil {
			client.room.removeClient(client)
		}

		// Update client data
		client.name = parts[2]
		client.p1 = parts[3]
		client.p2 = parts[4]
		client.p3 = parts[5]
		client.p4 = parts[6]
		client.achievements = parts[7]

		// Add client to the new room
		room.addClient(client)

	} else if strings.HasPrefix(msgStr, "FRAME\n") {
		// In-game frame data.
		// "FRAME\n2\n3\n0\n0\n0\n0\n0\n0"
		if client.room == nil {
			return // Not in a room
		}
		// The server's job is to just reflect this to everyone in the room.
		client.room.broadcast(message.data)

	} else if strings.HasPrefix(msgStr, "CHAT\n") {
		log.Printf("Received command 'CHAT' from client %d", client.id)
		// This is a client-sent chat message
		parts := strings.SplitN(msgStr, "\n", 2) // Safer split
		if client.room == nil || len(parts) < 2 {
			return
		}
		// Format: "CHAT\nMy message text"
		// Server reformats to: "CHAT\n<PlayerID>\n<PlayerName>\n<Message>"
		messageText := parts[1]
		chatMsg := fmt.Sprintf("CHAT\n%d\n%s\n%s", client.id, client.name, messageText)
		client.room.broadcast([]byte(chatMsg))

		// --- THIS IS THE FIX ---
		// Check if this chat message is the "Start Game" command
		if messageText == "clicked 'Start Game'" {
			log.Printf("Game start triggered by client %d in room %d", client.id, client.room.id)
			room := client.room
			room.mu.Lock()
			room.status = "INGAME"
			room.mu.Unlock()

			// Also broadcast the game start message
			// "ROOM_NOW_STARTED\n1\n687999"
			seed := rand.Intn(1000000) // Generate a random seed
			startMsg := fmt.Sprintf("ROOM_NOW_STARTED\n%d\n%d", room.id, seed)
			room.broadcast([]byte(startMsg))
		}
		// --- END FIX ---

	} else if strings.HasPrefix(msgStr, "LEAVE ") { // THE FIX
		log.Printf("Received command 'LEAVE' from client %d", client.id)
		// Client gracefully leaves the room
		// Log shows "LEAVE 2", so we split by space
		parts := strings.Split(msgStr, " ")
		if len(parts) < 2 {
			return
		}
		roomID, err := strconv.Atoi(parts[1])
		if err != nil {
			return
		}

		// Check if client is actually in that room
		if client.room != nil && client.room.id == roomID {
			client.room.removeClient(client)
		} else {
			log.Printf("Client %d tried to LEAVE room %d but isn't in it.", client.id, roomID)
		}

	} else if strings.HasPrefix(msgStr, "AWAY\n") {
		log.Printf("Received command 'AWAY' from client %d", client.id)
		// Client sets AFK status
		parts := strings.SplitN(msgStr, "\n", 2) // Safer split
		if client.room == nil || len(parts) < 2 {
			return
		}
		client.status = parts[1]
		// Broadcast: "AWAY\n<PlayerID>\n<Status>"
		msg := fmt.Sprintf("AWAY\n%d\n%s", client.id, client.status)
		client.room.broadcast([]byte(msg))

	} else if strings.HasPrefix(msgStr, "UPDATE_CONTROL_NAMES\n") {
		log.Printf("Received command 'UPDATE_CONTROL_NAMES' from client %d", client.id)
		// Client updates P1-P4 names
		parts := strings.Split(msgStr, "\n")
		if client.room == nil || len(parts) < 5 {
			return
		}
		client.p1 = parts[1]
		client.p2 = parts[2]
		client.p3 = parts[3]
		client.p4 = parts[4]
		// Broadcast: "UPDATE_CONTROL_NAMES\n<PlayerID>\n<P1>\n<P2>\n<P3>\n<P4>"
		msg := fmt.Sprintf("UPDATE_CONTROL_NAMES\n%d\n%s\n%s\n%s\n%s",
			client.id, client.p1, client.p2, client.p3, client.p4)
		client.room.broadcast([]byte(msg))

	} else if strings.HasPrefix(msgStr, "UPDATE_ACHIEVEMENTS\n") {
		log.Printf("Received command 'UPDATE_ACHIEVEMENTS' from client %d", client.id)
		// Client updates achievements
		parts := strings.SplitN(msgStr, "\n", 2) // Safer split
		if client.room == nil || len(parts) < 2 {
			return
		}
		client.achievements = parts[1]
		// Broadcast: "UPDATE_ACHIEVEMENTS\n<PlayerID>\n<AchievementsString>"
		msg := fmt.Sprintf("UPDATE_ACHIEVEMENTS\n%d\n%s", client.id, client.achievements)
		client.room.broadcast([]byte(msg))

	} else if strings.HasPrefix(msgStr, "CHANGE_LATENCY\n") {
		log.Printf("Received command 'CHANGE_LATENCY' from client %d", client.id)
		// Client (host?) changes the game latency
		parts := strings.Split(msgStr, "\n")
		if client.room == nil || len(parts) < 2 {
			return
		}
		// This was the logic from the misplaced 'case' block
		newLatency, err := strconv.Atoi(parts[1])
		if err != nil {
			return // Invalid latency value
		}

		// Update the room's latency
		client.room.mu.Lock()
		client.room.latency = newLatency
		client.room.mu.Unlock()

		// Broadcast the new PLAYER_LIST with the updated latency
		// This matches the behavior in your log.
		log.Printf("Room %d latency changed to %d by client %d", client.room.id, newLatency, client.id)
		client.room.broadcastPlayerList()

	} else {
		log.Printf("Received unknown command from client %d: %s", client.id, msgStr)
	}
}

// --- Room Methods ---

// addClient adds a client to the room and broadcasts the new player list.
func (r *Room) addClient(client *Client) {
	r.mu.Lock()
	r.clients[client] = true
	client.room = r
	r.mu.Unlock()

	log.Printf("Client %d joined room %d (%s)", client.id, r.id, r.name)

	// Broadcast the new player list to everyone in the room.
	r.broadcastPlayerList()
}

// removeClient removes a client from the room and broadcasts the new list.
func (r *Room) removeClient(client *Client) {
	r.mu.Lock()
	left := false
	if _, ok := r.clients[client]; ok {
		delete(r.clients, client)
		client.room = nil
		left = true
	}
	r.mu.Unlock()

	if left {
		log.Printf("Client %d left room %d (%s)", client.id, r.id, r.name)
		// Broadcast the updated player list to remaining clients
		r.broadcastPlayerList()
	}
}

// broadcastPlayerList generates and sends the full PLAYER_LIST to all clients in the room.
func (r *Room) broadcastPlayerList() {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// "PLAYER_LIST\n1\n3\n¶\n2\ndoix..."
	// The '3' is latency, not maxPlayers.
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("PLAYER_LIST\n%d\n%d\n", r.id, r.latency))

	playerItems := []string{}
	for client := range r.clients {
		// PlayerInfo Format: "<PlayerID>\n<PlayerName>\n<P1>\n<P2>\n<P3>\n<P4>\n<AchievementsString>"
		playerStr := fmt.Sprintf("%d\n%s\n%s\n%s\n%s\n%s\n%s",
			client.id,
			client.name,
			client.p1,
			client.p2,
			client.p3,
			client.p4,
			client.achievements,
		)
		playerItems = append(playerItems, playerStr)
	}

	// --- FIX: Add the separator before the first player ---
	if len(playerItems) > 0 {
		sb.WriteString("¶\n") // This was the missing part
	}
	sb.WriteString(strings.Join(playerItems, "\n¶\n"))

	// Broadcast to all clients in this room
	r.broadcast([]byte(sb.String()))
}

// broadcast sends a message to all clients in the room.
func (r *Room) broadcast(message []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	log.Printf("Broadcasting to room %d: %s", r.id, string(message))

	for client := range r.clients {
		select {
		case client.send <- message:
		default:
			// Failed to send, assume client is dead.
			// Let the hub's unregister logic handle cleanup.
		}
	}
}

// --- Client Methods ---

// readPump pumps messages from the websocket connection to the hub.
func (c *Client) readPump() {
	defer func() {
		// When this function exits, unregister the client and close connection.
		c.hub.unregister <- c
		c.conn.Close()
	}()
	c.conn.SetReadLimit(maxMessageSize)
	_ = c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { _ = c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })

	for {
		// Read a message from the WebSocket
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("Client %d read error: %v", c.id, err)
			}
			break // Exit loop, triggering defer
		}

		// Pass the message to the hub for processing
		c.hub.broadcast <- &Message{data: message, client: c}
	}
}

// writePump pumps messages from the hub to the websocket connection.
func (c *Client) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		// When this function exits, stop the ticker and close connection.
		ticker.Stop()
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			// Set a write deadline
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				_ = c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Write the message
			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return // Exit loop, triggering defer
			}
			_, _ = w.Write(message)

			// Add queued chat messages to the current websocket message.
			// This is an optimization to batch messages.
			// ---
			// REMOVED: The client code shows it expects one command per WebSocket message.
			// This batching logic was incorrect.
			// ---
			// n := len(c.send)
			// for i := 0; i < n; i++ {
			// 	_ = w.Write([]byte{'\n'}) // Use newline as a separator if batching
			// 	_ = w.Write(<-c.send)
			// }

			if err := w.Close(); err != nil {
				return // Exit loop, triggering defer
			}

		case <-ticker.C:
			// Send a ping message
			_ = c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return // Exit loop, triggering defer
			}
		}
	}
}

// --- Main ---

// createDefaultConfig creates a default rooms.json if one doesn't exist.
func createDefaultConfig(filename string) {
	// Check if the file already exists
	if _, err := os.Stat(filename); err == nil {
		log.Printf("Config file %s already exists, loading.", filename)
		return
	} else if !os.IsNotExist(err) {
		log.Printf("Warning: Could not check for config file: %v", err)
		return // Proceed, ReadFile will probably fail
	}

	log.Printf("Config file %s not found, creating a default one.", filename)

	// Create default config based on original logs
	defaultRooms := []RoomConfig{}
	for i := 1; i <= 8; i++ {
		defaultRooms = append(defaultRooms, RoomConfig{
			ID:             i,
			Name:           fmt.Sprintf("Room %d", i),
			MaxPlayers:     8, // This is for the server-side ROOM_FULL check
			DefaultLatency: 3, // This is what's sent to the client
		})
	}

	// Marshal to JSON with indentation
	data, err := json.MarshalIndent(defaultRooms, "", "  ")
	if err != nil {
		log.Fatalf("Failed to marshal default config: %v", err)
	}

	// Write to file
	if err := os.WriteFile(filename, data, 0644); err != nil {
		log.Fatalf("Failed to write default config file: %v", err)
	}
}

// serveWs handles websocket requests from the peer.
func serveWs(hub *Hub, w http.ResponseWriter, r *http.Request) {
	// Upgrade the HTTP connection to a WebSocket
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("Upgrade error:", err)
		return
	}

	// Get a unique ID for the new client
	idMutex.Lock()
	id := nextClientID
	nextClientID++
	idMutex.Unlock()

	// Create the client object
	client := &Client{
		hub:  hub,
		conn: conn,
		send: make(chan []byte, 256), // Buffered channel
		id:   id,
		// other fields (name, room) will be set upon JOIN
	}

	// Register the client with the hub
	client.hub.register <- client

	// Start the read and write goroutines
	// These will run as long as the connection is alive.
	go client.writePump()
	go client.readPump() // This one blocks, so call it last in this func
}

func main() {
	// Seed the random number generator
	rand.Seed(time.Now().UnixNano()) // <-- FIX 2: Properly seed the rand package

	// Define config file path
	configFile := "rooms.json"

	// Create a default config file if it's missing
	createDefaultConfig(configFile)

	// Create and run the central hub
	hub := newHub(configFile)
	go hub.run()

	// This single, smart handler will replace our two old http.HandleFunc calls.
	// It checks if a request is for a WebSocket and handles it,
	// otherwise it serves the HTML file.
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Check if the request is for a WebSocket upgrade
		if websocket.IsWebSocketUpgrade(r) {
			serveWs(hub, w, r)
			return // The serveWs function will handle the rest
		}
		// Otherwise, it's a normal HTTP request, so serve the test client
		http.ServeFile(w, r, "index.html")
	})

	// We no longer need the specific "/ws" handler, as "/" handles it.
	// http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
	// 	serveWs(hub, w, r)
	// })

	port := ":8080"
	log.Printf("Server starting on http://localhost%s", port)
	err := http.ListenAndServe(port, nil)
	if err != nil {
		log.Fatal("ListenAndServe: ", err)
	}
}



