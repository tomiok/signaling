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
	Data    any      `json:"data,omitempty"`
	Type    string   `json:"type"`
	RoomID  string   `json:"room_id,omitempty"`
	PeerID  string   `json:"peer_id,omitempty"`
	Target  string   `json:"target,omitempty"`
	Targets []string `json:"targets,omitempty"` // Múltiples targets
}

type Client struct {
	ID      string
	RoomID  string
	Name    string // NUEVO: Almacenar nombre del usuario
	Conn    *websocket.Conn
	Send    chan []byte
	HasName bool // NUEVO: Track si el cliente ya estableció su nombre
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

	// Send joined confirmation to the new client with their peer ID and participant count
	joinedMsg := Message{
		Type:   "joined",
		PeerID: client.ID,
		RoomID: client.RoomID,
		Data: map[string]interface{}{
			"participant_count": clientCount,
		},
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

	// NO notificar inmediatamente - esperar a que el cliente establezca su nombre
	fmt.Printf("🕒 Cliente %s agregado, esperando nombre antes de notificar a otros\n", client.ID)
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

// NUEVO: Manejar cuando un cliente establece su nombre
func (h *Hub) handleSetUserName(msg Message, sender *Client) {
	fmt.Printf("📛 Cliente %s estableciendo nombre\n", sender.ID)

	// Extraer nombre del mensaje
	nameData, ok := msg.Data.(map[string]interface{})
	if !ok {
		fmt.Printf("❌ Datos de nombre inválidos de %s\n", sender.ID)
		return
	}

	name, ok := nameData["name"].(string)
	if !ok || name == "" {
		fmt.Printf("❌ Nombre vacío o inválido de %s\n", sender.ID)
		return
	}

	// Establecer nombre en el cliente
	sender.Name = name
	sender.HasName = true

	fmt.Printf("✅ Cliente %s estableció nombre: '%s'\n", sender.ID, name)

	// Obtener la sala
	h.mutex.RLock()
	room, exists := h.rooms[sender.RoomID]
	h.mutex.RUnlock()

	if !exists {
		fmt.Printf("❌ Sala %s no encontrada\n", sender.RoomID)
		return
	}

	// Notificar a todos los clientes existentes (con nombre establecido) sobre el nuevo peer
	h.notifyPeerJoinedWithName(room, sender.ID)

	// Enviar peers existientes al nuevo cliente
	h.sendExistingPeersWithNames(sender, room)

	// Broadcast del nombre establecido a todos los demás
	h.broadcastUserNameSet(room, sender.ID, name)
}

// NUEVO: Notificar peer_joined solo a clientes que ya tienen nombre
func (h *Hub) notifyPeerJoinedWithName(room *Room, newPeerID string) {
	room.mutex.RLock()
	participantCount := len(room.Clients)
	room.mutex.RUnlock()

	message := Message{
		Type:   "peer_joined",
		PeerID: newPeerID,
		RoomID: room.ID,
		Data: map[string]interface{}{
			"participant_count": participantCount,
		},
	}

	data, _ := json.Marshal(message)
	fmt.Printf("📣 Notificando clientes existentes sobre nuevo peer: %s (total: %d)\n", newPeerID, participantCount)

	room.mutex.RLock()
	for clientID, client := range room.Clients {
		// Solo notificar a clientes que ya tienen nombre establecido y no son el nuevo peer
		if clientID != newPeerID && client.HasName {
			fmt.Printf("  └─ Enviando peer_joined a: %s\n", clientID)
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

// NUEVO: Broadcast cuando un usuario establece su nombre
func (h *Hub) broadcastUserNameSet(room *Room, peerId, name string) {
	message := Message{
		Type: "user_name_set",
		Data: map[string]interface{}{
			"peerId": peerId,
			"name":   name,
		},
	}

	data, _ := json.Marshal(message)
	fmt.Printf("📛 Broadcasting nombre establecido: %s -> %s\n", peerId, name)

	room.mutex.RLock()
	for clientID, client := range room.Clients {
		if clientID != peerId && client.HasName {
			select {
			case client.Send <- data:
				fmt.Printf("  └─ Enviado user_name_set a: %s\n", clientID)
			default:
				close(client.Send)
				delete(room.Clients, clientID)
			}
		}
	}
	room.mutex.RUnlock()
}

func (h *Hub) notifyPeerLeft(room *Room, leftPeerID string) {
	room.mutex.RLock()
	participantCount := len(room.Clients)
	room.mutex.RUnlock()

	message := Message{
		Type:   "peer_left",
		PeerID: leftPeerID,
		RoomID: room.ID,
		Data: map[string]interface{}{
			"participant_count": participantCount,
		},
	}

	data, _ := json.Marshal(message)
	fmt.Printf("📣 Notifying clients about peer leaving: %s (remaining: %d)\n", leftPeerID, participantCount)

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

// MODIFICADO: Solo enviar peers que ya tienen nombres establecidos
func (h *Hub) sendExistingPeersWithNames(newClient *Client, room *Room) {
	room.mutex.RLock()
	existingPeers := make([]string, 0)
	participantCount := len(room.Clients)
	for clientID, client := range room.Clients {
		// Solo incluir peers que ya tienen nombre establecido
		if clientID != newClient.ID && client.HasName {
			existingPeers = append(existingPeers, clientID)
		}
	}
	room.mutex.RUnlock()

	fmt.Printf("📋 Enviando peers existentes (con nombres) a %s: %v (total: %d)\n", newClient.ID, existingPeers, participantCount)

	// Send each existing peer as a separate peer_joined message
	for _, peerID := range existingPeers {
		message := Message{
			Type:   "peer_joined",
			PeerID: peerID,
			RoomID: room.ID,
			Data: map[string]interface{}{
				"participant_count": participantCount,
			},
		}
		data, _ := json.Marshal(message)
		select {
		case newClient.Send <- data:
			fmt.Printf("  └─ Enviado peer existente %s a nuevo cliente %s\n", peerID, newClient.ID)
		default:
			close(newClient.Send)
			room.mutex.Lock()
			delete(room.Clients, newClient.ID)
			room.mutex.Unlock()
			return
		}
	}
}

func (h *Hub) handleConnectionFailed(msg Message, sender *Client) {
	fmt.Printf("🚨 Connection failed reported by %s for peer %s\n", sender.ID, msg.Target)

	h.mutex.RLock()
	room, exists := h.rooms[sender.RoomID]
	h.mutex.RUnlock()

	if !exists {
		fmt.Printf("❌ Room %s not found for connection failure report\n", sender.RoomID)
		return
	}

	room.mutex.Lock()
	// Check if the target peer actually exists and remove them if they do
	if targetClient, exists := room.Clients[msg.Target]; exists {
		fmt.Printf("🗑️ Removing failed peer %s from room %s\n", msg.Target, sender.RoomID)

		// Close the failed client's connection
		close(targetClient.Send)
		delete(room.Clients, msg.Target)

		participantCount := len(room.Clients)
		room.mutex.Unlock()

		// Notify all remaining clients about the peer removal
		h.notifyPeerLeft(room, msg.Target)

		fmt.Printf("📊 Room %s now has %d participants after removing failed peer\n", sender.RoomID, participantCount)
	} else {
		room.mutex.Unlock()
		fmt.Printf("⚠️ Failed peer %s was not found in room %s\n", msg.Target, sender.RoomID)
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

	// Handle multiple specific targets
	if len(msg.Targets) > 0 {
		fmt.Printf("📤 Forwarding %s from %s to multiple targets: %v\n", msg.Type, sender.ID, msg.Targets)
		sentCount := 0
		for _, targetID := range msg.Targets {
			if targetClient, exists := room.Clients[targetID]; exists {
				select {
				case targetClient.Send <- data:
					sentCount++
					fmt.Printf("  ✅ Delivered to %s\n", targetID)
				default:
					close(targetClient.Send)
					delete(room.Clients, targetID)
					fmt.Printf("  ❌ Failed to deliver to %s (client disconnected)\n", targetID)
				}
			} else {
				fmt.Printf("  ❌ Target client %s not found\n", targetID)
			}
		}
		fmt.Printf("  📊 Successfully delivered to %d/%d targets\n", sentCount, len(msg.Targets))
		return
	}

	// Handle single specific target (existing behavior)
	if msg.Target != "" {
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
		return
	}

	// No target specified = broadcast to all except sender
	fmt.Printf("📢 Broadcasting %s from %s to all peers\n", msg.Type, sender.ID)
	sentCount := 0
	totalClients := len(room.Clients) - 1 // Exclude sender

	for clientID, client := range room.Clients {
		if clientID != sender.ID {
			select {
			case client.Send <- data:
				sentCount++
				fmt.Printf("  ✅ Broadcasted to %s\n", clientID)
			default:
				close(client.Send)
				delete(room.Clients, clientID)
				fmt.Printf("  ❌ Failed to broadcast to %s (client disconnected)\n", clientID)
			}
		}
	}
	fmt.Printf("  📊 Successfully broadcasted to %d/%d clients\n", sentCount, totalClients)
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
		ID:      clientID,
		RoomID:  roomID,
		Name:    "", // Inicialmente vacío
		Conn:    conn,
		Send:    make(chan []byte, 256),
		HasName: false, // Inicialmente false
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

		fmt.Printf("📨 Received from %s: %s (target: %s)\n", c.ID, msg.Type, msg.Target)

		// NUEVO: Manejar establecimiento de nombre
		if msg.Type == "set_user_name" {
			hub.handleSetUserName(msg, c)
			continue
		}

		// NUEVO: Manejar fallo de conexión
		if msg.Type == "connection_failed" {
			hub.handleConnectionFailed(msg, c)
			continue
		}

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
		type ClientInfo struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			HasName bool   `json:"has_name"`
		}
		type RoomInfo struct {
			ID      string       `json:"id"`
			Clients []ClientInfo `json:"clients"`
		}

		var rooms []RoomInfo
		for roomID, room := range hub.rooms {
			room.mutex.RLock()
			clients := make([]ClientInfo, 0, len(room.Clients))
			for clientID, client := range room.Clients {
				clients = append(clients, ClientInfo{
					ID:      clientID,
					Name:    client.Name,
					HasName: client.HasName,
				})
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
