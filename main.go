package main

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"nhooyr.io/websocket"
)

type Candidate struct {
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *int    `json:"sdpMLineIndex,omitempty"`
	Candidate     *string `json:"candidate,omitempty"`
}

type Envelope struct {
	Type      string     `json:"type"`
	Room      string     `json:"room,omitempty"`
	PeerID    string     `json:"peerId,omitempty"`
	From      string     `json:"from,omitempty"`
	SDP       string     `json:"sdp,omitempty"`
	Candidate *Candidate `json:"candidate,omitempty"`
	Payload   string     `json:"payload,omitempty"`
}

type Client struct {
	id   string
	room *Room
	conn *websocket.Conn
	send chan []byte
	ctx  context.Context
}

type Room struct {
	id      string
	clients map[string]*Client
	mu      sync.RWMutex
}

func (r *Room) add(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[c.id] = c
}

func (r *Room) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[id]; ok {
		delete(r.clients, id)
		log.Printf("Client %s removed from room %s", id, r.id)
	}
}

func (r *Room) broadcast(from string, payload []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, c := range r.clients {
		if id == from {
			continue
		}

		if c.ctx.Err() != nil {
			continue
		}

		select {
		case c.send <- payload:
		default:
			log.Printf("Slow consumer detected for client %s. Closing connection.", c.id)
			go c.conn.Close(websocket.StatusPolicyViolation, "slow consumer")
		}
	}
}

type Hub struct {
	rooms map[string]*Room
	mu    sync.RWMutex
}

func NewHub() *Hub { return &Hub{rooms: make(map[string]*Room)} }

func (h *Hub) getOrCreate(roomID string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomID]
	if !ok {
		r = &Room{id: roomID, clients: make(map[string]*Client)}
		h.rooms[roomID] = r
		log.Printf("Room '%s' created", roomID)
	}
	return r
}

func (h *Hub) maybeDelete(room *Room) {
	if room == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	room.mu.RLock()
	isEmpty := len(room.clients) == 0
	room.mu.RUnlock()

	if isEmpty {
		delete(h.rooms, room.id)
		log.Printf("Room '%s' deleted as it is empty", room.id)
	}
}

var hub = NewHub()

const (
	writeTimeout   = 10 * time.Second
	pingInterval   = 30 * time.Second
	readLimitBytes = 1 << 20 // 1 MiB
)

func wsHandler(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	peerID := r.URL.Query().Get("peer")

	if roomID == "" || peerID == "" {
		http.Error(w, "Query parameters 'room' and 'peer' are required", http.StatusBadRequest)
		return
	}

	isDevMode := getenv("WS_DEV_MODE", "") != ""
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		InsecureSkipVerify: isDevMode,
		OriginPatterns:     []string{"webrtc.mediarise.org"},
	})
	if err != nil {
		log.Printf("WebSocket upgrade failed for peer %s: %v", peerID, err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "handler finished")
	conn.SetReadLimit(readLimitBytes)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	room := hub.getOrCreate(roomID)
	client := &Client{
		id:   peerID,
		room: room,
		conn: conn,
		send: make(chan []byte, 256),
		ctx:  ctx,
	}
	room.add(client)
	log.Printf("Client '%s' connected to room '%s'", client.id, room.id)

	defer func() {
		room.remove(client.id)
		hub.maybeDelete(room)
		leavePayload, _ := json.Marshal(&Envelope{Type: "peer-left", PeerID: client.id})
		room.broadcast(client.id, leavePayload)
		log.Printf("Cleanup for client '%s' finished", client.id)
	}()

	existingPeers := make([]string, 0, len(room.clients))
	room.mu.RLock()
	for id := range room.clients {
		if id != client.id {
			existingPeers = append(existingPeers, id)
		}
	}
	room.mu.RUnlock()

	for _, peer := range existingPeers {
		payload, _ := json.Marshal(&Envelope{Type: "peer-joined", PeerID: peer})
		client.send <- payload
	}

	notifyPayload, _ := json.Marshal(&Envelope{Type: "peer-joined", PeerID: client.id})
	room.broadcast(client.id, notifyPayload)

	go writer(client)

	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Printf("Client %s context cancelled.", client.id)
			} else if websocket.CloseStatus(err) != -1 {
				log.Printf("Connection closed by client %s: %v", client.id, err)
			} else {
				log.Printf("Read error from client %s: %v", client.id, err)
			}
			break
		}

		if msgType != websocket.MessageText {
			continue
		}

		var envelope Envelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			log.Printf("Invalid JSON from %s: %v", client.id, err)
			continue
		}

		envelope.From = client.id
		envelope.Room = room.id
		log.Printf("Received '%s' from '%s', broadcasting to room '%s'", envelope.Type, client.id, room.id)

		payload, err := json.Marshal(envelope)
		if err != nil {
			log.Printf("JSON marshal error: %v", err)
			continue
		}

		switch envelope.Type {
		case "offer", "answer", "ice":
			room.broadcast(client.id, payload)
		default:
			log.Printf("Unknown message type '%s' from client '%s'", envelope.Type, client.id)
		}
	}
}

func writer(c *Client) {
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				c.conn.Close(websocket.StatusNormalClosure, "send channel closed")
				return
			}
			wctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				log.Printf("Write error to %s: %v", c.id, err)
				return
			}
		case <-pingTicker.C:
			pctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				log.Printf("Ping failed for %s: %v", c.id, err)
				return
			}
		}
	}
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func rootHandler(w http.ResponseWriter, _ *http.Request) {
	tmpl, err := template.ParseFiles("index.html")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	err = tmpl.Execute(w, nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}

}

func main() {
	addr := getenv("ADDR", ":8777")
	log.Printf("Starting server on %s", addr)

	fs := http.FileServer(http.Dir("static"))

	mux := http.NewServeMux()
	mux.HandleFunc("/", rootHandler)
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/healthz", healthz)
	mux.Handle("/static/", http.StripPrefix("/static/", fs))

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server ListenAndServe error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server Shutdown error: %v", err)
	} else {
		log.Println("Server gracefully stopped")
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
