package api

import (
	"encoding/json"
	"testing"
	"time"

	"coedit/internal/ws"
)

// 跟随功能集成测试：直接驱动 Hub + handleMessage，不依赖数据库

func newFollowTestServer() (*Server, *ws.Hub) {
	hub := ws.NewHub()
	go hub.Run()
	// follow/viewport 路径不触碰 store，传 nil 即可
	return NewServer(nil, nil, hub), hub
}

// joinClient 创建伪客户端并注册到房间（Send 是有缓冲channel，无需真实连接）
func joinClient(t *testing.T, hub *ws.Hub, room, name string) *ws.Client {
	t.Helper()
	c := ws.NewClient(hub, nil, room, name)
	hub.Register <- c
	waitFor(t, func() bool {
		for _, u := range hub.GetRoomUsers(room) {
			if u.ClientID == c.ClientID {
				return true
			}
		}
		return false
	}, "client registered")
	return c
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for: %s", what)
}

// recvMsg 从客户端发送缓冲区读下一条消息
func recvMsg(t *testing.T, c *ws.Client, who string) ws.Message {
	t.Helper()
	select {
	case data := <-c.Send:
		var msg ws.Message
		if err := json.Unmarshal(data, &msg); err != nil {
			t.Fatalf("[%s] unmarshal: %v", who, err)
		}
		return msg
	case <-time.After(2 * time.Second):
		t.Fatalf("[%s] timeout waiting for message", who)
		return ws.Message{}
	}
}

// expectNoMsg 断言指定时间内没有消息
func expectNoMsg(t *testing.T, c *ws.Client, who string) {
	t.Helper()
	select {
	case data := <-c.Send:
		t.Fatalf("[%s] unexpected message: %s", who, string(data))
	case <-time.After(150 * time.Millisecond):
	}
}

func TestFollowAckCarriesViewportSnapshot(t *testing.T) {
	s, hub := newFollowTestServer()
	presenter := joinClient(t, hub, "room1", "Presenter")
	follower := joinClient(t, hub, "room1", "Follower")

	// 被跟随者先上报过自己的视野
	s.handleMessage(presenter, ws.Message{Type: ws.MsgViewport, Position: 42, ScrollTop: 800, ScrollLeft: 3})

	// 跟随者发起跟随
	s.handleMessage(follower, ws.Message{Type: ws.MsgFollow, TargetID: presenter.ClientID})

	// 跟随者应收到 ack，且附带被跟随者当前视野快照
	ack := recvMsg(t, follower, "follower")
	if ack.Type != ws.MsgFollowAck {
		t.Fatalf("expected follow_ack, got %s", ack.Type)
	}
	if ack.TargetID != presenter.ClientID {
		t.Fatalf("ack target = %s, want %s", ack.TargetID, presenter.ClientID)
	}
	if ack.Username != "Presenter" {
		t.Fatalf("ack username = %s, want Presenter", ack.Username)
	}
	if ack.ScrollTop != 800 || ack.ScrollLeft != 3 || ack.Position != 42 {
		t.Fatalf("ack viewport = (%v,%v,%v), want (800,3,42)", ack.ScrollTop, ack.ScrollLeft, ack.Position)
	}
	if ack.Followers != 1 {
		t.Fatalf("ack followers = %d, want 1", ack.Followers)
	}

	// 被跟随者应收到跟随人数通知
	info := recvMsg(t, presenter, "presenter")
	if info.Type != ws.MsgFollowInfo || info.Followers != 1 {
		t.Fatalf("presenter got %v, want follow_info followers=1", info)
	}
}

func TestViewportOnlyRelayedToFollowers(t *testing.T) {
	s, hub := newFollowTestServer()
	presenter := joinClient(t, hub, "room1", "Presenter")
	followerA := joinClient(t, hub, "room1", "FollowerA")
	followerB := joinClient(t, hub, "room1", "FollowerB")
	bystander := joinClient(t, hub, "room1", "Bystander")

	s.handleMessage(followerA, ws.Message{Type: ws.MsgFollow, TargetID: presenter.ClientID})
	s.handleMessage(followerB, ws.Message{Type: ws.MsgFollow, TargetID: presenter.ClientID})
	recvMsg(t, followerA, "followerA") // ack
	recvMsg(t, followerB, "followerB") // ack
	recvMsg(t, presenter, "presenter") // follow_info 1
	recvMsg(t, presenter, "presenter") // follow_info 2

	// 被跟随者上报视野
	s.handleMessage(presenter, ws.Message{Type: ws.MsgViewport, Position: 7, ScrollTop: 1234})

	// 两个跟随者收到完全一致的视野消息（多人跟随看到同样视野）
	msgA := recvMsg(t, followerA, "followerA")
	msgB := recvMsg(t, followerB, "followerB")
	if msgA.Type != ws.MsgViewport || msgB.Type != ws.MsgViewport {
		t.Fatalf("followers should receive viewport, got %s / %s", msgA.Type, msgB.Type)
	}
	if msgA.ScrollTop != 1234 || msgB.ScrollTop != 1234 || msgA.Position != 7 || msgB.Position != 7 {
		t.Fatalf("followers received different viewports: %+v vs %+v", msgA, msgB)
	}
	if msgA.ClientID != presenter.ClientID {
		t.Fatalf("viewport sender = %s, want presenter %s", msgA.ClientID, presenter.ClientID)
	}

	// 旁观者与被跟随者本人不应收到
	expectNoMsg(t, bystander, "bystander")
	expectNoMsg(t, presenter, "presenter")
}

func TestUnfollowNotifiesPresenter(t *testing.T) {
	s, hub := newFollowTestServer()
	presenter := joinClient(t, hub, "room1", "Presenter")
	follower := joinClient(t, hub, "room1", "Follower")

	s.handleMessage(follower, ws.Message{Type: ws.MsgFollow, TargetID: presenter.ClientID})
	recvMsg(t, follower, "follower")   // ack
	recvMsg(t, presenter, "presenter") // follow_info 1

	s.handleMessage(follower, ws.Message{Type: ws.MsgUnfollow})

	info := recvMsg(t, presenter, "presenter")
	if info.Type != ws.MsgFollowInfo || info.Followers != 0 {
		t.Fatalf("presenter got %+v, want follow_info followers=0", info)
	}

	// 解除后视野更新不再转发
	s.handleMessage(presenter, ws.Message{Type: ws.MsgViewport, Position: 1, ScrollTop: 10})
	expectNoMsg(t, follower, "follower")
}

func TestPresenterDisconnectEndsFollow(t *testing.T) {
	s, hub := newFollowTestServer()
	presenter := joinClient(t, hub, "room1", "Presenter")
	follower := joinClient(t, hub, "room1", "Follower")

	s.handleMessage(follower, ws.Message{Type: ws.MsgFollow, TargetID: presenter.ClientID})
	recvMsg(t, follower, "follower")   // ack
	recvMsg(t, presenter, "presenter") // follow_info

	// 被跟随者断线
	hub.Unregister <- presenter

	// 跟随者必须先收到 follow_end，再收到 user_leave（保证提示顺序）
	first := recvMsg(t, follower, "follower")
	if first.Type != ws.MsgFollowEnd {
		t.Fatalf("first message = %s, want follow_end", first.Type)
	}
	if first.TargetID != presenter.ClientID || first.Reason != "disconnect" {
		t.Fatalf("follow_end = %+v, want target=%s reason=disconnect", first, presenter.ClientID)
	}
	second := recvMsg(t, follower, "follower")
	if second.Type != ws.MsgUserLeave {
		t.Fatalf("second message = %s, want user_leave", second.Type)
	}
}

func TestFollowerDisconnectUpdatesPresenterCount(t *testing.T) {
	s, hub := newFollowTestServer()
	presenter := joinClient(t, hub, "room1", "Presenter")
	follower := joinClient(t, hub, "room1", "Follower")

	s.handleMessage(follower, ws.Message{Type: ws.MsgFollow, TargetID: presenter.ClientID})
	recvMsg(t, follower, "follower")   // ack
	recvMsg(t, presenter, "presenter") // follow_info 1

	// 跟随者断线，被跟随者应收到人数归零通知
	hub.Unregister <- follower
	info := recvMsg(t, presenter, "presenter")
	if info.Type != ws.MsgFollowInfo || info.Followers != 0 {
		t.Fatalf("presenter got %+v, want follow_info followers=0", info)
	}
}

func TestFollowValidation(t *testing.T) {
	s, hub := newFollowTestServer()
	user := joinClient(t, hub, "room1", "User")

	// 跟随自己
	s.handleMessage(user, ws.Message{Type: ws.MsgFollow, TargetID: user.ClientID})
	if msg := recvMsg(t, user, "user"); msg.Type != ws.MsgError {
		t.Fatalf("follow self: got %s, want error", msg.Type)
	}

	// 跟随不存在的人
	s.handleMessage(user, ws.Message{Type: ws.MsgFollow, TargetID: "no-such-client"})
	if msg := recvMsg(t, user, "user"); msg.Type != ws.MsgError {
		t.Fatalf("follow missing: got %s, want error", msg.Type)
	}
}

func TestSwitchFollowTarget(t *testing.T) {
	s, hub := newFollowTestServer()
	p1 := joinClient(t, hub, "room1", "P1")
	p2 := joinClient(t, hub, "room1", "P2")
	follower := joinClient(t, hub, "room1", "Follower")

	s.handleMessage(follower, ws.Message{Type: ws.MsgFollow, TargetID: p1.ClientID})
	recvMsg(t, follower, "follower") // ack(p1)
	recvMsg(t, p1, "p1")             // follow_info 1

	// 换跟随 p2：p1 人数归零，p2 人数为 1
	s.handleMessage(follower, ws.Message{Type: ws.MsgFollow, TargetID: p2.ClientID})
	ack := recvMsg(t, follower, "follower")
	if ack.Type != ws.MsgFollowAck || ack.TargetID != p2.ClientID {
		t.Fatalf("ack = %+v, want follow_ack target=p2", ack)
	}
	info1 := recvMsg(t, p1, "p1")
	if info1.Type != ws.MsgFollowInfo || info1.Followers != 0 {
		t.Fatalf("p1 got %+v, want follow_info followers=0", info1)
	}
	info2 := recvMsg(t, p2, "p2")
	if info2.Type != ws.MsgFollowInfo || info2.Followers != 1 {
		t.Fatalf("p2 got %+v, want follow_info followers=1", info2)
	}

	// 旧目标的视野更新不再转发给跟随者
	s.handleMessage(p1, ws.Message{Type: ws.MsgViewport, Position: 1, ScrollTop: 1})
	expectNoMsg(t, follower, "follower")
}
