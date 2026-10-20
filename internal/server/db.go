package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/coder/websocket"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/store"
)

const (
	defaultListLimit = 100
	maxListLimit     = 1000
	maxListOffset    = 100_000
)

// collectionParams validates the {collection} and optional {id} path values.
func collectionParams(w http.ResponseWriter, r *http.Request, withID bool) (collection, id string, ok bool) {
	collection = r.PathValue("collection")
	if !store.ValidCollection(collection) {
		writeErr(w, http.StatusBadRequest, "collection names may only contain letters, digits, - and _ (max 64)")
		return "", "", false
	}
	if withID {
		id = r.PathValue("id")
		if !store.ValidDocID(id) {
			writeErr(w, http.StatusBadRequest, "ids may only contain letters, digits, - and _ (max 128)")
			return "", "", false
		}
	}
	return collection, id, true
}

func (s *Server) decodeDoc(w http.ResponseWriter, r *http.Request) (store.Doc, bool) {
	var doc store.Doc
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxDocBytes)
	if err := json.NewDecoder(body).Decode(&doc); err != nil || doc == nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "document too large")
		} else {
			writeErr(w, http.StatusBadRequest, "body must be a JSON object")
		}
		return nil, false
	}
	return doc, true
}

func (s *Server) handleListDocs(w http.ResponseWriter, r *http.Request) {
	collection, _, ok := collectionParams(w, r, false)
	if !ok {
		return
	}
	q := store.DocQuery{Limit: defaultListLimit}
	params := r.URL.Query()
	if v := params.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListLimit {
			writeErr(w, http.StatusBadRequest, "limit must be between 1 and 1000")
			return
		}
		q.Limit = n
	}
	if v := params.Get("offset"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > maxListOffset {
			writeErr(w, http.StatusBadRequest, "offset must be between 0 and 100000")
			return
		}
		q.Offset = n
	}
	switch params.Get("order") {
	case "", "asc":
	case "desc":
		q.Desc = true
	default:
		writeErr(w, http.StatusBadRequest, "order must be asc or desc")
		return
	}
	if v := params.Get("where"); v != "" {
		if err := json.Unmarshal([]byte(v), &q.Where); err != nil {
			writeErr(w, http.StatusBadRequest, "where must be a JSON object of field equality filters")
			return
		}
	}

	docs, err := s.store.ListDocs(r.Context(), siteFrom(r.Context()), collection, q)
	var bad *store.BadQueryError
	if errors.As(err, &bad) {
		writeErr(w, http.StatusBadRequest, bad.Error())
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, docs)
}

func (s *Server) handleGetDoc(w http.ResponseWriter, r *http.Request) {
	collection, id, ok := collectionParams(w, r, true)
	if !ok {
		return
	}
	doc, err := s.store.GetDoc(r.Context(), siteFrom(r.Context()), collection, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleCreateDoc(w http.ResponseWriter, r *http.Request) {
	collection, _, ok := collectionParams(w, r, false)
	if !ok {
		return
	}
	data, ok := s.decodeDoc(w, r)
	if !ok {
		return
	}
	site := siteFrom(r.Context())
	doc, err := s.store.CreateDoc(r.Context(), site, collection, auth.UserFrom(r.Context()).Email, data)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	s.hub.publish(site, collection, event{Type: "create", Doc: doc})
	writeJSON(w, http.StatusCreated, doc)
}

func (s *Server) handleSetDoc(w http.ResponseWriter, r *http.Request) {
	collection, id, ok := collectionParams(w, r, true)
	if !ok {
		return
	}
	data, ok := s.decodeDoc(w, r)
	if !ok {
		return
	}
	site := siteFrom(r.Context())
	doc, created, err := s.store.SetDoc(r.Context(), site, collection, id, auth.UserFrom(r.Context()).Email, data)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	status, typ := http.StatusOK, "update"
	if created {
		status, typ = http.StatusCreated, "create"
	}
	s.hub.publish(site, collection, event{Type: typ, Doc: doc})
	writeJSON(w, status, doc)
}

func (s *Server) handleUpdateDoc(w http.ResponseWriter, r *http.Request) {
	collection, id, ok := collectionParams(w, r, true)
	if !ok {
		return
	}
	patch, ok := s.decodeDoc(w, r)
	if !ok {
		return
	}
	site := siteFrom(r.Context())
	doc, err := s.store.UpdateDoc(r.Context(), site, collection, id, patch)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	s.hub.publish(site, collection, event{Type: "update", Doc: doc})
	writeJSON(w, http.StatusOK, doc)
}

func (s *Server) handleDeleteDoc(w http.ResponseWriter, r *http.Request) {
	collection, id, ok := collectionParams(w, r, true)
	if !ok {
		return
	}
	site := siteFrom(r.Context())
	err := s.store.DeleteDoc(r.Context(), site, collection, id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "document not found")
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	s.hub.publish(site, collection, event{Type: "delete", ID: id})
	w.WriteHeader(http.StatusNoContent)
}

// handleSubscribe streams create/update/delete events for one collection over a websocket.
func (s *Server) handleSubscribe(w http.ResponseWriter, r *http.Request) {
	collection, _, ok := collectionParams(w, r, false)
	if !ok {
		return
	}
	// Accept rejects cross-origin upgrades by default, so other sites can't subscribe here.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer conn.CloseNow()

	site := siteFrom(r.Context())
	sub := s.hub.subscribe(site, collection)
	defer s.hub.unsubscribe(site, collection, sub)

	// The client never sends anything; CloseRead handles control frames and cancels ctx on close.
	ctx := conn.CloseRead(r.Context())
	ping := time.NewTicker(30 * time.Second)
	defer ping.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case msg, open := <-sub.ch:
			if !open {
				conn.Close(websocket.StatusTryAgainLater, "resubscribe")
				return
			}
			wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := conn.Ping(pctx)
			cancel()
			if err != nil {
				return
			}
		}
	}
}
