package main

import (
	"log"
	"net/http"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // 允许所有跨域请求（生产环境应限制）
	},
}

// Client 代表一个 WebSocket 连接
type Client struct {
	conn *websocket.Conn
	send chan []byte
}

// Hub 管理一个聊天室内的所有客户端
type Hub struct {
	clients    map[*Client]bool
	broadcast  chan BroadcastMessage // 修改通道类型
	register   chan *Client
	unregister chan *Client
	onEmpty    func()
}

type BroadcastMessage struct {
	message []byte
	sender  *Client
}

func (h *Hub) Run() {
	for {
		select {
		case client := <-h.register:
			h.clients[client] = true
		case client := <-h.unregister:
			if _, ok := h.clients[client]; ok {
				delete(h.clients, client)
				close(client.send)
			}
			if len(h.clients) == 0 && h.onEmpty != nil {
				h.onEmpty()
			}
		case bm := <-h.broadcast:
			for client := range h.clients {
				if client == bm.sender {
					continue
				}
				select {
				case client.send <- bm.message:
				default:
					close(client.send)
					delete(h.clients, client)
				}
			}
		}
	}
}

// RoomManager 管理所有聊天室
type RoomManager struct {
	rooms map[string]*Hub // 房间ID到Hub的映射
	mutex sync.Mutex      // 保证线程安全
}

func NewHub() *Hub {
	return &Hub{
		broadcast:  make(chan BroadcastMessage),
		register:   make(chan *Client),
		unregister: make(chan *Client),
		clients:    make(map[*Client]bool),
	}
}

func (c *Client) readPump(hub *Hub) {
	defer func() {
		hub.unregister <- c
		c.conn.Close()
	}()

	for {
		_, message, err := c.conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("读取错误: %v", err)
			}
			break
		}
		hub.broadcast <- BroadcastMessage{message: message, sender: c}
	}
}

func (c *Client) writePump() {
	defer func() {
		c.conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.send:
			if !ok {
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			if err := w.Close(); err != nil {
				return
			}
		}
	}
}

func NewRoomManager() *RoomManager {
	return &RoomManager{
		rooms: make(map[string]*Hub),
	}
}

func (rm *RoomManager) getOrCreateHub(roomID string) *Hub {
	rm.mutex.Lock()
	defer rm.mutex.Unlock()

	if hub, exists := rm.rooms[roomID]; exists {
		return hub
	}

	hub := NewHub()
	hub.onEmpty = func() {
		rm.mutex.Lock()
		delete(rm.rooms, roomID)
		rm.mutex.Unlock()
		log.Printf("房间 %s 已销毁", roomID)
	}

	rm.rooms[roomID] = hub
	go hub.Run()
	log.Printf("房间 %s 已创建", roomID)
	return hub
}

func extractRoomID(path string) string {
	parts := strings.Split(path, "/")
	if len(parts) >= 3 {
		return parts[2]
	}
	return ""
}

func serveWs(rm *RoomManager, w http.ResponseWriter, r *http.Request) {
	roomID := extractRoomID(r.URL.Path)
	if roomID == "" {
		http.Error(w, "Invalid room ID", http.StatusBadRequest)
		return
	}

	hub := rm.getOrCreateHub(roomID)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Println("WebSocket upgrade failed:", err)
		return
	}

	client := &Client{
		conn: conn,
		send: make(chan []byte, 256),
	}

	hub.register <- client

	go client.writePump()
	go client.readPump(hub)
}

func main() {
	roomManager := NewRoomManager()

	http.HandleFunc("/ws/", func(w http.ResponseWriter, r *http.Request) {
		serveWs(roomManager, w, r)
	})

	log.Println("Server starting on :8080")
	if err := http.ListenAndServe(":8080", nil); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
