package ws

import (
	"encoding/json"
	"testing"
	"time"
)

// newTestHub 启动一个hub并创建若干已注册的客户端
func newTestHub(t *testing.T) *Hub {
	t.Helper()
	h := NewHub()
	go h.Run()
	return h
}

func registerClient(h *Hub, name string) *Client {
	c := NewClient(h, nil, "doc1", name)
	h.Register <- c
	// 等hub主循环处理注册（通过加锁的公共方法轮询）
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		for _, u := range h.GetRoomUsers("doc1") {
			if u.ClientID == c.ClientID {
				return c
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return c
}

// drain 非阻塞读取一条消息
func recv(c *Client) (Message, bool) {
	select {
	case data := <-c.Send:
		var m Message
		_ = json.Unmarshal(data, &m)
		return m, true
	case <-time.After(500 * time.Millisecond):
		return Message{}, false
	}
}

// TestFollow_BasicAndFanout 多人跟随同一人，应收到完全相同的视野广播
func TestFollow_BasicAndFanout(t *testing.T) {
	h := newTestHub(t)
	a := registerClient(h, "Alice") // 被跟随者
	b := registerClient(h, "Bob")
	c := registerClient(h, "Carol")

	if _, err := h.StartFollow(b, a.ClientID); err != nil {
		t.Fatalf("Bob follow Alice: %v", err)
	}
	if _, err := h.StartFollow(c, a.ClientID); err != nil {
		t.Fatalf("Carol follow Alice: %v", err)
	}

	followers := h.FollowersOf(a)
	if len(followers) != 2 {
		t.Fatalf("Alice 应有2个跟随者, 实际 %d", len(followers))
	}

	// Alice 移动视野，广播给房间其他人（服务端实际走 BroadcastCursors）
	h.BroadcastCursors("doc1", a.ClientID, Message{
		Type:           MsgCursorMove,
		ClientID:       a.ClientID,
		Position:       42,
		ScrollTopRatio: 0.75,
	})

	mb, ok := recv(b)
	if !ok {
		t.Fatal("Bob 没收到视野广播")
	}
	mc, ok := recv(c)
	if !ok {
		t.Fatal("Carol 没收到视野广播")
	}
	if mb.ScrollTopRatio != 0.75 || mc.ScrollTopRatio != 0.75 ||
		mb.Position != 42 || mc.Position != 42 {
		t.Fatalf("跟随者视野不一致: %+v vs %+v", mb, mc)
	}
}

// TestFollow_SelfAndChainAndMissing 拒绝自跟随/跟随链/不存在目标
func TestFollow_SelfAndChainAndMissing(t *testing.T) {
	h := newTestHub(t)
	a := registerClient(h, "Alice")
	b := registerClient(h, "Bob")
	d := registerClient(h, "Dave")

	if _, err := h.StartFollow(a, a.ClientID); err != ErrFollowSelf {
		t.Fatalf("自跟随应返回 ErrFollowSelf, got %v", err)
	}
	if _, err := h.StartFollow(a, "not-exist"); err != ErrFollowNotFound {
		t.Fatalf("跟随不存在用户应 ErrFollowNotFound, got %v", err)
	}

	// B 跟随 A；D 再跟随 B（B已经是跟随者）应被拒绝
	if _, err := h.StartFollow(b, a.ClientID); err != nil {
		t.Fatalf("B follow A: %v", err)
	}
	if _, err := h.StartFollow(d, b.ClientID); err != ErrFollowChain {
		t.Fatalf("跟随链应返回 ErrFollowChain, got %v", err)
	}
}

// TestFollow_TargetLeaves 被跟随者退出：跟随者收到 follow_stopped 并停在最后位置
func TestFollow_TargetLeaves(t *testing.T) {
	h := newTestHub(t)
	a := registerClient(h, "Alice")
	b := registerClient(h, "Bob")
	c := registerClient(h, "Carol")

	h.StartFollow(b, a.ClientID)
	h.StartFollow(c, a.ClientID)
	time.Sleep(20 * time.Millisecond)
	// 清掉注册/跟随确认之外可能堆积的消息（这里hub不主动发，无需清理）

	h.Unregister <- a // Alice 退出
	time.Sleep(50 * time.Millisecond)

	if b.FollowingID() != "" || c.FollowingID() != "" {
		t.Fatalf("目标退出后跟随关系应清除: b=%q c=%q", b.FollowingID(), c.FollowingID())
	}

	mb, ok := recv(b)
	if !ok || mb.Type != MsgFollowStopped || mb.Reason != FollowStopTargetLeft {
		t.Fatalf("Bob 应收到 target_left 的 follow_stopped, got %+v ok=%v", mb, ok)
	}
	mc, ok := recv(c)
	if !ok || mc.Type != MsgFollowStopped || mc.Reason != FollowStopTargetLeft {
		t.Fatalf("Carol 应收到 target_left 的 follow_stopped, got %+v ok=%v", mc, ok)
	}
}

// TestFollow_LeaderRetargets 被跟随者自己跑去跟随别人，跟随它的人被明确打断
func TestFollow_LeaderRetargets(t *testing.T) {
	h := newTestHub(t)
	a := registerClient(h, "Alice")
	b := registerClient(h, "Bob")
	d := registerClient(h, "Dave")

	// D 跟随 B（B是leader）；随后 B 跟随 A
	h.StartFollow(d, b.ClientID)
	change, err := h.StartFollow(b, a.ClientID)
	if err != nil {
		t.Fatalf("B 转去跟随 A: %v", err)
	}
	if len(change.StoppedFollowers) != 1 || change.StoppedFollowers[0].ClientID != d.ClientID {
		t.Fatalf("应打断 D, got %+v", change.StoppedFollowers)
	}
	if d.FollowingID() != "" {
		t.Fatalf("D 的跟随应被清除, got %q", d.FollowingID())
	}
	if b.FollowingID() != a.ClientID {
		t.Fatalf("B 应正在跟随 A, got %q", b.FollowingID())
	}

	// 主动解除：B 停止跟随 A
	target, remaining := h.StopFollow(b)
	if target == nil || target.ClientID != a.ClientID {
		t.Fatalf("解除跟随应返回 A, got %v", target)
	}
	if len(remaining) != 0 {
		t.Fatalf("A 不应有跟随者, got %d", len(remaining))
	}
	if b.FollowingID() != "" {
		t.Fatalf("解除后 B 的跟随应为空, got %q", b.FollowingID())
	}
}
