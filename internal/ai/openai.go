package ai

import (
	"context"
	"errors"
	"net/http"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/shared"
)

const openAIDefaultBaseURL = "https://api.openai.com/v1/"

// OpenAI calls the Chat Completions API with OpenAI's official SDK. Chat Completions is
// what OpenAI-compatible servers (OpenRouter, Ollama, vLLM, LiteLLM...) implement, so a
// base URL is enough to point it at one of them.
type OpenAI struct {
	client   openai.Client
	official bool // talking to api.openai.com rather than a compatible server
}

func NewOpenAI(apiKey, baseURL string, opts ...option.RequestOption) *OpenAI {
	official := baseURL == ""
	if official {
		baseURL = openAIDefaultBaseURL
	}
	if apiKey == "" {
		apiKey = "unused" // local servers such as Ollama don't check it, but the header must be set
	}
	opts = append([]option.RequestOption{option.WithAPIKey(apiKey), option.WithBaseURL(baseURL)}, opts...)
	return &OpenAI{client: openai.NewClient(opts...), official: official}
}

func (o *OpenAI) Stream(ctx context.Context, req Request, onText func(string) error) (*Result, error) {
	params := openai.ChatCompletionNewParams{
		Model:         req.Model,
		Messages:      make([]openai.ChatCompletionMessageParamUnion, 0, len(req.Messages)+1),
		StreamOptions: openai.ChatCompletionStreamOptionsParam{IncludeUsage: openai.Bool(true)},
	}
	if req.System != "" {
		params.Messages = append(params.Messages, openai.SystemMessage(req.System))
	}
	for _, m := range req.Messages {
		if m.Role == "assistant" {
			params.Messages = append(params.Messages, openai.AssistantMessage(m.Content))
		} else {
			params.Messages = append(params.Messages, openai.UserMessage(m.Content))
		}
	}
	// OpenAI replaced max_tokens with max_completion_tokens; compatible servers mostly
	// still expect the old name.
	if o.official {
		params.MaxCompletionTokens = openai.Int(req.MaxTokens)
	} else {
		params.MaxTokens = openai.Int(req.MaxTokens)
	}
	if req.Effort != "" {
		params.ReasoningEffort = shared.ReasoningEffort(req.Effort)
	}

	stream := o.client.Chat.Completions.NewStreaming(ctx, params)
	defer stream.Close()
	acc := openai.ChatCompletionAccumulator{}
	usageSeen := false
	for stream.Next() {
		chunk := stream.Current()
		acc.AddChunk(chunk)
		if chunk.JSON.Usage.Valid() && chunk.Usage.TotalTokens > 0 {
			usageSeen = true
		}
		if len(chunk.Choices) > 0 && chunk.Choices[0].Delta.Content != "" {
			if err := onText(chunk.Choices[0].Delta.Content); err != nil {
				return openAIResult(&acc, usageSeen), err
			}
		}
	}
	if err := stream.Err(); err != nil {
		return openAIResult(&acc, usageSeen), mapOpenAIError(err)
	}
	return openAIResult(&acc, usageSeen), nil
}

func openAIResult(acc *openai.ChatCompletionAccumulator, usageSeen bool) *Result {
	res := &Result{
		Model: acc.Model,
		Usage: Usage{InputTokens: acc.Usage.PromptTokens, OutputTokens: acc.Usage.CompletionTokens},
	}
	if len(acc.Choices) > 0 {
		res.Text = acc.Choices[0].Message.Content
		res.StopReason = stopReason(acc.Choices[0].FinishReason)
	}
	// Only a finished reply with usage is exact; otherwise the caller charges a reservation.
	res.Complete = res.StopReason != "" && usageSeen
	return res
}

// stopReason maps OpenAI finish reasons onto the names eze.ai reports for every provider.
func stopReason(finish string) string {
	switch finish {
	case "stop":
		return "end_turn"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	default:
		return finish
	}
}

func mapOpenAIError(err error) error {
	if errors.Is(err, context.Canceled) {
		return err
	}
	var apierr *openai.Error
	if !errors.As(err, &apierr) {
		return &Error{Status: http.StatusBadGateway, Message: "could not reach the AI provider"}
	}
	message := apierr.Message
	if message == "" {
		message = providerMessage(apierr.RawJSON(), apierr.StatusCode)
	}
	return statusError(apierr.StatusCode, message)
}
