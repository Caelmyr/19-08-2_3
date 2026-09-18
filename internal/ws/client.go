package ws

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 65536
)

// Upgrader WebSocket升级配置
var Upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin:     func(r *http.Request) bool { return true },
}

// NewClient 创建新的客户端连接
func NewClient(hub *Hub, conn *websocket.Conn, roomID, username string) *Client {
	return &Client{
		Hub:      hub,
		RoomID:   roomID,
		ClientID: uuid.New().String(),
		Username: username,
		Color:    randomColor(),
		Position: 0,
		Conn:     conn,
		Send:     make(chan []byte, 256),
	}
}

// ReadPump 从WebSocket读取消息并发送到hub
func (c *Client) ReadPump(opHandler func(*Client, Message)) {
	defer func() {
		c.Hub.Unregister <- c
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				log.Printf("[WS] Read error from %s: %v", c.ClientID, err)
			}
			break
		}

		var msg Message
		if err := json.Unmarshal(message, &msg); err != nil {
			log.Printf("[WS] Unmarshal error: %v", err)
			continue
		}

		msg.ClientID = c.ClientID

		switch msg.Type {
		case MsgOp, MsgCursorMove, MsgSnapshot, MsgFollow, MsgUnfollow, MsgViewport:
			// 交给handler处理
			if opHandler != nil {
				opHandler(c, msg)
			}
		default:
			log.Printf("[WS] Unknown message type: %s", msg.Type)
		}
	}
}

// WritePump 向WebSocket推送消息
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case message, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// hub closed channel
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			w, err := c.Conn.NextWriter(websocket.TextMessage)
			if err != nil {
				return
			}
			w.Write(message)

			// 批量发送待处理消息
			n := len(c.Send)
			for i := 0; i < n; i++ {
				w.Write([]byte{'\n'})
				w.Write(<-c.Send)
			}

			if err := w.Close(); err != nil {
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// SendMessage 发送消息到客户端
func (c *Client) SendMessage(msg Message) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case c.Send <- data:
		return nil
	default:
		return ErrSendBufferFull
	}
}
