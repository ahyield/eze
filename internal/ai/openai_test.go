package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/openai/openai-go/v3/option"
)

type fakeOpenAI struct {
	mu     sync.Mutex
	body   map[string]any
	header http.Header
	srv    *httptest.Server
}

// newFakeOpenAI answers Chat Completions requests with reply.
func newFakeOpenAI(t *testing.T, reply func(w http.ResponseWriter)) *fakeOpenAI {
	f := &fakeOpenAI{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		f.mu.Lock()
		if err := json.NewDecoder(r.Body).Decode(&f.body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		f.header = r.Header.Clone()
		f.mu.Unlock()
		reply(w)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func chunk(w http.ResponseWriter, data string) {
	fmt.Fprintf(w, "data: %s\n\n", data)
	w.(http.Flusher).Flush()
}

// streamOpenAI streams text in pieces, then the finish reason and (optionally) usage.
func streamOpenAI(pieces []string, finish string, withUsage bool) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for i, p := range pieces {
			role := ""
			if i == 0 {
				role = `"role":"assistant",`
			}
			b, _ := json.Marshal(p)
			chunk(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama3.3","choices":[{"index":0,"delta":{`+role+`"content":`+string(b)+`},"finish_reason":null}]}`)
		}
		if finish != "" {
			chunk(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama3.3","choices":[{"index":0,"delta":{},"finish_reason":"`+finish+`"}]}`)
		}
		if withUsage {
			chunk(w, `{"id":"c1","object":"chat.completion.chunk","created":1,"model":"llama3.3","choices":[],"usage":{"prompt_tokens":12,"completion_tokens":3,"total_tokens":15}}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

var hi = Request{Model: "llama3.3", System: "Be brief.", MaxTokens: 100, Messages: []Message{{Role: "user", Content: "hi"}, {Role: "assistant", Content: "yo"}, {Role: "user", Content: "again"}}}

func TestOpenAICompatibleStream(t *testing.T) {
	f := newFakeOpenAI(t, streamOpenAI([]string{"Hel", "lo"}, "stop", true))
	p := NewOpenAI("", f.srv.URL+"/v1/")
	var got []string
	res, err := p.Stream(context.Background(), hi, func(s string) error { got = append(got, s); return nil })
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, "|") != "Hel|lo" || res.Text != "Hello" || res.StopReason != "end_turn" || !res.Complete {
		t.Errorf("result = %+v, chunks %v", res, got)
	}
	if res.Usage != (Usage{InputTokens: 12, OutputTokens: 3}) || res.Model != "llama3.3" {
		t.Errorf("usage = %+v model %q", res.Usage, res.Model)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	msgs, _ := f.body["messages"].([]any)
	if len(msgs) != 4 || msgs[0].(map[string]any)["role"] != "system" || msgs[2].(map[string]any)["role"] != "assistant" {
		t.Errorf("messages = %v", f.body["messages"])
	}
	// Compatible servers get max_tokens, and must be asked for usage.
	if f.body["max_tokens"] != 100.0 || f.body["max_completion_tokens"] != nil {
		t.Errorf("token cap fields = %v / %v", f.body["max_tokens"], f.body["max_completion_tokens"])
	}
	if so, _ := f.body["stream_options"].(map[string]any); so["include_usage"] != true {
		t.Errorf("stream_options = %v", f.body["stream_options"])
	}
	if _, ok := f.body["reasoning_effort"]; ok {
		t.Error("unset effort was sent")
	}
	if f.header.Get("Authorization") != "Bearer unused" {
		t.Errorf("auth header for a keyless local server = %q", f.header.Get("Authorization"))
	}
}

func TestOpenAIOfficialUsesNewTokenField(t *testing.T) {
	f := newFakeOpenAI(t, streamOpenAI([]string{"x"}, "stop", true))
	// No base URL means api.openai.com; the override only redirects the test's traffic.
	p := NewOpenAI("sk-test", "", option.WithBaseURL(f.srv.URL+"/v1/"))
	req := hi
	req.Effort = "low"
	if _, err := p.Stream(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.body["max_completion_tokens"] != 100.0 || f.body["max_tokens"] != nil || f.body["reasoning_effort"] != "low" {
		t.Errorf("body = %v", f.body)
	}
	if f.header.Get("Authorization") != "Bearer sk-test" {
		t.Errorf("auth = %q", f.header.Get("Authorization"))
	}
}

func TestOpenAIStopReasonsAndCompleteness(t *testing.T) {
	for _, tt := range []struct {
		finish   string
		usage    bool
		stop     string
		complete bool
	}{
		{"length", true, "max_tokens", true},
		{"content_filter", true, "refusal", true},
		{"stop", false, "end_turn", false}, // a server that never reports usage can't be charged exactly
		{"", false, "", false},             // the stream ended early
	} {
		f := newFakeOpenAI(t, streamOpenAI([]string{"x"}, tt.finish, tt.usage))
		res, err := NewOpenAI("", f.srv.URL+"/v1/").Stream(context.Background(), hi, func(string) error { return nil })
		if err != nil {
			t.Fatal(err)
		}
		if res.StopReason != tt.stop || res.Complete != tt.complete {
			t.Errorf("finish %q usage %v: stop %q complete %v", tt.finish, tt.usage, res.StopReason, res.Complete)
		}
	}
}

func TestOpenAIErrors(t *testing.T) {
	for _, tt := range []struct {
		status   int
		message  string
		want     int
		contains string
	}{
		{400, "context length exceeded", 400, "context length exceeded"},
		{404, "model 'gpt-9' not found", 400, "not found"},
		{401, "bad key", 502, "credentials"},
		{429, "slow down", 503, "busy"},
		{500, "boom", 502, "failed"},
	} {
		f := newFakeOpenAI(t, func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(tt.status)
			fmt.Fprintf(w, `{"error":{"message":%q,"type":"x","code":null,"param":null}}`, tt.message)
		})
		_, err := NewOpenAI("", f.srv.URL+"/v1/", option.WithMaxRetries(0)).Stream(context.Background(), hi, func(string) error { return nil })
		var aerr *Error
		if !errors.As(err, &aerr) || aerr.Status != tt.want || !strings.Contains(aerr.Message, tt.contains) {
			t.Errorf("status %d: got %v", tt.status, err)
		}
	}
}
