// Package ai forwards chat requests from sites to a model provider using keys held by
// the server, so pages never see an API key.
package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

type Message struct {
	Role    string `json:"role"` // "user" or "assistant"
	Content string `json:"content"`
}

type Request struct {
	Model     string
	System    string
	Messages  []Message
	MaxTokens int64
	Effort    string // optional: low, medium, high, xhigh, max
}

type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// Total is what counts against a user's budget: everything read, cached or not, plus everything written.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

type Result struct {
	Text       string `json:"text"`
	Model      string `json:"model"`       // the model that answered, which a refusal fallback can change
	StopReason string `json:"stop_reason"` // end_turn, max_tokens, refusal, ...
	Usage      Usage  `json:"usage"`
	// Complete reports whether the provider sent final usage. Without it, OutputTokens
	// undercounts: generation (including unseen thinking) may have run past what arrived.
	Complete bool `json:"-"`
}

// Provider generates a reply, calling onText with each piece of text as it arrives.
type Provider interface {
	Stream(ctx context.Context, req Request, onText func(string) error) (*Result, error)
}

// Error is a provider failure, already mapped to the status eze should answer with.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

var Efforts = []string{"low", "medium", "high", "xhigh", "max"}

// Anthropic calls the Claude Messages API with the official SDK.
type Anthropic struct {
	client anthropic.Client
}

func NewAnthropic(apiKey, baseURL string, opts ...option.RequestOption) *Anthropic {
	opts = append([]option.RequestOption{option.WithAPIKey(apiKey)}, opts...)
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &Anthropic{client: anthropic.NewClient(opts...)}
}

// Models that accept server-side refusal fallbacks in the "default" form.
var fallbackModels = []string{"claude-fable-5-1", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5"}

func (a *Anthropic) Stream(ctx context.Context, req Request, onText func(string) error) (*Result, error) {
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(req.Model),
		MaxTokens: req.MaxTokens,
		Messages:  make([]anthropic.BetaMessageParam, 0, len(req.Messages)),
	}
	for _, m := range req.Messages {
		role := anthropic.BetaMessageParamRoleUser
		if m.Role == "assistant" {
			role = anthropic.BetaMessageParamRoleAssistant
		}
		params.Messages = append(params.Messages, anthropic.BetaMessageParam{
			Role:    role,
			Content: []anthropic.BetaContentBlockParamUnion{anthropic.NewBetaTextBlock(m.Content)},
		})
	}
	if req.System != "" {
		params.System = []anthropic.BetaTextBlockParam{{Text: req.System}}
	}
	if req.Effort != "" {
		params.OutputConfig = anthropic.BetaOutputConfigParam{Effort: anthropic.BetaOutputConfigEffort(req.Effort)}
	}
	// If a safety classifier declines, let the API re-serve the request on a suitable model
	// instead of the page getting an empty reply.
	if slices.Contains(fallbackModels, req.Model) {
		params.Betas = []anthropic.AnthropicBeta{anthropic.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
	}

	// Always stream upstream: long replies would otherwise hit HTTP timeouts.
	stream := a.client.Beta.Messages.NewStreaming(ctx, params)
	defer stream.Close()
	msg := anthropic.BetaMessage{}
	for stream.Next() {
		event := stream.Current()
		if err := msg.Accumulate(event); err != nil {
			return nil, err
		}
		if delta, ok := event.AsAny().(anthropic.BetaRawContentBlockDeltaEvent); ok {
			if text, ok := delta.Delta.AsAny().(anthropic.BetaTextDelta); ok && text.Text != "" {
				if err := onText(text.Text); err != nil {
					return resultOf(&msg), err
				}
			}
		}
	}
	if err := stream.Err(); err != nil {
		return resultOf(&msg), mapError(err)
	}
	return resultOf(&msg), nil
}

func resultOf(msg *anthropic.BetaMessage) *Result {
	var text strings.Builder
	for _, block := range msg.Content {
		if t, ok := block.AsAny().(anthropic.BetaTextBlock); ok {
			text.WriteString(t.Text)
		}
	}
	u := msg.Usage
	return &Result{
		Text:       text.String(),
		Model:      string(msg.Model),
		StopReason: string(msg.StopReason),
		Complete:   msg.StopReason != "",
		Usage: Usage{
			InputTokens:  u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens,
			OutputTokens: u.OutputTokens,
		},
	}
}

// mapError turns Anthropic SDK errors into statuses that make sense to a page.
func mapError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var apierr *anthropic.Error
	if !errors.As(err, &apierr) {
		return &Error{Status: http.StatusBadGateway, Message: "could not reach the AI provider"}
	}
	return statusError(apierr.StatusCode, providerMessage(apierr.RawJSON(), apierr.StatusCode))
}

// statusError maps a provider's HTTP status: problems with the request are the page's to
// fix (400), everything else is the server's (502/503).
func statusError(status int, message string) *Error {
	switch status {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusRequestEntityTooLarge, http.StatusUnprocessableEntity:
		return &Error{Status: http.StatusBadRequest, Message: "the AI provider rejected the request: " + message}
	case http.StatusTooManyRequests, 529:
		return &Error{Status: http.StatusServiceUnavailable, Message: "the AI provider is busy, try again shortly"}
	case http.StatusUnauthorized, http.StatusForbidden:
		return &Error{Status: http.StatusBadGateway, Message: "the server's AI credentials were rejected; tell whoever runs eze"}
	default:
		return &Error{Status: http.StatusBadGateway, Message: "the AI provider failed"}
	}
}

// providerMessage extracts the human-readable message from an API error body, which is
// either the whole response ({"error":{"message":...}}) or just its error object.
func providerMessage(raw string, status int) string {
	var body struct {
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(raw), &body) == nil {
		if body.Error.Message != "" {
			return body.Error.Message
		}
		if body.Message != "" {
			return body.Message
		}
	}
	return http.StatusText(status)
}
