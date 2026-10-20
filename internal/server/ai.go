package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ahyield/eze/internal/ai"
	"github.com/ahyield/eze/internal/auth"
)

const (
	maxAIRequestBytes = 4 << 20
	maxAIMessages     = 500
)

type chatRequest struct {
	Model     string       `json:"model"`
	System    string       `json:"system"`
	Messages  []ai.Message `json:"messages"`
	MaxTokens int64        `json:"max_tokens"`
	Effort    string       `json:"effort"`
	Stream    bool         `json:"stream"`
}

func (s *Server) aiEnabled(w http.ResponseWriter) bool {
	if s.ai == nil {
		writeErr(w, http.StatusServiceUnavailable, "AI is not configured on this server (EZE_ANTHROPIC_API_KEY)")
		return false
	}
	return true
}

// validateChat fills in defaults and reports what's wrong with the request, if anything.
func (s *Server) validateChat(req *chatRequest) string {
	if req.Model == "" {
		req.Model = s.cfg.AIDefaultModel
	}
	if !slices.Contains(s.cfg.AIModels, req.Model) {
		return fmt.Sprintf("model %q is not enabled; choose one of %v", req.Model, s.cfg.AIModels)
	}
	if req.MaxTokens == 0 {
		req.MaxTokens = s.cfg.AIMaxOutputTokens
	}
	if req.MaxTokens < 1 || req.MaxTokens > s.cfg.AIMaxOutputTokens {
		return fmt.Sprintf("max_tokens must be between 1 and %d", s.cfg.AIMaxOutputTokens)
	}
	if req.Effort != "" && !slices.Contains(ai.Efforts, req.Effort) {
		return fmt.Sprintf("effort must be one of %v", ai.Efforts)
	}
	if len(req.Messages) == 0 || len(req.Messages) > maxAIMessages {
		return fmt.Sprintf("messages must have between 1 and %d entries", maxAIMessages)
	}
	for i, m := range req.Messages {
		if m.Role != "user" && m.Role != "assistant" {
			return fmt.Sprintf("messages[%d].role must be \"user\" or \"assistant\"", i)
		}
		if m.Content == "" {
			return fmt.Sprintf("messages[%d].content must be a non-empty string", i)
		}
	}
	if req.Messages[0].Role != "user" {
		return "the first message must be from the user"
	}
	return ""
}

// estimateInputTokens over-estimates the prompt's size: roughly a token per 3 bytes, which
// covers English (about 4 bytes a token) and most other scripts, plus per-message overhead.
func estimateInputTokens(req *chatRequest) int64 {
	n := int64(len(req.System))
	for _, m := range req.Messages {
		n += int64(len(m.Content)) + 30
	}
	return n/3 + 50
}

var (
	errAIBusy   = errors.New("too many AI calls in flight")
	errAIBudget = errors.New("over budget")
)

// aiLedger tracks each person's AI calls in flight and the tokens reserved for them.
type aiLedger struct {
	mu       sync.Mutex
	inflight map[string]int
	reserved map[string]int64
}

func newAILedger() *aiLedger {
	return &aiLedger{inflight: map[string]int{}, reserved: map[string]int64{}}
}

func (l *aiLedger) reserve(email string, used, limit, amount int64, maxConcurrent int) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inflight[email] >= maxConcurrent {
		return errAIBusy
	}
	if used+l.reserved[email]+amount > limit {
		return errAIBudget
	}
	l.inflight[email]++
	l.reserved[email] += amount
	return nil
}

func (l *aiLedger) release(email string, amount int64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.inflight[email]--
	l.reserved[email] -= amount
	if l.inflight[email] == 0 {
		delete(l.inflight, email)
		delete(l.reserved, email)
	}
}

func (s *Server) budgetMessage(err error, used, reservation int64, now time.Time) string {
	resets := nextUTCMidnight(now).Format(time.RFC3339)
	switch {
	case errors.Is(err, errAIBusy):
		return fmt.Sprintf("you already have %d AI requests running; wait for one to finish", s.cfg.AIMaxConcurrent)
	case used >= s.cfg.AIDailyTokens:
		return fmt.Sprintf("daily AI budget of %d tokens used up; it resets at %s", s.cfg.AIDailyTokens, resets)
	default:
		return fmt.Sprintf("this request could use up to %d tokens, more than is left of today's budget "+
			"(%d of %d used, including requests still running); send less or lower max_tokens. It resets at %s",
			reservation, used, s.cfg.AIDailyTokens, resets)
	}
}

func nextUTCMidnight(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d+1, 0, 0, 0, 0, time.UTC)
}

func (s *Server) handleAIUsage(w http.ResponseWriter, r *http.Request) {
	now := time.Now()
	used, err := s.store.AITokensUsed(r.Context(), auth.UserFrom(r.Context()).Email, now)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled":        s.ai != nil,
		"used":           used,
		"limit":          s.cfg.AIDailyTokens,
		"resets_at":      nextUTCMidnight(now),
		"models":         s.cfg.AIModels,
		"default_model":  s.cfg.AIDefaultModel,
		"max_tokens_cap": s.cfg.AIMaxOutputTokens,
	})
}

func (s *Server) handleAIChat(w http.ResponseWriter, r *http.Request) {
	if !s.aiEnabled(w) {
		return
	}
	var req chatRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxAIRequestBytes)).Decode(&req); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeErr(w, http.StatusRequestEntityTooLarge, "request too large")
		} else {
			writeErr(w, http.StatusBadRequest, "body must be JSON: {messages: [{role, content}], ...}")
		}
		return
	}
	if msg := s.validateChat(&req); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}

	user := auth.UserFrom(r.Context())
	site := siteFrom(r.Context())
	now := time.Now()
	used, err := s.store.AITokensUsed(r.Context(), user.Email, now)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	// Reserve the most this call could cost before making it, so parallel requests can't
	// each pass the budget check and overspend together.
	estimate := estimateInputTokens(&req)
	reservation := estimate + req.MaxTokens
	if err := s.aiLedger.reserve(user.Email, used, s.cfg.AIDailyTokens, reservation, s.cfg.AIMaxConcurrent); err != nil {
		writeErr(w, http.StatusTooManyRequests, s.budgetMessage(err, used, reservation, now))
		return
	}
	defer s.aiLedger.release(user.Email, reservation)

	aiReq := ai.Request{Model: req.Model, System: req.System, Messages: req.Messages, MaxTokens: req.MaxTokens, Effort: req.Effort}
	// The request context is cancelled when the browser goes away, which stops the upstream
	// call too: an abandoned tab doesn't keep spending tokens.
	var res *ai.Result
	if req.Stream {
		res, err = s.streamChat(w, r, aiReq)
	} else {
		res, err = s.ai.Stream(r.Context(), aiReq, func(string) error { return nil })
		if err == nil {
			writeJSON(w, http.StatusOK, res)
		} else {
			s.writeAIErr(w, r, err)
		}
	}

	if res == nil || res.Usage.Total() == 0 {
		return // the provider never started the reply, so nothing was billed
	}
	charged := res.Usage
	if !res.Complete {
		// Stopped early, by the browser or a failure: final usage never arrived, and output
		// (including thinking that is never streamed) may have run past what we saw.
		// Charge the reservation so an abort can't be used to spend for free.
		charged = ai.Usage{InputTokens: max(charged.InputTokens, estimate), OutputTokens: req.MaxTokens}
	}
	if rerr := s.store.RecordAIUsage(context.WithoutCancel(r.Context()), user.Email, site, now, charged.InputTokens, charged.OutputTokens); rerr != nil {
		slog.Error("recording AI usage", "err", rerr)
	}
	slog.Info("ai", "site", site, "by", user.Email, "model", res.Model, "in", charged.InputTokens, "out", charged.OutputTokens, "stop", res.StopReason, "complete", res.Complete)
}

func (s *Server) writeAIErr(w http.ResponseWriter, r *http.Request, err error) {
	var aerr *ai.Error
	switch {
	case errors.As(err, &aerr):
		writeErr(w, aerr.Status, aerr.Message)
	case errors.Is(err, context.Canceled):
		// The client left; nobody is listening.
	default:
		s.internalErr(w, r, err)
	}
}

// streamChat relays the reply as server-sent events:
//
//	event: text   data: {"text": "..."}          one per chunk
//	event: done   data: {text, model, stop_reason, usage}
//	event: error  data: {"error": "...", "status": 503}
func (s *Server) streamChat(w http.ResponseWriter, r *http.Request, req ai.Request) (*ai.Result, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.internalErr(w, r, errors.New("response writer cannot flush"))
		return nil, nil
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // tell nginx not to buffer the stream
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	send := func(event string, v any) error {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}

	res, err := s.ai.Stream(r.Context(), req, func(text string) error {
		return send("text", map[string]string{"text": text})
	})
	if err != nil {
		var aerr *ai.Error
		if errors.As(err, &aerr) {
			send("error", map[string]any{"error": aerr.Message, "status": aerr.Status})
		} else if !errors.Is(err, context.Canceled) {
			slog.Error("ai stream", "err", err)
			send("error", map[string]any{"error": "internal error", "status": http.StatusInternalServerError})
		}
		return res, err
	}
	send("done", res)
	return res, nil
}
