package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ahyield/eze/internal/auth"
)

type roomMsg struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	From  string          `json:"from"`
	Data  json.RawMessage `json:"data"`
	State json.RawMessage `json:"state"`
	Peer  peerInfo        `json:"peer"`
	Peers []peerInfo      `json:"peers"`
	Error string          `json:"error"`
}

// recv takes the next queued message for p, failing if there is none.
func recv(t *testing.T, p *peer) roomMsg {
	t.Helper()
	select {
	case b, ok := <-p.send:
		if !ok {
			t.Fatal("peer channel closed")
		}
		var m roomMsg
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	default:
		t.Fatal("no message queued")
	}
	return roomMsg{}
}

func empty(p *peer) bool { return len(p.send) == 0 }

func joinT(t *testing.T, rs *rooms, site, room, email string) *peer {
	t.Helper()
	p, err := rs.join(site, room, &auth.User{Email: email, Name: email[:strings.Index(email, "@")]})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRoomJoinWelcomeLeave(t *testing.T) {
	rs := newRooms()
	a := joinT(t, rs, "blog", "lobby", ana)
	if m := recv(t, a); m.Type != "welcome" || m.ID != a.id || len(m.Peers) != 0 {
		t.Fatalf("a welcome = %+v", m)
	}
	rs.setState(a, json.RawMessage(`{"x":1}`))

	b := joinT(t, rs, "blog", "lobby", bo)
	m := recv(t, b)
	if m.Type != "welcome" || len(m.Peers) != 1 || m.Peers[0].ID != a.id || m.Peers[0].User.Email != ana || string(m.Peers[0].State) != `{"x":1}` {
		t.Fatalf("b welcome = %+v", m)
	}
	if m := recv(t, a); m.Type != "join" || m.Peer.ID != b.id || m.Peer.User.Email != bo || string(m.Peer.State) != "null" {
		t.Fatalf("a join = %+v", m)
	}

	rs.leave(b)
	if m := recv(t, a); m.Type != "leave" || m.ID != b.id {
		t.Fatalf("a leave = %+v", m)
	}
	if _, open := <-b.send; open {
		t.Error("left peer's channel still open")
	}
	rs.leave(b) // twice is harmless
	rs.leave(a)
	if len(rs.rooms) != 0 {
		t.Errorf("empty room not removed: %v", rs.rooms)
	}
}

func TestRoomMessagesAndPresence(t *testing.T) {
	rs := newRooms()
	a := joinT(t, rs, "blog", "lobby", ana)
	b := joinT(t, rs, "blog", "lobby", bo)
	c := joinT(t, rs, "blog", "lobby", "cy@example.com")
	for _, p := range []*peer{a, b, c} {
		for !empty(p) {
			<-p.send // welcomes and joins
		}
	}

	rs.message(a, "", json.RawMessage(`"hi"`))
	if !empty(a) {
		t.Error("sender received its own broadcast")
	}
	for _, p := range []*peer{b, c} {
		if m := recv(t, p); m.Type != "message" || m.From != a.id || string(m.Data) != `"hi"` {
			t.Errorf("broadcast = %+v", m)
		}
	}

	if !rs.message(a, c.id, json.RawMessage(`{"offer":1}`)) {
		t.Fatal("direct message to present peer failed")
	}
	if m := recv(t, c); m.From != a.id || string(m.Data) != `{"offer":1}` {
		t.Errorf("direct = %+v", m)
	}
	if !empty(b) {
		t.Error("direct message leaked to another peer")
	}
	if rs.message(a, "nobody", json.RawMessage(`1`)) || rs.message(a, a.id, json.RawMessage(`1`)) {
		t.Error("message to unknown peer or self should report false")
	}

	rs.setState(b, json.RawMessage(`{"x":5}`))
	if !empty(b) {
		t.Error("presence echoed to its sender")
	}
	for _, p := range []*peer{a, c} {
		if m := recv(t, p); m.Type != "presence" || m.ID != b.id || string(m.State) != `{"x":5}` {
			t.Errorf("presence = %+v", m)
		}
	}
}

func TestRoomsIsolated(t *testing.T) {
	rs := newRooms()
	a := joinT(t, rs, "blog", "lobby", ana)
	<-a.send
	joinT(t, rs, "blog", "other-room", bo)
	joinT(t, rs, "other-site", "lobby", bo)
	if !empty(a) {
		t.Error("join leaked across rooms or sites")
	}
}

func TestRoomFull(t *testing.T) {
	rs := newRooms()
	for range maxRoomPeers { // a welcome plus up to 199 joins fits each peer's buffer, so nobody is dropped
		joinT(t, rs, "blog", "lobby", ana)
	}
	if _, err := rs.join("blog", "lobby", &auth.User{Email: bo}); err != errRoomFull {
		t.Errorf("join full room = %v", err)
	}
	if _, err := rs.join("blog", "other", &auth.User{Email: bo}); err != nil {
		t.Errorf("other room affected: %v", err)
	}
}

func TestRoomDropsSlowPeer(t *testing.T) {
	rs := newRooms()
	a := joinT(t, rs, "blog", "lobby", ana)
	fast := joinT(t, rs, "blog", "lobby", bo)
	slow := joinT(t, rs, "blog", "lobby", "cy@example.com")

	drain := func(p *peer) {
		for !empty(p) {
			<-p.send
		}
	}
	drain(a)
	drain(fast)
	for range peerSendBuffer + 1 {
		rs.message(a, "", json.RawMessage(`1`))
		drain(fast)
	}
	// slow's buffer overflowed: it was removed, its channel closed, and the others told.
	n := 0
	for range slow.send {
		n++
	}
	if n != peerSendBuffer { // its welcome plus the messages that fit
		t.Errorf("slow peer had %d queued messages, want %d", n, peerSendBuffer)
	}
	if m := recv(t, a); m.Type != "leave" || m.ID != slow.id {
		t.Errorf("a = %+v, want leave of slow peer", m)
	}
	rs.message(a, "", json.RawMessage(`2`)) // must not panic sending to the closed channel
	if m := recv(t, fast); m.Type != "message" {
		t.Errorf("fast peer = %+v", m)
	}
}

func TestRoomsDropSite(t *testing.T) {
	rs := newRooms()
	a := joinT(t, rs, "blog", "lobby", ana)
	b := joinT(t, rs, "blog", "chat", bo)
	other := joinT(t, rs, "other", "lobby", bo)
	rs.dropSite("blog")
	for _, p := range []*peer{a, b} {
		for range p.send {
		}
	}
	rs.leave(a) // already closed: no double close
	rs.message(b, "", json.RawMessage(`1`))
	if _, ok := rs.rooms[roomKey("other", "lobby")]; !ok {
		t.Error("other site's room dropped")
	}
	if m := recv(t, other); m.Type != "welcome" {
		t.Errorf("other = %+v", m)
	}
}

// waitPeers waits until the server's rooms hold n peers in total.
func waitPeers(t *testing.T, e *env, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e.srv.rooms.mu.Lock()
		got := 0
		for _, members := range e.srv.rooms.rooms {
			got += len(members)
		}
		e.srv.rooms.mu.Unlock()
		if got == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("rooms hold %d peers, want %d", got, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func serve(t *testing.T, e *env) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(e.srv)
	t.Cleanup(ts.Close)
	return ts
}

func dialRoom(t *testing.T, e *env, client *http.Client, site, room, as string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://"+site+".eze.test/_eze/ws/"+room, &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"X-Email": {as}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() })
	return conn
}

func read(t *testing.T, conn *websocket.Conn) roomMsg {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, b, err := conn.Read(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var m roomMsg
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func write(t *testing.T, conn *websocket.Conn, s string) {
	t.Helper()
	if err := conn.Write(context.Background(), websocket.MessageText, []byte(s)); err != nil {
		t.Fatal(err)
	}
}

func TestRoomOverWebsocket(t *testing.T) {
	e := newEnv(t)
	client := dialer(serve(t, e))

	a := dialRoom(t, e, client, "blog", "lobby", ana)
	welcomeA := read(t, a)
	if welcomeA.Type != "welcome" || len(welcomeA.Peers) != 0 {
		t.Fatalf("a welcome = %+v", welcomeA)
	}
	write(t, a, `{"type":"presence","state":{"x":10}}`)

	b := dialRoom(t, e, client, "blog", "lobby", bo)
	welcomeB := read(t, b)
	if len(welcomeB.Peers) != 1 || welcomeB.Peers[0].User.Email != ana || string(welcomeB.Peers[0].State) != `{"x":10}` {
		t.Fatalf("b welcome = %+v", welcomeB)
	}
	if m := read(t, a); m.Type != "join" || m.Peer.User.Email != bo {
		t.Fatalf("a join = %+v", m)
	}

	write(t, b, `{"type":"message","data":{"hello":"world"}}`)
	if m := read(t, a); m.Type != "message" || m.From != welcomeB.ID || string(m.Data) != `{"hello":"world"}` {
		t.Errorf("a message = %+v", m)
	}
	write(t, a, `{"type":"message","to":"`+welcomeB.ID+`","data":"direct"}`)
	if m := read(t, b); m.From != welcomeA.ID || string(m.Data) != `"direct"` {
		t.Errorf("b direct = %+v", m)
	}

	write(t, a, `{"type":"message","to":"ghost","data":1}`)
	if m := read(t, a); m.Type != "error" || !strings.Contains(m.Error, "ghost") {
		t.Errorf("unknown peer = %+v", m)
	}
	write(t, a, `not json`)
	if m := read(t, a); m.Type != "error" {
		t.Errorf("bad json = %+v", m)
	}
	write(t, a, `{"type":"shout"}`)
	if m := read(t, a); m.Type != "error" {
		t.Errorf("unknown type = %+v", m)
	}

	b.Close(websocket.StatusNormalClosure, "")
	if m := read(t, a); m.Type != "leave" || m.ID != welcomeB.ID {
		t.Errorf("a leave = %+v", m)
	}
}

func TestRoomWebsocketRejects(t *testing.T) {
	e := newEnv(t)
	ts := serve(t, e)
	client := dialer(ts)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	watcher := dialRoom(t, e, client, "blog", "lobby", bo)
	read(t, watcher) // welcome
	_, res, err := websocket.Dial(ctx, "ws://blog.eze.test/_eze/ws/lobby", &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"X-Email": {ana}, "Origin": {"https://evil.eze.test"}},
	})
	if err == nil || (res != nil && res.StatusCode != http.StatusForbidden) {
		t.Errorf("cross-origin join: err=%v", err)
	}
	// The rejected viewer must never have been announced to the room.
	rctx, rcancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer rcancel()
	if _, msg, err := watcher.Read(rctx); err == nil {
		t.Errorf("room saw a rejected cross-origin viewer: %s", msg)
	}
	watcher.Close(websocket.StatusNormalClosure, "")
	waitPeers(t, e, 0)
	_, res, err = websocket.Dial(ctx, "ws://blog.eze.test/_eze/ws/lobby", &websocket.DialOptions{HTTPClient: client})
	if err == nil || (res != nil && res.StatusCode != http.StatusUnauthorized) {
		t.Errorf("anonymous join: err=%v", err)
	}
	if w := e.do(req{url: "https://blog.eze.test/_eze/ws/bad.room", as: ana}); w.Code != 400 {
		t.Errorf("bad room name = %d", w.Code)
	}
	// A failed upgrade must not leave a ghost peer behind.
	if w := e.do(req{url: "https://blog.eze.test/_eze/ws/lobby", as: ana}); w.Code == http.StatusSwitchingProtocols {
		t.Fatal("plain GET upgraded")
	}
	waitPeers(t, e, 0)
}

func TestRoomMessageTooLarge(t *testing.T) {
	e := newEnv(t)
	client := dialer(serve(t, e))
	a := dialRoom(t, e, client, "blog", "lobby", ana)
	read(t, a)
	write(t, a, `{"type":"message","data":"`+strings.Repeat("x", maxRoomMessage)+`"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := a.Read(ctx); websocket.CloseStatus(err) != websocket.StatusMessageTooBig {
		t.Errorf("oversized message: %v", err)
	}
}

func TestRoomsDroppedWithSite(t *testing.T) {
	e := newEnv(t)
	client := dialer(serve(t, e))
	e.deploy("blog", ana, map[string]string{"index.html": "x"})
	a := dialRoom(t, e, client, "blog", "lobby", ana)
	read(t, a)
	if w := e.do(req{method: "DELETE", url: "https://eze.test/_eze/sites/blog", as: ana}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := a.Read(ctx); websocket.CloseStatus(err) != websocket.StatusTryAgainLater {
		t.Errorf("after site delete: %v", err)
	}
}
