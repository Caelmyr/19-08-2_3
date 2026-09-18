package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"coedit/internal/ws"
)

func httpHandler(s *Server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWebSocket)
	return mux
}

// wsClient 带一个持续读取的消息泵，测试只从 channel 取消息
type wsClient struct {
	conn *websocket.Conn
	msgs chan ws.Message
}

func newWSClient(t *testing.T, srv *httptest.Server, name string) *wsClient {
	t.Helper()
	url := strings.Replace(srv.URL, "http", "ws", 1) + "/ws?doc_id=doc1&username=" + name
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", name, err)
	}
	c := &wsClient{conn: conn, msgs: make(chan ws.Message, 64)}
	go func() {
		for {
			_, data, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var m ws.Message
			if json.Unmarshal(data, &m) == nil {
				select {
				case c.msgs <- m:
				default:
				}
			}
		}
	}()
	return c
}

func (c *wsClient) send(v interface{}) {
	if err := c.conn.WriteJSON(v); err != nil {
		panic(err)
	}
}

func (c *wsClient) wait(t *testing.T, typ ws.MessageType) ws.Message {
	t.Helper()
	// 在剩余消息里找指定类型；过滤掉无关广播
	deadline := time.After(2 * time.Second)
	for {
		select {
		case m := <-c.msgs:
			if m.Type == typ {
				return m
			}
		case <-deadline:
			t.Fatalf("等待消息 %s 超时", typ)
		}
	}
}

func (c *wsClient) next(t *testing.T) ws.Message {
	t.Helper()
	select {
	case m := <-c.msgs:
		return m
	case <-time.After(2 * time.Second):
		t.Fatalf("等待任意消息超时")
		return ws.Message{}
	}
}

func setupFollowServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	hub := ws.NewHub()
	go hub.Run()
	s := NewServer(nil, nil, hub)
	s.docStates["doc1"] = &DocState{ID: "doc1", Content: "hello world", Version: 0}
	srv := httptest.NewServer(httpHandler(s))
	return s, srv
}

// TestE2E_FollowLifecycle 端到端：跟随 → 视野同步 → 被跟随者退出
func TestE2E_FollowLifecycle(t *testing.T) {
	_, srv := setupFollowServer(t)
	defer srv.Close()

	alice := newWSClient(t, srv, "Alice")
	defer alice.conn.Close()
	bob := newWSClient(t, srv, "Bob")
	defer bob.conn.Close()
	carol := newWSClient(t, srv, "Carol")
	defer carol.conn.Close()

	aliceID := alice.wait(t, ws.MsgInit).ClientID
	bob.wait(t, ws.MsgInit)
	carol.wait(t, ws.MsgInit)
	time.Sleep(100 * time.Millisecond) // 让 user_join 广播落袋

	// Bob、Carol 跟随 Alice
	bob.send(map[string]string{"type": "follow", "target_id": aliceID})
	fsBob := bob.wait(t, ws.MsgFollowState)
	if fsBob.Active == nil || !*fsBob.Active || fsBob.TargetID != aliceID {
		t.Fatalf("Bob 应收到 active follow_state, got %+v", fsBob)
	}

	carol.send(map[string]string{"type": "follow", "target_id": aliceID})
	fsCarol := carol.wait(t, ws.MsgFollowState)
	if fsCarol.Active == nil || !*fsCarol.Active {
		t.Fatalf("Carol 应收到 active follow_state, got %+v", fsCarol)
	}

	// Alice 应最终看到2个跟随者
	upd := alice.wait(t, ws.MsgFollowersUpdate)
	for len(upd.Followers) < 2 {
		upd = alice.wait(t, ws.MsgFollowersUpdate)
	}
	if len(upd.Followers) != 2 {
		t.Fatalf("Alice 应有2个跟随者, got %d", len(upd.Followers))
	}

	// Alice 移动视野：两个跟随者收到完全一致的视野
	alice.send(map[string]interface{}{
		"type": "cursor_move", "position": 33,
		"scroll_top_ratio": 0.42, "scroll_left_ratio": 0.1,
		"viewport_height": 600, "viewport_width": 800,
	})
	viewBob := bob.wait(t, ws.MsgCursorMove)
	viewCarol := carol.wait(t, ws.MsgCursorMove)
	if viewBob.ScrollTopRatio != 0.42 || viewCarol.ScrollTopRatio != 0.42 ||
		viewBob.Position != 33 || viewCarol.Position != 33 ||
		viewBob.ScrollLeftRatio != viewCarol.ScrollLeftRatio {
		t.Fatalf("两个跟随者视野不一致: %+v vs %+v", viewBob, viewCarol)
	}

	// 让服务端处理完视野广播再断开 Alice
	time.Sleep(100 * time.Millisecond)

	// Alice 退出：Bob/Carol 收到 follow_stopped(target_left) 和 user_leave
	_ = alice.conn.Close()
	stopBob := bob.wait(t, ws.MsgFollowStopped)
	if stopBob.Reason != ws.FollowStopTargetLeft {
		t.Fatalf("Bob reason 应为 target_left, got %q", stopBob.Reason)
	}
	bob.wait(t, ws.MsgUserLeave)
	stopCarol := carol.wait(t, ws.MsgFollowStopped)
	if stopCarol.Reason != ws.FollowStopTargetLeft {
		t.Fatalf("Carol reason 应为 target_left, got %q", stopCarol.Reason)
	}
	carol.wait(t, ws.MsgUserLeave)
}

// TestE2E_FollowRejections 自跟随/跟随链/不存在目标
func TestE2E_FollowRejections(t *testing.T) {
	_, srv := setupFollowServer(t)
	defer srv.Close()

	alice := newWSClient(t, srv, "Alice")
	defer alice.conn.Close()
	bob := newWSClient(t, srv, "Bob")
	defer bob.conn.Close()

	aliceID := alice.wait(t, ws.MsgInit).ClientID
	bob.wait(t, ws.MsgInit)
	time.Sleep(100 * time.Millisecond)

	// 自跟随
	alice.send(map[string]string{"type": "follow", "target_id": aliceID})
	if m := alice.wait(t, ws.MsgError); !strings.Contains(m.Error, "自己") {
		t.Fatalf("自跟随应报错, got %+v", m)
	}

	// 不存在的目标
	alice.send(map[string]string{"type": "follow", "target_id": "ghost"})
	alice.wait(t, ws.MsgError)
}

// TestE2E_FollowChainRejected Dave 想跟随"正在跟随别人的Bob"应被拒绝
func TestE2E_FollowChainRejected(t *testing.T) {
	_, srv := setupFollowServer(t)
	defer srv.Close()

	alice := newWSClient(t, srv, "Alice")
	defer alice.conn.Close()
	bob := newWSClient(t, srv, "Bob")
	defer bob.conn.Close()
	dave := newWSClient(t, srv, "Dave")
	defer dave.conn.Close()

	aliceID := alice.wait(t, ws.MsgInit).ClientID
	bobInit := bob.wait(t, ws.MsgInit)
	dave.wait(t, ws.MsgInit)
	time.Sleep(100 * time.Millisecond)

	bob.send(map[string]string{"type": "follow", "target_id": aliceID})
	bob.wait(t, ws.MsgFollowState)
	alice.wait(t, ws.MsgFollowersUpdate)

	dave.send(map[string]string{"type": "follow", "target_id": bobInit.ClientID})
	m := dave.wait(t, ws.MsgError)
	if !strings.Contains(m.Error, "跟随别人") {
		t.Fatalf("跟随链应被拒绝, got %+v", m)
	}
}

// TestE2E_Unfollow 主动解除：服务端确认 + leader 列表清零
func TestE2E_Unfollow(t *testing.T) {
	_, srv := setupFollowServer(t)
	defer srv.Close()

	alice := newWSClient(t, srv, "Alice")
	defer alice.conn.Close()
	bob := newWSClient(t, srv, "Bob")
	defer bob.conn.Close()

	aliceID := alice.wait(t, ws.MsgInit).ClientID
	bob.wait(t, ws.MsgInit)
	time.Sleep(100 * time.Millisecond)

	bob.send(map[string]string{"type": "follow", "target_id": aliceID})
	bob.wait(t, ws.MsgFollowState)
	if m := alice.wait(t, ws.MsgFollowersUpdate); len(m.Followers) != 1 {
		t.Fatalf("Alice 应有1个跟随者, got %d", len(m.Followers))
	}

	bob.send(map[string]string{"type": "unfollow"})
	m := bob.wait(t, ws.MsgFollowState)
	if m.Active == nil || *m.Active {
		t.Fatalf("Bob 应收到 active=false, got %+v", m)
	}
	am := alice.wait(t, ws.MsgFollowersUpdate)
	if len(am.Followers) != 0 {
		t.Fatalf("Alice 跟随者应清零, got %d", len(am.Followers))
	}
}

// TestE2E_LeaderRetargets 被跟随者跑去跟随别人，跟随它的人被明确打断
func TestE2E_LeaderRetargets(t *testing.T) {
	_, srv := setupFollowServer(t)
	defer srv.Close()

	alice := newWSClient(t, srv, "Alice")
	defer alice.conn.Close()
	bob := newWSClient(t, srv, "Bob")
	defer bob.conn.Close()
	dave := newWSClient(t, srv, "Dave")
	defer dave.conn.Close()

	alice.wait(t, ws.MsgInit)
	bobID := bob.wait(t, ws.MsgInit).ClientID
	daveInit := dave.wait(t, ws.MsgInit)
	time.Sleep(100 * time.Millisecond) // user_join 广播到达

	// Dave 跟随 Bob（Bob 此时是自由身，可以带节奏）
	dave.send(map[string]string{"type": "follow", "target_id": bobID})
	dave.wait(t, ws.MsgFollowState)
	bob.wait(t, ws.MsgFollowersUpdate)

	// Bob 跑去跟随 Alice（变成跟随者），Dave 应收到 follow_stopped(retarget)
	var aliceID string
	for _, u := range daveInit.Users {
		if u.Username == "Alice" {
			aliceID = u.ClientID
		}
	}
	if aliceID == "" {
		t.Fatal("Dave 的 init 用户中没有 Alice")
	}

	bob.send(map[string]string{"type": "follow", "target_id": aliceID})
	bob.wait(t, ws.MsgFollowState)
	stop := dave.wait(t, ws.MsgFollowStopped)
	if stop.Reason != ws.FollowStopRetarget {
		t.Fatalf("Dave 应收到 retarget, got %q", stop.Reason)
	}
	if stop.TargetID != bobID {
		t.Fatalf("follow_stopped 的 target 应为 Bob, got %q", stop.TargetID)
	}
}
