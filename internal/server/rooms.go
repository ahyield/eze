package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
	"golang.org/x/time/rate"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/store"
)

const (
	maxRoomPeers   = 200
	maxRoomMessage = 64 << 10 // bytes per websocket message, in either direction
	peerSendBuffer = 256
	peerMsgsPerSec = 50
	peerMsgsBurst  = 100
)

var errRoomFull = errors.New("room is full")

// rooms relays messages between the peers in each site's rooms. Nothing is stored: a room
// exists while someone is in it.
type rooms struct {
	mu    sync.Mutex
	rooms map[string]map[string]*peer // key site + "/" + room, then peer id
}

type peer struct {
	id    string
	key   string
	user  *auth.User
	state json.RawMessage
	send  chan []byte
	// closed is guarded by rooms.mu; send is closed exactly once, when the peer leaves the room.
	closed bool
}

// peerInfo is how a peer is described to others.
type peerInfo struct {
	ID    string          `json:"id"`
	User  *auth.User      `json:"user"`
	State json.RawMessage `json:"state"`
}

func (p *peer) info() peerInfo {
	state := p.state
	if state == nil {
		state = json.RawMessage("null")
	}
	return peerInfo{ID: p.id, User: p.user, State: state}
}

func newRooms() *rooms { return &rooms{rooms: map[string]map[string]*peer{}} }

func roomKey(site, room string) string { return site + "/" + room }

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only ever called with plain structs and json.RawMessage
	}
	return b
}

// join adds a peer. Its first message is a welcome listing everyone already there; the
// others are told it joined.
func (rs *rooms) join(site, room string, user *auth.User) (*peer, error) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	key := roomKey(site, room)
	members := rs.rooms[key]
	if len(members) >= maxRoomPeers {
		return nil, errRoomFull
	}
	if members == nil {
		members = map[string]*peer{}
		rs.rooms[key] = members
	}
	p := &peer{id: store.NewID()[:12], key: key, user: user, send: make(chan []byte, peerSendBuffer)}

	// Announce first: that can drop slow peers, and the welcome must not list them.
	rs.fanoutLocked(members, "", mustJSON(map[string]any{"type": "join", "peer": p.info()}))
	others := make([]peerInfo, 0, len(members))
	for _, o := range members {
		others = append(others, o.info())
	}
	p.send <- mustJSON(map[string]any{"type": "welcome", "id": p.id, "peers": others})
	members[p.id] = p
	rs.rooms[key] = members // removing the last slow peer may have deleted the room entry
	return p, nil
}

func (rs *rooms) leave(p *peer) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	rs.removeLocked(p)
}

// setState records a peer's presence state and tells the others.
func (rs *rooms) setState(p *peer, state json.RawMessage) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if p.closed {
		return
	}
	p.state = state
	rs.fanoutLocked(rs.rooms[p.key], p.id, mustJSON(map[string]any{"type": "presence", "id": p.id, "state": state}))
}

// message relays data from p to everyone else in the room, or only to peer `to`.
// It reports false if `to` isn't in the room.
func (rs *rooms) message(p *peer, to string, data json.RawMessage) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if p.closed {
		return true
	}
	msg := mustJSON(map[string]any{"type": "message", "from": p.id, "data": data})
	members := rs.rooms[p.key]
	if to == "" {
		rs.fanoutLocked(members, p.id, msg)
		return true
	}
	target, ok := members[to]
	if !ok || target == p {
		return false
	}
	rs.fanoutLocked(map[string]*peer{to: target}, "", msg)
	return true
}

// fanoutLocked sends msg to members except skip. Peers whose buffer is full are too slow
// to keep up: they are removed, which in turn tells the rest they left.
func (rs *rooms) fanoutLocked(members map[string]*peer, skip string, msg []byte) {
	var slow []*peer
	for id, m := range members {
		if id == skip {
			continue
		}
		select {
		case m.send <- msg:
		default:
			slow = append(slow, m)
		}
	}
	for _, m := range slow {
		rs.removeLocked(m)
	}
}

func (rs *rooms) removeLocked(p *peer) {
	if p.closed {
		return
	}
	p.closed = true
	close(p.send)
	members := rs.rooms[p.key]
	delete(members, p.id)
	if len(members) == 0 {
		delete(rs.rooms, p.key)
		return
	}
	rs.fanoutLocked(members, "", mustJSON(map[string]any{"type": "leave", "id": p.id}))
}

// notify sends a message to p alone, e.g. an error about its own request.
func (rs *rooms) notify(p *peer, msg []byte) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if !p.closed {
		rs.fanoutLocked(map[string]*peer{p.id: p}, "", msg)
	}
}

// dropSite disconnects everyone in a deleted site's rooms.
func (rs *rooms) dropSite(site string) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	prefix := site + "/"
	for key, members := range rs.rooms {
		if !strings.HasPrefix(key, prefix) {
			continue
		}
		for _, p := range members {
			p.closed = true
			close(p.send)
		}
		delete(rs.rooms, key)
	}
}

// handleRoom connects a browser to a room over a websocket.
//
// Client to server:  {"type":"message","data":any,"to"?:peerId}  {"type":"presence","state":any}
// Server to client:  welcome {id, peers}, join {peer}, leave {id}, presence {id, state},
//
//	message {from, data}, error {error}
func (s *Server) handleRoom(w http.ResponseWriter, r *http.Request) {
	room := r.PathValue("room")
	if !store.ValidCollection(room) {
		writeErr(w, http.StatusBadRequest, "room names may only contain letters, digits, - and _ (max 64)")
		return
	}
	p, err := s.rooms.join(siteFrom(r.Context()), room, auth.UserFrom(r.Context()))
	if errors.Is(err, errRoomFull) {
		writeErr(w, http.StatusServiceUnavailable, "room is full")
		return
	}
	// Accept rejects cross-origin upgrades by default, so other sites can't join this site's rooms.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		s.rooms.leave(p)
		return
	}
	defer conn.CloseNow()
	defer s.rooms.leave(p)
	conn.SetReadLimit(maxRoomMessage)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go roomWriter(ctx, cancel, conn, p)

	limiter := rate.NewLimiter(peerMsgsPerSec, peerMsgsBurst)
	for {
		typ, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		// Over the rate, messages are dropped: for cursors and the like, the next one supersedes it.
		if typ != websocket.MessageText || !limiter.Allow() {
			continue
		}
		var in struct {
			Type  string          `json:"type"`
			To    string          `json:"to"`
			Data  json.RawMessage `json:"data"`
			State json.RawMessage `json:"state"`
		}
		if err := json.Unmarshal(data, &in); err != nil {
			s.rooms.notify(p, mustJSON(map[string]string{"type": "error", "error": "messages must be JSON"}))
			continue
		}
		switch in.Type {
		case "message":
			if in.Data == nil {
				in.Data = json.RawMessage("null")
			}
			if !s.rooms.message(p, in.To, in.Data) {
				s.rooms.notify(p, mustJSON(map[string]string{"type": "error", "error": "peer " + in.To + " is not in this room"}))
			}
		case "presence":
			if in.State == nil {
				in.State = json.RawMessage("null")
			}
			s.rooms.setState(p, in.State)
		default:
			s.rooms.notify(p, mustJSON(map[string]string{"type": "error", "error": "unknown message type"}))
		}
	}
}

// roomWriter delivers queued messages and keeps the connection alive. When the room closes
// the peer's queue (it was too slow, or the site was deleted) it ends the connection.
func roomWriter(ctx context.Context, cancel context.CancelFunc, conn *websocket.Conn, p *peer) {
	defer cancel()
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, open := <-p.send:
			if !open {
				conn.Close(websocket.StatusTryAgainLater, "disconnected, please rejoin")
				return
			}
			wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, msg)
			wcancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			pcancel()
			if err != nil {
				return
			}
		}
	}
}
