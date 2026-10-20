package server

import (
	"encoding/json"
	"strings"
	"sync"
)

// hub fans out document changes to websocket subscribers of a site's collection.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[*subscriber]struct{} // key: site + "/" + collection
}

// subscriber is only ever in the subs map while its channel is open; closed guards
// against closing twice when both the hub and the connection drop it.
type subscriber struct {
	ch     chan []byte
	closed bool
}

type event struct {
	Type string `json:"type"` // "create", "update" or "delete"
	Doc  any    `json:"doc,omitempty"`
	ID   string `json:"id,omitempty"`
}

func newHub() *hub { return &hub{subs: map[string]map[*subscriber]struct{}{}} }

func hubKey(site, collection string) string { return site + "/" + collection }

func (h *hub) subscribe(site, collection string) *subscriber {
	s := &subscriber{ch: make(chan []byte, 64)}
	h.mu.Lock()
	defer h.mu.Unlock()
	k := hubKey(site, collection)
	if h.subs[k] == nil {
		h.subs[k] = map[*subscriber]struct{}{}
	}
	h.subs[k][s] = struct{}{}
	return s
}

func (h *hub) unsubscribe(site, collection string, s *subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	k := hubKey(site, collection)
	if _, ok := h.subs[k][s]; !ok {
		return
	}
	delete(h.subs[k], s)
	if len(h.subs[k]) == 0 {
		delete(h.subs, k)
	}
	if !s.closed {
		s.closed = true
		close(s.ch)
	}
}

func (h *hub) publish(site, collection string, ev event) {
	msg, err := json.Marshal(ev)
	if err != nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	set := h.subs[hubKey(site, collection)]
	for s := range set {
		select {
		case s.ch <- msg:
		default:
			// Too slow to keep up: drop it so the client reconnects and refetches.
			delete(set, s)
			s.closed = true
			close(s.ch)
		}
	}
}

// dropSite disconnects every subscriber of a deleted site.
func (h *hub) dropSite(site string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	prefix := site + "/"
	for k, set := range h.subs {
		if strings.HasPrefix(k, prefix) {
			for s := range set {
				s.closed = true
				close(s.ch)
			}
			delete(h.subs, k)
		}
	}
}
