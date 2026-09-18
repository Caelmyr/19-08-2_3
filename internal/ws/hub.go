// Package ws 实现WebSocket Hub和房间管理
// 负责连接管理、操作广播、在线用户光标同步
package ws

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// MessageType 消息类型
type MessageType string

const (
	MsgOp         MessageType = "op"          // 操作消息
	MsgAck        MessageType = "ack"         // 操作确认
	MsgCursors    MessageType = "cursors"     // 光标位置广播
	MsgCursorMove MessageType = "cursor_move" // 光标移动
	MsgUserJoin   MessageType = "user_join"   // 用户加入
	MsgUserLeave  MessageType = "user_leave"  // 用户离开
	MsgInit       MessageType = "init"        // 初始化消息
	MsgError      MessageType = "error"       // 错误消息
	MsgSnapshot   MessageType = "snapshot"    // 快照请求
	MsgFollow     MessageType = "follow"      // 请求跟随某用户
	MsgUnfollow   MessageType = "unfollow"    // 取消跟随
	MsgViewport   MessageType = "viewport"    // 视野（滚动位置+光标）同步
	MsgFollowAck  MessageType = "follow_ack"  // 跟随确认（附被跟随者当前视野）
	MsgFollowEnd  MessageType = "follow_end"  // 被跟随者离开，跟随结束
	MsgFollowInfo MessageType = "follow_info" // 通知被跟随者其跟随人数变化
)

// Message WebSocket消息
type Message struct {
	Type       MessageType `json:"type"`
	DocID      string      `json:"doc_id,omitempty"`
	ClientID   string      `json:"client_id,omitempty"`
	Username   string      `json:"username,omitempty"`
	Version    int64       `json:"version,omitempty"`
	BaseVer    int64       `json:"base_version,omitempty"`
	Op         interface{} `json:"op,omitempty"`
	Content    string      `json:"content,omitempty"`
	Users      []UserInfo  `json:"users,omitempty"`
	Cursors    []Cursor    `json:"cursors,omitempty"`
	Position   int         `json:"position,omitempty"`
	Color      string      `json:"color,omitempty"`
	Error      string      `json:"error,omitempty"`
	TargetID   string      `json:"target_id,omitempty"`   // 跟随目标（被跟随者）的client_id
	ScrollTop  float64     `json:"scroll_top,omitempty"`  // 视野滚动位置
	ScrollLeft float64     `json:"scroll_left,omitempty"` // 视野横向滚动位置
	Followers  int         `json:"followers,omitempty"`   // 当前跟随人数
	Reason     string      `json:"reason,omitempty"`      // 跟随结束原因
	Timestamp  time.Time   `json:"timestamp,omitempty"`
}

// UserInfo 用户信息
type UserInfo struct {
	ClientID string `json:"client_id"`
	Username string `json:"username"`
	Color    string `json:"color"`
}

// Cursor 光标位置
type Cursor struct {
	ClientID string `json:"client_id"`
	Username string `json:"username"`
	Position int    `json:"position"`
	Color    string `json:"color"`
}

// Client 表示一个WebSocket连接
type Client struct {
	Hub        *Hub
	RoomID     string
	ClientID   string
	Username   string
	Color      string
	Position   int
	ScrollTop  float64 // 最近一次上报的视野滚动位置
	ScrollLeft float64
	Conn       *websocket.Conn
	Send       chan []byte
	mu         sync.Mutex
}

// Room 表示一个文档房间
type Room struct {
	ID      string
	Clients map[string]*Client
	Follows map[string]string // 跟随关系：followerClientID -> targetClientID
	mu      sync.RWMutex
}

// Hub 管理所有房间
type Hub struct {
	Rooms      map[string]*Room
	mu         sync.RWMutex
	Register   chan *Client
	Unregister chan *Client
	Broadcast  chan *BroadcastMessage
}

// BroadcastMessage 广播消息
type BroadcastMessage struct {
	RoomID  string
	Message []byte
	Except  string // 排除的client_id（发送者）
}

// 预定义颜色列表
var colors = []string{
	"#e74c3c", "#3498db", "#2ecc71", "#f39c12", "#9b59b6",
	"#1abc9c", "#e67e22", "#2980b9", "#27ae60", "#c0392b",
	"#8e44ad", "#d35400", "#16a085", "#2c3e50", "#e84393",
}

// randomColor 随机选颜色
func randomColor() string {
	return colors[int(uuid.New().ID())%len(colors)]
}

// NewHub 创建Hub
func NewHub() *Hub {
	return &Hub{
		Rooms:      make(map[string]*Room),
		Register:   make(chan *Client),
		Unregister: make(chan *Client),
		Broadcast:  make(chan *BroadcastMessage, 256),
	}
}

// Run 启动Hub主循环
func (h *Hub) Run() {
	for {
		select {
		case client := <-h.Register:
			h.addClient(client)
		case client := <-h.Unregister:
			h.removeClient(client)
		case msg := <-h.Broadcast:
			h.broadcast(msg)
		}
	}
}

// addClient 添加客户端到房间
func (h *Hub) addClient(client *Client) {
	h.mu.Lock()
	room, exists := h.Rooms[client.RoomID]
	if !exists {
		room = &Room{ID: client.RoomID, Clients: make(map[string]*Client), Follows: make(map[string]string)}
		h.Rooms[client.RoomID] = room
	}
	h.mu.Unlock()

	room.mu.Lock()
	room.Clients[client.ClientID] = client
	room.mu.Unlock()

	log.Printf("[Hub] Client %s joined room %s (total: %d)", client.ClientID, client.RoomID, len(room.Clients))
}

// removeClient 从房间移除客户端
func (h *Hub) removeClient(client *Client) {
	h.mu.RLock()
	room, exists := h.Rooms[client.RoomID]
	h.mu.RUnlock()
	if !exists {
		return
	}

	// 跟随关系清理（在锁内收集，锁外发送，避免持锁阻塞）
	var followEndMsgs []*Client     // 需要收到"被跟随者已离开"通知的跟随者
	var followInfoTargets []*Client // 跟随人数发生变化、需要被通知的被跟随者

	room.mu.Lock()
	if _, ok := room.Clients[client.ClientID]; ok {
		delete(room.Clients, client.ClientID)
		close(client.Send)
	}
	// 1) 离开者若是被跟随者：通知所有跟随者跟随结束
	for followerID, targetID := range room.Follows {
		if targetID == client.ClientID {
			delete(room.Follows, followerID)
			if follower, ok := room.Clients[followerID]; ok {
				followEndMsgs = append(followEndMsgs, follower)
			}
		}
	}
	// 2) 离开者若是跟随者：解除其跟随关系，并通知对方更新跟随人数
	if targetID, ok := room.Follows[client.ClientID]; ok {
		delete(room.Follows, client.ClientID)
		if target, ok := room.Clients[targetID]; ok {
			followInfoTargets = append(followInfoTargets, target)
		}
	}
	isEmpty := len(room.Clients) == 0
	room.mu.Unlock()

	// 先推送跟随结束通知，保证先于 user_leave 到达客户端
	for _, follower := range followEndMsgs {
		follower.SendMessage(Message{
			Type:     MsgFollowEnd,
			DocID:    room.ID,
			TargetID: client.ClientID,
			Username: client.Username,
			Reason:   "disconnect",
		})
	}
	for _, target := range followInfoTargets {
		target.SendMessage(Message{
			Type:      MsgFollowInfo,
			DocID:     room.ID,
			ClientID:  target.ClientID,
			Followers: h.FollowerCount(room.ID, target.ClientID),
		})
	}

	// 如果房间为空，删除
	if isEmpty {
		h.mu.Lock()
		delete(h.Rooms, client.RoomID)
		h.mu.Unlock()
		log.Printf("[Hub] Room %s empty, removed", client.RoomID)
	} else {
		// 通知其他用户
		h.broadcastUserLeave(room, client)
		log.Printf("[Hub] Client %s left room %s (remaining: %d)", client.ClientID, client.RoomID, len(room.Clients))
	}
}

// broadcast 发送消息到房间内所有客户端
func (h *Hub) broadcast(msg *BroadcastMessage) {
	h.mu.RLock()
	room, exists := h.Rooms[msg.RoomID]
	h.mu.RUnlock()
	if !exists {
		return
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	for _, client := range room.Clients {
		if client.ClientID == msg.Except {
			continue
		}
		select {
		case client.Send <- msg.Message:
		default:
			log.Printf("[Hub] Client %s send buffer full, dropping message", client.ClientID)
		}
	}
}

// broadcastUserLeave 广播用户离开事件
func (h *Hub) broadcastUserLeave(room *Room, leaving *Client) {
	msg := Message{
		Type:     MsgUserLeave,
		DocID:    room.ID,
		ClientID: leaving.ClientID,
		Username: leaving.Username,
	}
	data, _ := json.Marshal(msg)

	room.mu.RLock()
	defer room.mu.RUnlock()

	for _, client := range room.Clients {
		select {
		case client.Send <- data:
		default:
		}
	}
}

// BroadcastOp 广播操作消息
func (h *Hub) BroadcastOp(roomID string, senderID string, opMsg Message) {
	data, _ := json.Marshal(opMsg)
	h.Broadcast <- &BroadcastMessage{
		RoomID:  roomID,
		Message: data,
		Except:  senderID,
	}
}

// BroadcastCursors 广播光标位置
func (h *Hub) BroadcastCursors(roomID string, senderID string, cursorMsg Message) {
	data, _ := json.Marshal(cursorMsg)
	h.Broadcast <- &BroadcastMessage{
		RoomID:  roomID,
		Message: data,
		Except:  senderID,
	}
}

// SetFollow 建立跟随关系，返回被跟随者、原被跟随者ID（换跟随时）及当前跟随人数
func (h *Hub) SetFollow(roomID, followerID, targetID string) (target *Client, oldTargetID string, followers int, err error) {
	if followerID == targetID {
		return nil, "", 0, ErrCannotFollowSelf
	}

	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return nil, "", 0, ErrDocNotFound
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	target, ok := room.Clients[targetID]
	if !ok {
		return nil, "", 0, ErrFollowTargetNotFound
	}

	// 换跟随目标时先解除旧关系，由调用方通知旧目标更新人数
	oldTargetID = room.Follows[followerID]
	room.Follows[followerID] = targetID

	followers = 0
	for _, t := range room.Follows {
		if t == targetID {
			followers++
		}
	}
	return target, oldTargetID, followers, nil
}

// RemoveFollow 解除跟随关系，返回原被跟随者ID和是否之前有跟随
func (h *Hub) RemoveFollow(roomID, followerID string) (targetID string, ok bool) {
	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return "", false
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	targetID, ok = room.Follows[followerID]
	if ok {
		delete(room.Follows, followerID)
	}
	return targetID, ok
}

// FollowerCount 返回某客户端当前的跟随者数量
func (h *Hub) FollowerCount(roomID, targetID string) int {
	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return 0
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	count := 0
	for _, t := range room.Follows {
		if t == targetID {
			count++
		}
	}
	return count
}

// BroadcastToFollowers 只把消息发给 targetID 的跟随者
// 多人跟随同一人时收到的是同一份数据，保证大家看到同样的视野
func (h *Hub) BroadcastToFollowers(roomID, targetID string, msg Message) {
	data, _ := json.Marshal(msg)

	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	for followerID, t := range room.Follows {
		if t != targetID {
			continue
		}
		follower, ok := room.Clients[followerID]
		if !ok {
			continue
		}
		select {
		case follower.Send <- data:
		default:
			log.Printf("[Hub] Follower %s send buffer full, dropping viewport", followerID)
		}
	}
}

// SendToClient 向房间内指定客户端发送消息
func (h *Hub) SendToClient(roomID, clientID string, msg Message) {
	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return
	}

	room.mu.RLock()
	client, ok := room.Clients[clientID]
	room.mu.RUnlock()
	if !ok {
		return
	}
	client.SendMessage(msg)
}

// GetRoomUsers 获取房间内所有用户信息
func (h *Hub) GetRoomUsers(roomID string) []UserInfo {
	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return nil
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	users := make([]UserInfo, 0, len(room.Clients))
	for _, c := range room.Clients {
		users = append(users, UserInfo{
			ClientID: c.ClientID,
			Username: c.Username,
			Color:    c.Color,
		})
	}
	return users
}

// GetRoomCursors 获取房间内所有光标位置
func (h *Hub) GetRoomCursors(roomID string) []Cursor {
	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return nil
	}

	room.mu.RLock()
	defer room.mu.RUnlock()

	cursors := make([]Cursor, 0, len(room.Clients))
	for _, c := range room.Clients {
		cursors = append(cursors, Cursor{
			ClientID: c.ClientID,
			Username: c.Username,
			Position: c.Position,
			Color:    c.Color,
		})
	}
	return cursors
}

// IsRoomEmpty 检查房间是否为空
func (h *Hub) IsRoomEmpty(roomID string) bool {
	h.mu.RLock()
	room, exists := h.Rooms[roomID]
	h.mu.RUnlock()
	if !exists {
		return true
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	return len(room.Clients) == 0
}
