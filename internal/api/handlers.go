// Package api 实现HTTP API路由和WebSocket OT处理器
package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"coedit/internal/ot"
	"coedit/internal/store"
	"coedit/internal/ws"
)

// Server 服务端应用
type Server struct {
	DB       *sql.DB
	Store    *store.Store
	Hub      *ws.Hub
	upgrader websocket.Upgrader

	// 内存中的文档状态缓存（用于实时OT）
	// 生产环境应该用分布式锁，单机用内存map就行
	docStates map[string]*DocState
	mu        sync.RWMutex
}

// DocState 文档的运行时状态
type DocState struct {
	ID        string
	Content   string
	Version   int64
	Ops       []ot.Operation // 版本号从1开始，ops[0]对应version=1
	mu        sync.Mutex
}

// NewServer 创建服务端
func NewServer(db *sql.DB, st *store.Store, hub *ws.Hub) *Server {
	return &Server{
		DB:    db,
		Store: st,
		Hub:   hub,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 1024,
			CheckOrigin:     func(r *http.Request) bool { return true },
		},
		docStates: make(map[string]*DocState),
	}
}

// getOrLoadDocState 获取或加载文档运行时状态
func (s *Server) getOrLoadDocState(docID string) (*DocState, error) {
	s.mu.RLock()
	state, exists := s.docStates[docID]
	s.mu.RUnlock()
	if exists {
		return state, nil
	}

	// 从数据库加载
	doc, err := s.Store.GetDocument(docID)
	if err != nil {
		return nil, err
	}
	if doc == nil {
		return nil, fmt.Errorf("document not found: %s", docID)
	}

	state = &DocState{
		ID:      docID,
		Content: doc.ContentSnapshot,
		Version: doc.CurrentVersion,
		Ops:     nil,
	}

	// 加载操作序列
	records, err := s.Store.GetOperationsRange(docID, 0, doc.CurrentVersion)
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		op := recordToOp(rec)
		state.Ops = append(state.Ops, op)
	}

	s.mu.Lock()
	s.docStates[docID] = state
	s.mu.Unlock()

	return state, nil
}

// recordToOp 将数据库记录转换为OT操作
func recordToOp(rec store.OperationRecord) ot.Operation {
	return ot.Operation{
		Type:     ot.OpType(rec.OpType),
		Position: rec.Position,
		Length:   rec.Length,
		Text:     rec.Content,
	}
}

// opToRecordType 将OT操作转为数据库字段
func opToRecordType(op ot.Operation) string {
	return string(op.Type)
}

// ============================================================
// HTTP API
// ============================================================

// CreateDocument 创建文档
func (s *Server) CreateDocument(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	if r.Header.Get("Content-Type") == "application/json" {
		json.NewDecoder(r.Body).Decode(&req)
	}
	if req.Title == "" {
		req.Title = "未命名文档"
	}

	id := uuid.New().String()
	doc, err := s.Store.CreateDocument(id, req.Title)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, doc)
}

// GetDocument 获取文档详情
func (s *Server) GetDocument(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = strings.TrimPrefix(r.URL.Path, "/api/documents/")
		id = strings.TrimSuffix(id, "/snapshot")
		id = strings.TrimSuffix(id, "/versions")
		id = strings.TrimSuffix(id, "/rollback")
	}

	doc, err := s.Store.GetDocument(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if doc == nil {
		http.Error(w, "document not found", 404)
		return
	}

	writeJSON(w, 200, doc)
}

// ListDocuments 列出所有文档
func (s *Server) ListDocuments(w http.ResponseWriter, r *http.Request) {
	docs, err := s.Store.ListDocuments()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, docs)
}

// GetDocumentByID 按ID获取文档
func (s *Server) GetDocumentByID(w http.ResponseWriter, r *http.Request, id string) {
	doc, err := s.Store.GetDocument(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	if doc == nil {
		http.Error(w, "document not found", 404)
		return
	}
	writeJSON(w, 200, doc)
}

// DeleteDocumentByID 按ID删除文档
func (s *Server) DeleteDocumentByID(w http.ResponseWriter, r *http.Request, id string) {
	_, err := s.DB.Exec("DELETE FROM documents WHERE id = ?", id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	s.mu.Lock()
	delete(s.docStates, id)
	s.mu.Unlock()
	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// GetDocumentSnapshotByID 按ID获取快照
func (s *Server) GetDocumentSnapshotByID(w http.ResponseWriter, r *http.Request, id string) {

	state, err := s.getOrLoadDocState(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	state.mu.Lock()
	content := state.Content
	version := state.Version
	state.mu.Unlock()

	writeJSON(w, 200, map[string]interface{}{
		"doc_id":   id,
		"content":  content,
		"version":  version,
	})
}

// GetOperationsByID 按ID获取增量操作
func (s *Server) GetOperationsByID(w http.ResponseWriter, r *http.Request, id string) {
	sinceStr := r.URL.Query().Get("since")
	since, _ := strconv.ParseInt(sinceStr, 10, 64)

	ops, err := s.Store.GetOperationsSince(id, since)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"doc_id": id,
		"since":  since,
		"ops":    ops,
	})
}

// GetVersionHistoryByID 按ID获取版本历史
func (s *Server) GetVersionHistoryByID(w http.ResponseWriter, r *http.Request, id string) {
	state, err := s.getOrLoadDocState(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	writeJSON(w, 200, map[string]interface{}{
		"doc_id":  id,
		"version": state.Version,
		"content": state.Content,
	})
}

// RollbackDocumentByID 按ID回滚版本
func (s *Server) RollbackDocumentByID(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	if req.Version < 0 {
		http.Error(w, "version must be non-negative", 400)
		return
	}

	state, err := s.getOrLoadDocState(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	state.mu.Lock()

	newContent := ""
	records, err := s.Store.GetOperationsRange(id, 0, req.Version)
	if err != nil {
		state.mu.Unlock()
		http.Error(w, err.Error(), 500)
		return
	}

	var ops []ot.Operation
	for _, rec := range records {
		op := recordToOp(rec)
		newContent, err = ot.Apply(newContent, op)
		if err != nil {
			state.mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		ops = append(ops, op)
	}

	state.Content = newContent
	state.Version = req.Version
	state.Ops = ops

	state.mu.Unlock()

	if err := s.Store.SaveSnapshotTransaction(id, newContent, req.Version); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"doc_id":  id,
		"version": req.Version,
		"content": newContent,
		"message": "rolled back successfully",
	})
}

// GetOperations 获取增量操作
func (s *Server) GetOperations(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/documents/")
	id = strings.TrimSuffix(id, "/operations")

	sinceStr := r.URL.Query().Get("since")
	since, _ := strconv.ParseInt(sinceStr, 10, 64)

	ops, err := s.Store.GetOperationsSince(id, since)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"doc_id": id,
		"since":  since,
		"ops":    ops,
	})
}

// GetVersionHistory 获取版本历史
func (s *Server) GetVersionHistory(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/documents/")
	id = strings.TrimSuffix(id, "/versions")

	state, err := s.getOrLoadDocState(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	writeJSON(w, 200, map[string]interface{}{
		"doc_id":  id,
		"version": state.Version,
		"content": state.Content,
	})
}

// RollbackDocument 回滚到指定版本
func (s *Server) RollbackDocument(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/documents/")
	id = strings.TrimSuffix(id, "/rollback")

	var req struct {
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	if req.Version < 0 {
		http.Error(w, "version must be non-negative", 400)
		return
	}

	state, err := s.getOrLoadDocState(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	state.mu.Lock()

	// 从base重新计算到目标版本
	newContent := ""
	records, err := s.Store.GetOperationsRange(id, 0, req.Version)
	if err != nil {
		state.mu.Unlock()
		http.Error(w, err.Error(), 500)
		return
	}

	var ops []ot.Operation
	for _, rec := range records {
		op := recordToOp(rec)
		newContent, err = ot.Apply(newContent, op)
		if err != nil {
			state.mu.Unlock()
			http.Error(w, err.Error(), 500)
			return
		}
		ops = append(ops, op)
	}

	state.Content = newContent
	state.Version = req.Version
	state.Ops = ops

	state.mu.Unlock()

	// 更新数据库快照和版本
	if err := s.Store.SaveSnapshotTransaction(id, newContent, req.Version); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]interface{}{
		"doc_id":  id,
		"version": req.Version,
		"content": newContent,
		"message": "rolled back successfully",
	})
}

// DeleteDocument 删除文档
func (s *Server) DeleteDocument(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/documents/")

	// 删除数据库记录
	_, err := s.DB.Exec("DELETE FROM documents WHERE id = ?", id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// 清理内存状态
	s.mu.Lock()
	delete(s.docStates, id)
	s.mu.Unlock()

	writeJSON(w, 200, map[string]string{"status": "deleted"})
}

// ============================================================
// WebSocket 处理
// ============================================================

// HandleWebSocket 处理WebSocket连接
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// 从URL参数获取doc_id和username
	docID := r.URL.Query().Get("doc_id")
	username := r.URL.Query().Get("username")
	if username == "" {
		username = "User-" + uuid.New().String()[:4]
	}

	if docID == "" {
		http.Error(w, "missing doc_id", 400)
		return
	}

	// 确保文档存在
	_, err := s.getOrLoadDocState(docID)
	if err != nil {
		http.Error(w, err.Error(), 404)
		return
	}

	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("[WS] Upgrade error: %v", err)
		return
	}

	client := ws.NewClient(s.Hub, conn, docID, username)

	// 注册到Hub
	s.Hub.Register <- client

	// 发送初始化消息
	s.sendInitMessage(client)

	// 启动读写循环
	go client.WritePump()
	go client.ReadPump(s.handleMessage)
}

// sendInitMessage 发送初始化消息给新连接的客户端
func (s *Server) sendInitMessage(client *ws.Client) {
	state, err := s.getOrLoadDocState(client.RoomID)
	if err != nil {
		return
	}

	state.mu.Lock()
	msg := ws.Message{
		Type:     ws.MsgInit,
		DocID:    client.RoomID,
		ClientID: client.ClientID,
		Username: client.Username,
		Content:  state.Content,
		Version:  state.Version,
		Users:    s.Hub.GetRoomUsers(client.RoomID),
		Cursors:  s.Hub.GetRoomCursors(client.RoomID),
		Color:    client.Color,
	}
	state.mu.Unlock()

	client.SendMessage(msg)

	// 通知其他用户有新用户加入
	s.Hub.BroadcastOp(client.RoomID, client.ClientID, ws.Message{
		Type:     ws.MsgUserJoin,
		DocID:    client.RoomID,
		ClientID: client.ClientID,
		Username: client.Username,
		Color:    client.Color,
	})
}

// handleMessage 处理来自客户端的WebSocket消息
func (s *Server) handleMessage(client *ws.Client, msg ws.Message) {
	switch msg.Type {
	case ws.MsgCursorMove:
		// 更新光标位置并广播
		client.Position = msg.Position

		cursorMsg := ws.Message{
			Type:     ws.MsgCursorMove,
			DocID:    client.RoomID,
			ClientID: client.ClientID,
			Username: client.Username,
			Position: msg.Position,
			Color:    client.Color,
		}
		s.Hub.BroadcastCursors(client.RoomID, client.ClientID, cursorMsg)

	case ws.MsgOp:
		// 处理OT操作
		s.handleOperation(client, msg)
	}
}

// handleOperation 核心OT操作处理
func (s *Server) handleOperation(client *ws.Client, msg ws.Message) {
	// 解析操作
	rawOp, err := json.Marshal(msg.Op)
	if err != nil {
		s.sendError(client, "invalid op format")
		return
	}

	op, err := ot.UnmarshalOp(rawOp)
	if err != nil {
		s.sendError(client, fmt.Sprintf("invalid op: %v", err))
		return
	}

	state, err := s.getOrLoadDocState(client.RoomID)
	if err != nil {
		s.sendError(client, "document not found")
		return
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	// 获取客户端报告的base_version
	baseVersion := msg.BaseVer
	if baseVersion == 0 {
		baseVersion = state.Version
	}

	// Adjust op against 所有中间版本的操作
	if baseVersion < state.Version {
		// 取出中间操作 [baseVersion+1, currentVersion]
		// state.Ops 是从version=1开始的
		// 版本号 vs 索引: state.Ops[i] 的版本是 i+1
		startIdx := baseVersion
		endIdx := state.Version
		if startIdx < 0 {
			startIdx = 0
		}
		if endIdx > int64(len(state.Ops)) {
			endIdx = int64(len(state.Ops))
		}

		intermediateOps := state.Ops[startIdx:endIdx]
		op = ot.AdjustOpAgainstSeq(op, intermediateOps)
	}

	// 验证操作
	if err := op.Validate(len([]rune(state.Content))); err != nil {
		log.Printf("[OT] Op validation error: %v, content=%q, op=%s", err, state.Content, op)
		s.sendError(client, err.Error())
		return
	}

	// 应用操作
	newContent, err := ot.Apply(state.Content, op)
	if err != nil {
		log.Printf("[OT] Apply error: %v, op=%s", err, op)
		s.sendError(client, err.Error())
		return
	}

	// 更新状态
	state.Content = newContent
	state.Version++
	state.Ops = append(state.Ops, op)
	newVersion := state.Version

	// 持久化到数据库
	_ = s.Store.AppendOperation(
		client.RoomID, client.ClientID,
		string(op.Type), newVersion,
		int64(op.Position), int64(op.Length), op.Text,
	)
	_ = s.Store.UpdateSnapshot(client.RoomID, newContent, newVersion)

	log.Printf("[OT] Op applied: doc=%s, version=%d, client=%s, op=%s, content_len=%d",
		client.RoomID, newVersion, client.ClientID, op, len([]rune(newContent)))

	// 广播给房间内其他用户
	broadcastMsg := ws.Message{
		Type:      ws.MsgOp,
		DocID:     client.RoomID,
		ClientID:  client.ClientID,
		Username:  client.Username,
		Version:   newVersion,
		BaseVer:   newVersion - 1,
		Op:        op,
		Timestamp: time.Now(),
	}

	s.Hub.BroadcastOp(client.RoomID, client.ClientID, broadcastMsg)

	// 发送ack给发送者
	ackMsg := ws.Message{
		Type:     ws.MsgAck,
		DocID:    client.RoomID,
		ClientID: client.ClientID,
		Version:  newVersion,
	}
	client.SendMessage(ackMsg)
}

// sendError 发送错误消息
func (s *Server) sendError(client *ws.Client, errMsg string) {
	client.SendMessage(ws.Message{
		Type:     ws.MsgError,
		DocID:    client.RoomID,
		ClientID: client.ClientID,
		Error:    errMsg,
	})
}

// ============================================================
// 辅助函数
// ============================================================

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
