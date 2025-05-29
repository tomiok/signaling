package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allow all origins for testing
	},
}

type Message struct {
	Data   any    `json:"data,omitempty"`
	Type   string `json:"type"`
	RoomID string `json:"room_id,omitempty"`
	PeerID string `json:"peer_id,omitempty"`
	Target string `json:"target,omitempty"`
}

type Client struct {
	ID     string
	RoomID string
	Conn   *websocket.Conn
	Send   chan []byte
}

type Room struct {
	ID      string
	Clients map[string]*Client
	mutex   sync.RWMutex
}

type Hub struct {
	rooms      map[string]*Room
	mutex      sync.RWMutex
	register   chan *Client
	unregister chan *Client
	broadcast  chan []byte
}

func newHub() *Hub {
	return &Hub{
		rooms:      make(map[string]*Room),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		broadcast:  make(chan []byte),
	}
}

func (h *Hub) run() {
	for {
		select {
		case client := <-h.register:
			h.addClientToRoom(client)

		case client := <-h.unregister:
			h.removeClientFromRoom(client)

		case message := <-h.broadcast:
			// Handle broadcast messages if needed
			fmt.Printf("📢 Broadcast: %s\n", message)
		}
	}
}

func (h *Hub) addClientToRoom(client *Client) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	room, exists := h.rooms[client.RoomID]
	log.Printf("Client joining. roomID: %s, clientID (peer): %s", client.RoomID, client.ID)

	if !exists {
		room = &Room{
			ID:      client.RoomID,
			Clients: make(map[string]*Client),
		}
		h.rooms[client.RoomID] = room
		fmt.Printf("🏠 Created new room: %s\n", client.RoomID)
	}

	room.mutex.Lock()
	room.Clients[client.ID] = client
	clientCount := len(room.Clients)
	room.mutex.Unlock()

	fmt.Printf("👤 Client %s joined room %s (total: %d)\n", client.ID, client.RoomID, clientCount)

	// Send joined confirmation to the new client with their peer ID
	joinedMsg := Message{
		Type:   "joined",
		PeerID: client.ID,
		RoomID: client.RoomID,
	}
	data, _ := json.Marshal(joinedMsg)
	select {
	case client.Send <- data:
	default:
		close(client.Send)
		room.mutex.Lock()
		delete(room.Clients, client.ID)
		room.mutex.Unlock()
		return
	}

	// Notify existing clients about new peer (but not the new client itself)
	h.notifyPeerJoined(room, client.ID)

	// Send existing peers to new client
	h.sendExistingPeers(client, room)
}

func (h *Hub) removeClientFromRoom(client *Client) {
	h.mutex.Lock()
	defer h.mutex.Unlock()

	room, exists := h.rooms[client.RoomID]
	if !exists {
		return
	}

	room.mutex.Lock()
	delete(room.Clients, client.ID)
	clientCount := len(room.Clients)
	room.mutex.Unlock()

	close(client.Send)

	fmt.Printf("👋 Client %s left room %s (remaining: %d)\n", client.ID, client.RoomID, clientCount)

	// Notify remaining clients
	h.notifyPeerLeft(room, client.ID)

	// Remove empty room
	if clientCount == 0 {
		delete(h.rooms, client.RoomID)
		fmt.Printf("🗑️ Removed empty room: %s\n", client.RoomID)
	}
}

func (h *Hub) notifyPeerJoined(room *Room, newPeerID string) {
	message := Message{
		Type:   "peer_joined",
		PeerID: newPeerID,
		RoomID: room.ID,
	}

	data, _ := json.Marshal(message)
	fmt.Printf("📣 Notifying existing clients about new peer: %s\n", newPeerID)

	room.mutex.RLock()
	for clientID, client := range room.Clients {
		// Don't notify the new peer about themselves
		if clientID != newPeerID {
			fmt.Printf("  └─ Sending peer_joined to: %s\n", clientID)
			select {
			case client.Send <- data:
			default:
				close(client.Send)
				delete(room.Clients, clientID)
			}
		}
	}
	room.mutex.RUnlock()
}

func (h *Hub) notifyPeerLeft(room *Room, leftPeerID string) {
	message := Message{
		Type:   "peer_left",
		PeerID: leftPeerID,
		RoomID: room.ID,
	}

	data, _ := json.Marshal(message)
	fmt.Printf("📣 Notifying clients about peer leaving: %s\n", leftPeerID)

	room.mutex.RLock()
	for clientID, client := range room.Clients {
		fmt.Printf("  └─ Sending peer_left to: %s\n", clientID)
		select {
		case client.Send <- data:
		default:
			close(client.Send)
		}
	}
	room.mutex.RUnlock()
}

func (h *Hub) sendExistingPeers(newClient *Client, room *Room) {
	room.mutex.RLock()
	existingPeers := make([]string, 0)
	for clientID := range room.Clients {
		if clientID != newClient.ID {
			existingPeers = append(existingPeers, clientID)
		}
	}
	room.mutex.RUnlock()

	fmt.Printf("📋 Sending existing peers to new client %s: %v\n", newClient.ID, existingPeers)

	// Send each existing peer as a separate peer_joined message
	for _, peerID := range existingPeers {
		message := Message{
			Type:   "peer_joined",
			PeerID: peerID,
			RoomID: room.ID,
		}
		data, _ := json.Marshal(message)
		select {
		case newClient.Send <- data:
			fmt.Printf("  └─ Sent existing peer %s to new client %s\n", peerID, newClient.ID)
		default:
			close(newClient.Send)
			room.mutex.Lock()
			delete(room.Clients, newClient.ID)
			room.mutex.Unlock()
			return
		}
	}
}

func (h *Hub) forwardMessage(msg Message, sender *Client) {
	h.mutex.RLock()
	room, exists := h.rooms[sender.RoomID]
	h.mutex.RUnlock()

	if !exists {
		fmt.Printf("❌ Room %s not found for message forwarding\n", sender.RoomID)
		return
	}

	// Set the sender's peer ID
	msg.PeerID = sender.ID

	data, _ := json.Marshal(msg)

	room.mutex.RLock()
	defer room.mutex.RUnlock()

	if msg.Target != "" {
		// Send to specific peer
		fmt.Printf("📤 Forwarding %s from %s to %s\n", msg.Type, sender.ID, msg.Target)
		if targetClient, exists := room.Clients[msg.Target]; exists {
			select {
			case targetClient.Send <- data:
				fmt.Printf("  ✅ Message delivered to %s\n", msg.Target)
			default:
				close(targetClient.Send)
				delete(room.Clients, msg.Target)
				fmt.Printf("  ❌ Failed to deliver to %s (client disconnected)\n", msg.Target)
			}
		} else {
			fmt.Printf("  ❌ Target client %s not found\n", msg.Target)
		}
	} else {
		// Broadcast to all except sender
		fmt.Printf("📢 Broadcasting %s from %s to all peers\n", msg.Type, sender.ID)
		for clientID, client := range room.Clients {
			if clientID != sender.ID {
				select {
				case client.Send <- data:
					fmt.Printf("  ✅ Broadcasted to %s\n", clientID)
				default:
					close(client.Send)
					delete(room.Clients, clientID)
					fmt.Printf("  ❌ Failed to broadcast to %s (client disconnected)\n", clientID)
				}
			}
		}
	}
}

// Generate a unique peer ID
func generatePeerID() string {
	bytes := make([]byte, 8)
	rand.Read(bytes)
	return fmt.Sprintf("peer_%x", bytes)
}

var hub = newHub()

func handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// Extract room ID from URL: /room/123
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) < 3 || parts[1] != "room" {
		http.Error(w, "Invalid room URL", http.StatusBadRequest)
		return
	}
	roomID := parts[2]

	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Failed to upgrade: %v", err)
		return
	}

	// Generate unique client ID using random bytes
	clientID := generatePeerID()

	client := &Client{
		ID:     clientID,
		RoomID: roomID,
		Conn:   conn,
		Send:   make(chan []byte, 256),
	}

	fmt.Printf("🔗 New WebSocket connection: %s joining room %s\n", clientID, roomID)

	hub.register <- client

	go client.writePump()
	go client.readPump(hub)
}

func (c *Client) readPump(hub *Hub) {
	defer func() {
		fmt.Printf("🔌 Client %s disconnecting from room %s\n", c.ID, c.RoomID)
		hub.unregister <- c
		c.Conn.Close()
	}()

	for {
		_, messageData, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				fmt.Printf("❌ Unexpected close error for client %s: %v\n", c.ID, err)
			} else {
				fmt.Printf("🔌 Client %s disconnected normally\n", c.ID)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(messageData, &msg); err != nil {
			fmt.Printf("❌ JSON unmarshal error from %s: %v\n", c.ID, err)
			continue
		}

		// The peer ID will be set in forwardMessage
		fmt.Printf("📨 Received from %s: %s (target: %s)\n", c.ID, msg.Type, msg.Target)

		// Forward message to appropriate peers
		hub.forwardMessage(msg, c)
	}
}

func (c *Client) writePump() {
	defer c.Conn.Close()

	for {
		select {
		case message, ok := <-c.Send:
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			// Parse message for logging
			var msg Message
			if err := json.Unmarshal(message, &msg); err == nil {
				fmt.Printf("📤 Sending to %s: %s\n", c.ID, msg.Type)
			}

			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				fmt.Printf("❌ Write error for client %s: %v\n", c.ID, err)
				return
			}
		}
	}
}

func main() {
	go hub.run()

	http.HandleFunc("/room/", handleWebSocket)

	// Health check endpoint
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		hub.mutex.RLock()
		roomCount := len(hub.rooms)
		totalClients := 0
		for _, room := range hub.rooms {
			room.mutex.RLock()
			totalClients += len(room.Clients)
			room.mutex.RUnlock()
		}
		hub.mutex.RUnlock()

		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "Signaling server OK. Active rooms: %d, Total clients: %d", roomCount, totalClients)
	})

	// Debug endpoint to see current rooms
	http.HandleFunc("/debug", func(w http.ResponseWriter, r *http.Request) {
		hub.mutex.RLock()
		defer hub.mutex.RUnlock()

		w.Header().Set("Content-Type", "application/json")
		type RoomInfo struct {
			ID      string   `json:"id"`
			Clients []string `json:"clients"`
		}

		var rooms []RoomInfo
		for roomID, room := range hub.rooms {
			room.mutex.RLock()
			clients := make([]string, 0, len(room.Clients))
			for clientID := range room.Clients {
				clients = append(clients, clientID)
			}
			room.mutex.RUnlock()

			rooms = append(rooms, RoomInfo{
				ID:      roomID,
				Clients: clients,
			})
		}

		json.NewEncoder(w).Encode(map[string]interface{}{
			"rooms": rooms,
		})
	})

	fmt.Println("🚀 P2P Signaling Server running on http://localhost:8081")
	fmt.Println("📊 Health check: http://localhost:8081/health")
	fmt.Println("🐛 Debug info: http://localhost:8081/debug")
	log.Fatal(http.ListenAndServe(":8081", nil))
}
