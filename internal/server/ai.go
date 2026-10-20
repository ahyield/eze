package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
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

// validate fills in defaults and reports what's wrong with the request, if anything.
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
	// The check is before the call, so one request can run past the budget; the next one is refused.
	if used >= s.cfg.AIDailyTokens {
		writeErr(w, http.StatusTooManyRequests, fmt.Sprintf("daily AI budget of %d tokens used up; it resets at %s",
			s.cfg.AIDailyTokens, nextUTCMidnight(now).Format(time.RFC3339)))
		return
	}

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

	if res != nil && res.Usage.Total() > 0 {
		// Record even when the request failed or was abandoned: the tokens were spent.
		if rerr := s.store.RecordAIUsage(context.WithoutCancel(r.Context()), user.Email, site, now, res.Usage.InputTokens, res.Usage.OutputTokens); rerr != nil {
			slog.Error("recording AI usage", "err", rerr)
		}
		slog.Info("ai", "site", site, "by", user.Email, "model", res.Model, "in", res.Usage.InputTokens, "out", res.Usage.OutputTokens, "stop", res.StopReason)
	}
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
