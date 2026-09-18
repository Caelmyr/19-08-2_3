// Package ws 实现WebSocket Hub和房间管理
// 负责连接管理、操作广播、在线用户光标/视口同步、跟随（follow）关系维护
package ws

import (
	"encoding/json"
	"log"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
)

// sortClients 按 ClientID 稳定排序
func sortClients(cs []*Client) {
	sort.Slice(cs, func(i, j int) bool { return cs[i].ClientID < cs[j].ClientID })
}

// MessageType 消息类型
type MessageType string

const (
	MsgOp              MessageType = "op"               // 操作消息
	MsgAck             MessageType = "ack"              // 操作确认
	MsgCursors         MessageType = "cursors"          // 光标位置广播
	MsgCursorMove      MessageType = "cursor_move"      // 光标/视口移动
	MsgUserJoin        MessageType = "user_join"        // 用户加入
	MsgUserLeave       MessageType = "user_leave"       // 用户离开
	MsgInit            MessageType = "init"             // 初始化消息
	MsgError           MessageType = "error"            // 错误消息
	MsgSnapshot        MessageType = "snapshot"         // 快照请求
	MsgFollow          MessageType = "follow"           // 请求跟随某人
	MsgUnfollow        MessageType = "unfollow"         // 主动解除跟随
	MsgFollowState     MessageType = "follow_state"     // 跟随状态/目标最新视野
	MsgFollowStopped   MessageType = "follow_stopped"   // 跟随被动结束（对方离开等）
	MsgFollowersUpdate MessageType = "followers_update" // 我的跟随者数量变化
)

// 跟随被动结束的原因
const (
	FollowStopTargetLeft = "target_left" // 被跟随者断线/退出
	FollowStopRetarget   = "retarget"    // 跟随者改去跟随别人
)

// Message WebSocket消息
type Message struct {
	Type      MessageType `json:"type"`
	DocID     string      `json:"doc_id,omitempty"`
	ClientID  string      `json:"client_id,omitempty"`
	Username  string      `json:"username,omitempty"`
	TargetID  string      `json:"target_id,omitempty"` // follow 的目标
	Version   int64       `json:"version,omitempty"`
	BaseVer   int64       `json:"base_version,omitempty"`
	Op        interface{} `json:"op,omitempty"`
	Content   string      `json:"content,omitempty"`
	Users     []UserInfo  `json:"users,omitempty"`
	Cursors   []Cursor    `json:"cursors,omitempty"`
	Position  int         `json:"position,omitempty"`
	Color     string      `json:"color,omitempty"`
	Error     string      `json:"error,omitempty"`
	Reason    string      `json:"reason,omitempty"`
	Active    *bool       `json:"active,omitempty"`
	Followers []UserInfo  `json:"followers,omitempty"`
	// 视口信息：跟随功能需要同步滚动位置。
	// 传比例（scrollTop/可滚动高度）而不是绝对像素，
	// 这样不同窗口尺寸的跟随者也能看到同一段内容。
	ScrollTopRatio  float64   `json:"scroll_top_ratio,omitempty"`
	ScrollLeftRatio float64   `json:"scroll_left_ratio,omitempty"`
	ViewportHeight  int       `json:"viewport_height,omitempty"`
	ViewportWidth   int       `json:"viewport_width,omitempty"`
	Timestamp       time.Time `json:"timestamp,omitempty"`
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

// Viewport 客户端当前视口（用于跟随同步）
type Viewport struct {
	Position        int     `json:"position"`
	ScrollTopRatio  float64 `json:"scroll_top_ratio"`
	ScrollLeftRatio float64 `json:"scroll_left_ratio"`
	ViewportHeight  int     `json:"viewport_height"`
	ViewportWidth   int     `json:"viewport_width"`
}

// Client 表示一个WebSocket连接
type Client struct {
	Hub       *Hub
	RoomID    string
	ClientID  string
	Username  string
	Color     string
	Conn      *websocket.Conn
	Send      chan []byte
	done      chan struct{} // 连接关闭时关闭，通知所有发送方放弃
	closeOnce sync.Once
	mu        sync.Mutex
	// stateMu 保护下面的光标/视口/跟随状态，
	// 与 mu（保护 Send channel）分开，避免嵌套死锁。
	stateMu     sync.Mutex
	position    int
	viewport    Viewport
	followingID string // 正在跟随谁（空表示没有跟随）
}

// Done 返回连接生命周期结束信号
func (c *Client) Done() <-chan struct{} { return c.done }

// Close 标记连接已关闭（幂等）。不关闭 Send channel，
// 这样其他 goroutine 向其发送消息不会因"send on closed channel"而 panic；
// 发送方通过 select 监听 done 放弃投递。
func (c *Client) Close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// SetCursor 更新光标位置
func (c *Client) SetCursor(pos int) {
	c.stateMu.Lock()
	c.position = pos
	c.stateMu.Unlock()
}

// SetViewport 更新视口信息（同时更新光标位置）
func (c *Client) SetViewport(v Viewport) {
	c.stateMu.Lock()
	c.position = v.Position
	c.viewport = v
	c.stateMu.Unlock()
}

// Snapshot 返回光标和视口的快照
func (c *Client) Snapshot() (int, Viewport) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.position, c.viewport
}

// FollowingID 返回当前跟随目标
func (c *Client) FollowingID() string {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.followingID
}

// SetFollowing 设置跟随目标
func (c *Client) SetFollowing(id string) {
	c.stateMu.Lock()
	c.followingID = id
	c.stateMu.Unlock()
}

// Room 表示一个文档房间
type Room struct {
	ID      string
	Clients map[string]*Client
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
		room = &Room{ID: client.RoomID, Clients: make(map[string]*Client)}
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

	// 在锁内收集跟随关系并清理
	var stoppedFollowers []*Client // 正在跟随离开者的人
	var leader *Client             // 离开者正在跟随的人
	var leaderFollowers []*Client  // 离开者的leader的新跟随者列表
	isEmpty := false

	room.mu.Lock()
	if _, ok := room.Clients[client.ClientID]; ok {
		// 1) 正在跟随离开者的人 -> 跟随终止
		for _, c := range room.Clients {
			if c.FollowingID() == client.ClientID {
				c.SetFollowing("")
				stoppedFollowers = append(stoppedFollowers, c)
			}
		}
		// 2) 离开者本人正在跟随某人 -> 对方的跟随者列表要更新
		leaderID := client.FollowingID()
		client.SetFollowing("")
		if leaderID != "" {
			leader = room.Clients[leaderID]
		}

		delete(room.Clients, client.ClientID)
		client.Close() // 标记关闭，不 close(Send) 以避免其他发送方 panic

		isEmpty = len(room.Clients) == 0
		if leader != nil {
			leaderFollowers = room.followersLocked(leader.ClientID)
		}
	}
	room.mu.Unlock()

	// 如果房间为空，删除
	if isEmpty {
		h.mu.Lock()
		delete(h.Rooms, client.RoomID)
		h.mu.Unlock()
		log.Printf("[Hub] Room %s empty, removed", client.RoomID)
		return
	}

	// 通知被打断的跟随者：停在最后位置
	stopMsg := Message{
		Type:     MsgFollowStopped,
		DocID:    room.ID,
		TargetID: client.ClientID,
		Username: client.Username,
		Reason:   FollowStopTargetLeft,
	}
	stopData, _ := json.Marshal(stopMsg)
	for _, c := range stoppedFollowers {
		sendRaw(c, stopData)
	}
	// leader 的跟随者数量变化
	if leader != nil {
		sendRaw(leader, mustMarshal(Message{
			Type:      MsgFollowersUpdate,
			DocID:     room.ID,
			Followers: toUserInfos(leaderFollowers),
		}))
	}

	// 通知其他用户离开
	h.broadcastUserLeave(room, client)
	log.Printf("[Hub] Client %s left room %s (remaining: %d, stopped %d followers)",
		client.ClientID, client.RoomID, len(room.Clients), len(stoppedFollowers))
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
		case <-client.done:
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
		case <-client.done:
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

// FollowChange StartFollow 的结果：携带锁外需要发送的通知。
type FollowChange struct {
	Follower           *Client   // 发起跟随的人
	Target             *Client   // 被跟随者
	TargetViewport     Viewport  // 被跟随者当前视野
	StoppedFollowers   []*Client // 因为 follower 原来在带节奏，被打断跟随的人
	OldLeader          *Client   // follower 原来跟随的人（切换目标时）
	OldLeaderFollowers []*Client // 原leader剩余跟随者
}

// StartFollow 建立 follower -> targetID 的跟随关系。
// 返回 ErrFollow* 表示被拒绝；返回 *FollowChange 表示成功，调用方负责发通知。
func (h *Hub) StartFollow(follower *Client, targetID string) (*FollowChange, error) {
	if targetID == follower.ClientID {
		return nil, ErrFollowSelf
	}

	h.mu.RLock()
	room, exists := h.Rooms[follower.RoomID]
	h.mu.RUnlock()
	if !exists {
		return nil, ErrDocNotFound
	}

	change := &FollowChange{Follower: follower}

	room.mu.Lock()
	defer room.mu.Unlock()

	target, ok := room.Clients[targetID]
	if !ok {
		return nil, ErrFollowNotFound
	}
	// 不允许跟随链：被跟随者必须是"自由身"
	if target.FollowingID() != "" {
		return nil, ErrFollowChain
	}

	// follower 原来在跟随谁？切换目标时通知旧leader
	if oldID := follower.FollowingID(); oldID != "" && oldID != targetID {
		change.OldLeader = room.Clients[oldID]
	}

	// follower 原来如果正被别人跟随（是leader），新跟随会让它变成跟随者，
	// 跟随它的人需要被明确打断。
	change.StoppedFollowers = room.followersLocked(follower.ClientID)
	for _, c := range change.StoppedFollowers {
		c.SetFollowing("")
	}

	follower.SetFollowing(targetID)
	change.Target = target
	_, change.TargetViewport = target.Snapshot()

	if change.OldLeader != nil {
		change.OldLeaderFollowers = room.followersLocked(change.OldLeader.ClientID)
	}
	return change, nil
}

// StopFollow 主动解除跟随，返回被跟随者（若存在）及其剩余跟随者列表
func (h *Hub) StopFollow(follower *Client) (target *Client, remaining []*Client) {
	h.mu.RLock()
	room, exists := h.Rooms[follower.RoomID]
	h.mu.RUnlock()
	if !exists {
		return nil, nil
	}

	room.mu.Lock()
	defer room.mu.Unlock()

	targetID := follower.FollowingID()
	if targetID == "" {
		return nil, nil
	}
	follower.SetFollowing("")
	target = room.Clients[targetID]
	if target != nil {
		remaining = room.followersLocked(targetID)
	}
	return target, remaining
}

// FollowersOf 返回某客户端的跟随者（锁外安全的快照）
func (h *Hub) FollowersOf(client *Client) []*Client {
	h.mu.RLock()
	room, exists := h.Rooms[client.RoomID]
	h.mu.RUnlock()
	if !exists {
		return nil
	}
	room.mu.RLock()
	defer room.mu.RUnlock()
	return room.followersLocked(client.ClientID)
}

// followersLocked 返回 clientID 的跟随者列表（调用方持有 room.mu）
func (r *Room) followersLocked(clientID string) []*Client {
	var out []*Client
	for _, c := range r.Clients {
		if c.FollowingID() == clientID {
			out = append(out, c)
		}
	}
	// 稳定排序，保证每个跟随者看到一致的呈现顺序
	sortClients(out)
	return out
}

func toUserInfos(clients []*Client) []UserInfo {
	users := make([]UserInfo, 0, len(clients))
	for _, c := range clients {
		users = append(users, UserInfo{
			ClientID: c.ClientID,
			Username: c.Username,
			Color:    c.Color,
		})
	}
	return users
}

// sendRaw 尽力推送一条已序列化消息（连接已关闭或缓冲区满则丢弃）
func sendRaw(c *Client, data []byte) {
	select {
	case <-c.done:
	case c.Send <- data:
	default:
		log.Printf("[Hub] Client %s send buffer full, dropping follow message", c.ClientID)
	}
}

func mustMarshal(v interface{}) []byte {
	data, _ := json.Marshal(v)
	return data
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
	sort.Slice(users, func(i, j int) bool { return users[i].ClientID < users[j].ClientID })
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
		pos, _ := c.Snapshot()
		cursors = append(cursors, Cursor{
			ClientID: c.ClientID,
			Username: c.Username,
			Position: pos,
			Color:    c.Color,
		})
	}
	sort.Slice(cursors, func(i, j int) bool { return cursors[i].ClientID < cursors[j].ClientID })
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
