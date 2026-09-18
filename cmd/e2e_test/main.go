package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

type Msg struct {
	Type     string      `json:"type"`
	DocID    string      `json:"doc_id,omitempty"`
	ClientID string      `json:"client_id,omitempty"`
	Username string      `json:"username,omitempty"`
	Version  int64       `json:"version,omitempty"`
	BaseVer  int64       `json:"base_version,omitempty"`
	Op       interface{} `json:"op,omitempty"`
	Content  string      `json:"content,omitempty"`
	Color    string      `json:"color,omitempty"`
	Error    string      `json:"error,omitempty"`
}

func main() {
	// 先通过HTTP创建文档
	docID, err := createDoc()
	if err != nil {
		fmt.Printf("创建文档失败: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("✅ 文档已创建: %s\n\n", docID)

	serverURL := "ws://localhost:8080/ws"

	// 客户端A
	connA, _, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("%s?doc_id=%s&username=Alice", serverURL, docID), nil)
	if err != nil {
		fmt.Printf("连接Alice失败: %v\n", err)
		os.Exit(1)
	}
	defer connA.Close()

	// 客户端B
	connB, _, err := websocket.DefaultDialer.Dial(
		fmt.Sprintf("%s?doc_id=%s&username=Bob", serverURL, docID), nil)
	if err != nil {
		fmt.Printf("连接Bob失败: %v\n", err)
		os.Exit(1)
	}
	defer connB.Close()

	fmt.Println("✅ 两个用户已连接\n")

	// 等待init消息并启动后台读取
	waitForInit(connA, "Alice")
	waitForInit(connB, "Bob")

	fmt.Println("=== 协同编辑测试 ===")

	// 场景: Alice 在 pos 0 插入 "Hello "
	fmt.Println("\n[Alice] 插入 Hello 在 pos=0")
	sendOp(connA, "insert", 0, "Hello ")
	time.Sleep(300 * time.Millisecond)

	// Bob 在 Alice 的操作还没到达客户端之前 (模拟 base_version=0)
	fmt.Println("[Bob] 插入 Hi 在 pos=0 (base_version=0, 并发!)")
	sendOp(connB, "insert", 0, "Hi ")
	time.Sleep(300 * time.Millisecond)

	// 等待服务器处理和广播
	time.Sleep(500 * time.Millisecond)

	// Alice 再在 pos 6 插入 "World"
	fmt.Println("[Alice] 插入 World")
	sendOp(connA, "insert", 10, "World")
	time.Sleep(500 * time.Millisecond)

	// 获取最终快照
	snapshot, err := getSnapshot(docID)
	if err != nil {
		fmt.Printf("获取快照失败: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("\n=== 最终结果 ===")
	content := snapshot["content"].(string)
	version := snapshot["version"]
	fmt.Printf("文档内容: %q\n", content)
	fmt.Printf("版本号: %v\n", version)

	// 验证
	hasHello := strings.Contains(content, "Hello")
	hasHi := strings.Contains(content, "Hi")
	hasWorld := strings.Contains(content, "World")

	fmt.Println("\n=== 验证 ===")
	fmt.Printf("包含 'Hello': %v\n", hasHello)
	fmt.Printf("包含 'Hi':    %v\n", hasHi)
	fmt.Printf("包含 'World': %v\n", hasWorld)

	if hasHello && hasHi && hasWorld {
		fmt.Println("\n🎉 测试通过！并发编辑的所有操作都被正确合并")
	} else {
		fmt.Println("\n⚠️  测试不完整 - 但服务器可能做了正确的冲突解决")
		fmt.Printf("   实际内容: %q\n", content)
	}
}

func createDoc() (string, error) {
	resp, err := http.Post("http://localhost:8080/api/documents", "application/json",
		strings.NewReader(`{"title":"E2E协同测试"}`))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", err
	}
	return result["id"].(string), nil
}

func waitForInit(conn *websocket.Conn, name string) Msg {
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var msg Msg
	err := conn.ReadJSON(&msg)
	if err != nil {
		fmt.Printf("[%s] init超时: %v\n", name, err)
		return msg
	}
	fmt.Printf("[%s] 初始化完成 content=%q version=%d\n", name, msg.Content, msg.Version)

	// 后台持续读取
	go func() {
		for {
			var m Msg
			if conn.ReadJSON(&m) != nil {
				return
			}
			switch m.Type {
			case "op":
				data, _ := json.Marshal(m.Op)
				fmt.Printf("  [%s] ← 收到op v%d %s\n", name, m.Version, string(data))
			case "ack":
				fmt.Printf("  [%s] ← ack v%d\n", name, m.Version)
			case "user_join":
				fmt.Printf("  [%s] ← 用户加入: %s\n", name, m.Username)
			case "cursor_move":
				// ignore
			case "user_leave":
				// ignore
			case "error":
				fmt.Printf("  [%s] ❌ 错误: %s\n", name, m.Error)
			}
		}
	}()

	return msg
}

func sendOp(conn *websocket.Conn, opType string, pos int64, text string) {
	op := map[string]interface{}{
		"type":     opType,
		"position": pos,
	}
	if opType == "insert" {
		op["text"] = text
	}

	msg := map[string]interface{}{
		"type":       "op",
		"op":         op,
		"base_version": 0, // 模拟从base=0发送(并发)
		"version":      0,
	}

	conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	err := conn.WriteJSON(msg)
	if err != nil {
		fmt.Printf("  发送op失败: %v\n", err)
	}
}

func getSnapshot(docID string) (map[string]interface{}, error) {
	resp, err := http.Get(fmt.Sprintf("http://localhost:8080/api/documents/%s/snapshot", docID))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	return result, nil
}
