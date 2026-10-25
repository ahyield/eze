package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"
	openaioption "github.com/openai/openai-go/v3/option"

	"github.com/ahyield/eze/internal/ai"
	"github.com/ahyield/eze/internal/config"
)

// fakeAnthropic stands in for the Messages API. reply writes the response for each request.
type fakeAnthropic struct {
	mu      sync.Mutex
	bodies  []map[string]any
	headers []http.Header
	reply   func(w http.ResponseWriter, r *http.Request)
	srv     *httptest.Server
}

func newFakeAnthropic(t *testing.T, reply func(w http.ResponseWriter, r *http.Request)) *fakeAnthropic {
	f := &fakeAnthropic{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decoding request body: %v", err)
		}
		f.mu.Lock()
		f.bodies = append(f.bodies, body)
		f.headers = append(f.headers, r.Header.Clone())
		f.mu.Unlock()
		f.reply(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAnthropic) last() (map[string]any, http.Header) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.bodies) == 0 {
		return nil, nil
	}
	return f.bodies[len(f.bodies)-1], f.headers[len(f.headers)-1]
}

func sseEvent(w io.Writer, event string, data string) {
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, data)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// streamReply answers like the Messages API streaming a text reply.
func streamReply(model string, chunks []string, stop string, in, out int) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseEvent(w, "message_start", fmt.Sprintf(`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":%d,"output_tokens":1}}}`, model, in))
		sseEvent(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			sseEvent(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":`+string(b)+`}}`)
		}
		sseEvent(w, "content_block_stop", `{"type":"content_block_stop","index":0}`)
		sseEvent(w, "message_delta", fmt.Sprintf(`{"type":"message_delta","delta":{"stop_reason":%q,"stop_sequence":null},"usage":{"output_tokens":%d}}`, stop, out))
		sseEvent(w, "message_stop", `{"type":"message_stop"}`)
	}
}

func errorReply(status int, typ, message string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"type":"error","error":{"type":%q,"message":%q}}`, typ, message)
	}
}

// aiEnv is a server whose AI provider talks to a fake upstream, with retries off so error
// tests are fast.
func aiEnv(t *testing.T, reply func(http.ResponseWriter, *http.Request), mutate ...func(*config.Config)) (*env, *fakeAnthropic) {
	t.Helper()
	fake := newFakeAnthropic(t, reply)
	e := newEnv(t, mutate...)
	e.srv.ai = ai.NewAnthropic("sk-test", fake.srv.URL, option.WithMaxRetries(0))
	return e, fake
}

func chat(e *env, body any) *httptest.ResponseRecorder {
	return e.do(req{method: "POST", url: "https://blog.eze.test/_eze/ai/chat", as: ana, body: jsonBody(body)})
}

var hello = map[string]any{"messages": []map[string]string{{"role": "user", "content": "hi"}}}

func TestAIDisabled(t *testing.T) {
	e := newEnv(t)
	if w := chat(e, hello); w.Code != 503 || !strings.Contains(w.Body.String(), "EZE_ANTHROPIC_API_KEY") {
		t.Errorf("chat = %d %s", w.Code, w.Body)
	}
	u := decode[map[string]any](t, e.do(req{url: "https://blog.eze.test/_eze/ai/usage", as: ana}))
	if u["enabled"] != false {
		t.Errorf("usage = %v", u)
	}
}

func TestAIChat(t *testing.T) {
	e, fake := aiEnv(t, streamReply("claude-opus-5-5", []string{"Hel", "lo"}, "end_turn", 12, 3))
	w := chat(e, map[string]any{
		"system":   "Be brief.",
		"effort":   "low",
		"messages": []map[string]string{{"role": "user", "content": "hi"}, {"role": "assistant", "content": "yo"}, {"role": "user", "content": "again"}},
	})
	if w.Code != 200 {
		t.Fatalf("chat = %d %s", w.Code, w.Body)
	}
	res := decode[map[string]any](t, w)
	if res["text"] != "Hello" || res["model"] != "claude-opus-5-5" || res["stop_reason"] != "end_turn" {
		t.Errorf("result = %v", res)
	}
	if u := res["usage"].(map[string]any); u["input_tokens"] != 12.0 || u["output_tokens"] != 3.0 {
		t.Errorf("usage = %v", u)
	}

	body, headers := fake.last()
	if headers.Get("X-Api-Key") != "sk-test" {
		t.Errorf("api key header = %q", headers.Get("X-Api-Key"))
	}
	if body["model"] != "claude-opus-5-5" || body["max_tokens"] != float64(e.cfg.AIMaxOutputTokens) || body["stream"] != true {
		t.Errorf("upstream body = %v", body)
	}
	if sys, _ := json.Marshal(body["system"]); !strings.Contains(string(sys), "Be brief.") {
		t.Errorf("system = %s", sys)
	}
	if msgs := body["messages"].([]any); len(msgs) != 3 || msgs[1].(map[string]any)["role"] != "assistant" {
		t.Errorf("messages = %v", msgs)
	}
	if oc, _ := body["output_config"].(map[string]any); oc["effort"] != "low" {
		t.Errorf("output_config = %v", body["output_config"])
	}
	if body["fallbacks"] != "default" || !strings.Contains(headers.Get("Anthropic-Beta"), "server-side-fallback-2026-07-01") {
		t.Errorf("fallbacks = %v, beta = %q", body["fallbacks"], headers.Get("Anthropic-Beta"))
	}

	u := decode[map[string]any](t, e.do(req{url: "https://blog.eze.test/_eze/ai/usage", as: ana}))
	if u["enabled"] != true || u["used"] != 15.0 || u["default_model"] != "claude-opus-5-5" {
		t.Errorf("usage = %v", u)
	}
}

func TestAIFallbacksOnlyWhereSupported(t *testing.T) {
	e, fake := aiEnv(t, streamReply("claude-haiku-5-5", []string{"ok"}, "end_turn", 1, 1))
	if w := chat(e, map[string]any{"model": "claude-haiku-5-5", "messages": hello["messages"]}); w.Code != 200 {
		t.Fatalf("chat = %d %s", w.Code, w.Body)
	}
	body, headers := fake.last()
	if _, ok := body["fallbacks"]; ok || headers.Get("Anthropic-Beta") != "" {
		t.Errorf("haiku got fallbacks = %v, beta = %q", body["fallbacks"], headers.Get("Anthropic-Beta"))
	}
	if _, ok := body["output_config"]; ok {
		t.Errorf("unset effort sent: %v", body["output_config"])
	}
}

// readSSE parses an event stream into (event, data) pairs.
func readSSE(t *testing.T, r io.Reader) [][2]string {
	t.Helper()
	var events [][2]string
	var event string
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			events = append(events, [2]string{event, strings.TrimPrefix(line, "data: ")})
		}
	}
	return events
}

func TestAIChatStream(t *testing.T) {
	e, _ := aiEnv(t, streamReply("claude-opus-5-5", []string{"a", "b", "c"}, "end_turn", 5, 3))
	w := chat(e, map[string]any{"stream": true, "messages": hello["messages"]})
	if w.Code != 200 || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	events := readSSE(t, w.Body)
	if len(events) != 4 {
		t.Fatalf("events = %v", events)
	}
	for i, want := range []string{`{"text":"a"}`, `{"text":"b"}`, `{"text":"c"}`} {
		if events[i][0] != "text" || events[i][1] != want {
			t.Errorf("event %d = %v", i, events[i])
		}
	}
	var done ai.Result
	if err := json.Unmarshal([]byte(events[3][1]), &done); err != nil {
		t.Fatal(err)
	}
	if events[3][0] != "done" || done.Text != "abc" || done.Usage.OutputTokens != 3 {
		t.Errorf("done = %v", events[3])
	}
}

func TestAIStreamErrorMidway(t *testing.T) {
	e, _ := aiEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseEvent(w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":40,"output_tokens":1}}}`)
		sseEvent(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sseEvent(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`)
		sseEvent(w, "error", `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	})
	w := chat(e, map[string]any{"stream": true, "messages": hello["messages"]})
	events := readSSE(t, w.Body)
	if len(events) != 2 || events[0][1] != `{"text":"partial"}` || events[1][0] != "error" {
		t.Fatalf("events = %v", events)
	}
	// Final usage never arrived, so the whole reservation is charged: input estimate plus max_tokens.
	want := estimateInputTokens(&chatRequest{Messages: []ai.Message{{Role: "user", Content: "hi"}}}) + e.cfg.AIMaxOutputTokens
	if n, _ := e.store.AITokensUsed(context.Background(), ana, time.Now()); n != want {
		t.Errorf("recorded usage = %d, want the reservation %d", n, want)
	}
}

func TestAIUpstreamErrors(t *testing.T) {
	for _, tt := range []struct {
		status   int
		typ, msg string
		want     int
		contains string
	}{
		{400, "invalid_request_error", "messages: roles must alternate", 400, "roles must alternate"},
		{401, "authentication_error", "invalid x-api-key", 502, "credentials"},
		{429, "rate_limit_error", "slow down", 503, "busy"},
		{529, "overloaded_error", "Overloaded", 503, "busy"},
		{500, "api_error", "boom", 502, "failed"},
	} {
		e, _ := aiEnv(t, errorReply(tt.status, tt.typ, tt.msg))
		w := chat(e, hello)
		if w.Code != tt.want || !strings.Contains(w.Body.String(), tt.contains) {
			t.Errorf("upstream %d: got %d %s", tt.status, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "x-api-key") {
			t.Errorf("upstream %d: leaked provider auth details: %s", tt.status, w.Body)
		}
	}
}

func TestAIValidation(t *testing.T) {
	e, fake := aiEnv(t, streamReply("claude-opus-5-5", []string{"x"}, "end_turn", 1, 1),
		func(c *config.Config) { c.AIMaxOutputTokens = 1000 })
	msgs := func(m ...map[string]string) []map[string]string { return m }
	user := map[string]string{"role": "user", "content": "hi"}
	for _, tt := range []struct {
		name string
		body any
	}{
		{"no messages", map[string]any{}},
		{"model not enabled", map[string]any{"model": "gpt-9", "messages": msgs(user)}},
		{"max_tokens over cap", map[string]any{"max_tokens": 1001, "messages": msgs(user)}},
		{"negative max_tokens", map[string]any{"max_tokens": -1, "messages": msgs(user)}},
		{"bad effort", map[string]any{"effort": "extreme", "messages": msgs(user)}},
		{"bad role", map[string]any{"messages": msgs(map[string]string{"role": "system", "content": "x"})}},
		{"empty content", map[string]any{"messages": msgs(map[string]string{"role": "user", "content": ""})}},
		{"assistant first", map[string]any{"messages": msgs(map[string]string{"role": "assistant", "content": "x"}, user)}},
		{"not json", "nope"},
	} {
		if w := chat(e, tt.body); w.Code != 400 {
			t.Errorf("%s = %d %s", tt.name, w.Code, w.Body)
		}
	}
	if body, _ := fake.last(); body != nil {
		t.Errorf("invalid requests reached the provider: %v", body)
	}
	if w := chat(e, map[string]any{"max_tokens": 1000, "messages": msgs(user)}); w.Code != 200 {
		t.Errorf("at cap = %d %s", w.Code, w.Body)
	}
}

func TestAIBudget(t *testing.T) {
	e, fake := aiEnv(t, streamReply("claude-opus-5-5", []string{"x"}, "end_turn", 100, 20),
		func(c *config.Config) { c.AIDailyTokens = 100; c.AIMaxOutputTokens = 30 })
	if w := chat(e, hello); w.Code != 200 {
		t.Fatalf("first = %d", w.Code)
	}
	w := chat(e, hello)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "budget") {
		t.Errorf("over budget = %d %s", w.Code, w.Body)
	}
	fake.mu.Lock()
	calls := len(fake.bodies)
	fake.mu.Unlock()
	if calls != 1 {
		t.Errorf("provider called %d times, want 1", calls)
	}
	// Budgets are per user and span sites.
	if w := e.do(req{method: "POST", url: "https://game.eze.test/_eze/ai/chat", as: ana, body: jsonBody(hello)}); w.Code != 429 {
		t.Errorf("same user, other site = %d", w.Code)
	}
	if w := e.do(req{method: "POST", url: "https://blog.eze.test/_eze/ai/chat", as: bo, body: jsonBody(hello)}); w.Code != 200 {
		t.Errorf("other user = %d", w.Code)
	}
	u := decode[map[string]any](t, e.do(req{url: "https://blog.eze.test/_eze/ai/usage", as: ana}))
	if u["used"] != 120.0 || u["limit"] != 100.0 {
		t.Errorf("usage = %v", u)
	}
}

func TestAICrossOriginBlocked(t *testing.T) {
	e, fake := aiEnv(t, streamReply("claude-opus-5-5", []string{"x"}, "end_turn", 1, 1))
	w := e.do(req{method: "POST", url: "https://blog.eze.test/_eze/ai/chat", as: ana, body: jsonBody(hello),
		headers: map[string]string{"Origin": "https://evil.eze.test"}})
	if w.Code != 403 {
		t.Errorf("cross-origin chat = %d", w.Code)
	}
	if body, _ := fake.last(); body != nil {
		t.Error("cross-origin request reached the provider")
	}
}

func TestAIClientDisconnectCancelsUpstream(t *testing.T) {
	cancelled := make(chan struct{})
	e, _ := aiEnv(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseEvent(w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":1}}}`)
		sseEvent(w, "content_block_start", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		sseEvent(w, "content_block_delta", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"thinking..."}}`)
		select {
		case <-r.Context().Done():
			close(cancelled)
		case <-time.After(5 * time.Second):
		}
	})
	client := dialer(serve(t, e))
	ctx, cancel := context.WithCancel(context.Background())
	r, _ := http.NewRequestWithContext(ctx, "POST", "http://blog.eze.test/_eze/ai/chat", jsonBody(map[string]any{"stream": true, "messages": hello["messages"]}))
	r.Header.Set("X-Email", ana)
	res, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(res.Body).ReadString('\n') // wait until text is flowing
	if !strings.HasPrefix(line, "event: text") {
		t.Fatalf("first line = %q", line)
	}
	cancel()
	res.Body.Close()
	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("upstream request kept running after the browser left")
	}
	// The input already read still counts against the budget.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if n, _ := e.store.AITokensUsed(context.Background(), ana, time.Now()); n > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("usage of an abandoned request was not recorded")
}

// blockingReply streams the start of a reply, then holds the request open until release is closed.
func blockingReply(started chan<- struct{}, release <-chan struct{}) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		sseEvent(w, "message_start", `{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"claude-opus-5-5","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":1}}}`)
		started <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		sseEvent(w, "message_delta", `{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":2}}`)
		sseEvent(w, "message_stop", `{"type":"message_stop"}`)
	}
}

// refusedOrStarted makes a call that should be refused. If it reaches the provider instead,
// it fails the test at once (the provider would hold it open) and returns nil.
func refusedOrStarted(t *testing.T, e *env, started <-chan struct{}) *httptest.ResponseRecorder {
	t.Helper()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- chat(e, hello) }()
	select {
	case w := <-done:
		return w
	case <-started:
		t.Error("the call reached the AI provider; it should have been refused")
		return nil
	}
}

func TestAIConcurrencyCap(t *testing.T) {
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	e, _ := aiEnv(t, blockingReply(started, release), func(c *config.Config) { c.AIMaxConcurrent = 2 })
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); codes <- chat(e, hello).Code }()
	}
	<-started
	<-started
	if w := refusedOrStarted(t, e, started); w != nil && (w.Code != 429 || !strings.Contains(w.Body.String(), "already have 2")) {
		t.Errorf("third concurrent call = %d %s", w.Code, w.Body)
	}
	close(release)
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Errorf("held call = %d", c)
		}
	}
	if w := chat(e, hello); w.Code != 200 {
		t.Errorf("after the others finished = %d", w.Code)
	}
}

func TestAIParallelCallsCantOverspend(t *testing.T) {
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	// Each call reserves its input estimate plus max_tokens (1000); the budget fits two.
	e, _ := aiEnv(t, blockingReply(started, release), func(c *config.Config) {
		c.AIDailyTokens = 2300
		c.AIMaxOutputTokens = 1000
		c.AIMaxConcurrent = 10
	})
	var wg sync.WaitGroup
	results := make(chan int, 5)
	for range 2 {
		wg.Add(1)
		go func() { defer wg.Done(); results <- chat(e, hello).Code }()
	}
	<-started
	<-started
	if w := refusedOrStarted(t, e, started); w != nil && (w.Code != 429 || !strings.Contains(w.Body.String(), "still running")) {
		t.Errorf("third call while two are reserved = %d %s", w.Code, w.Body)
	}
	close(release)
	wg.Wait()
}

func TestAIReservationRefusesHugePrompts(t *testing.T) {
	e, fake := aiEnv(t, streamReply("claude-opus-5-5", []string{"x"}, "end_turn", 1, 1),
		func(c *config.Config) { c.AIDailyTokens = 10_000; c.AIMaxOutputTokens = 1000 })
	big := strings.Repeat("word ", 20_000) // about 33k estimated tokens
	w := chat(e, map[string]any{"messages": []map[string]string{{"role": "user", "content": big}}})
	if w.Code != 429 || !strings.Contains(w.Body.String(), "could use up to") {
		t.Errorf("huge prompt = %d %s", w.Code, w.Body)
	}
	if body, _ := fake.last(); body != nil {
		t.Error("an over-budget prompt reached the provider")
	}
	if w := chat(e, hello); w.Code != 200 {
		t.Errorf("small prompt = %d", w.Code)
	}
}

func TestLedger(t *testing.T) {
	l := newAILedger()
	if err := l.reserve("a", 0, 100, 60, 2); err != nil {
		t.Fatal(err)
	}
	if err := l.reserve("a", 0, 100, 50, 2); !errors.Is(err, errAIBudget) {
		t.Errorf("over reservation = %v", err)
	}
	if err := l.reserve("a", 0, 100, 40, 2); err != nil {
		t.Errorf("exactly at budget = %v", err)
	}
	if err := l.reserve("a", 0, 1000, 1, 2); !errors.Is(err, errAIBusy) {
		t.Errorf("third in flight = %v", err)
	}
	if err := l.reserve("b", 0, 100, 100, 2); err != nil {
		t.Errorf("other user = %v", err)
	}
	l.release("a", 60)
	l.release("a", 40)
	if len(l.inflight) != 1 || len(l.reserved) != 1 {
		t.Errorf("ledger not cleaned up: %v %v", l.inflight, l.reserved)
	}
}

// openAIReply answers like an OpenAI-compatible server; usage is omitted when withUsage is false.
func openAIReply(text string, withUsage bool) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		b, _ := json.Marshal(text)
		sseLine := func(data string) {
			fmt.Fprintf(w, "data: %s\n\n", data)
			w.(http.Flusher).Flush()
		}
		sseLine(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"llama3.3","choices":[{"index":0,"delta":{"role":"assistant","content":` + string(b) + `},"finish_reason":null}]}`)
		sseLine(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"llama3.3","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		if withUsage {
			sseLine(`{"id":"c","object":"chat.completion.chunk","created":1,"model":"llama3.3","choices":[],"usage":{"prompt_tokens":20,"completion_tokens":5,"total_tokens":25}}`)
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func openAIEnv(t *testing.T, reply func(http.ResponseWriter, *http.Request)) *env {
	t.Helper()
	fake := newFakeAnthropic(t, reply) // records requests; the path differs but the handler doesn't care
	e := newEnv(t, func(c *config.Config) {
		c.AIProvider = config.AIOpenAI
		c.AIModels = []string{"llama3.3"}
		c.AIDefaultModel = "llama3.3"
		c.AIMaxOutputTokens = 500
	})
	e.srv.ai = ai.NewOpenAI("", fake.srv.URL+"/v1/", openaioption.WithMaxRetries(0))
	return e
}

func TestAIThroughOpenAICompatibleProvider(t *testing.T) {
	e := openAIEnv(t, openAIReply("Hello from llama", true))
	w := chat(e, map[string]any{"stream": true, "messages": hello["messages"]})
	events := readSSE(t, w.Body)
	if len(events) != 2 || events[0][1] != `{"text":"Hello from llama"}` || events[1][0] != "done" {
		t.Fatalf("events = %v", events)
	}
	if n, _ := e.store.AITokensUsed(context.Background(), ana, time.Now()); n != 25 {
		t.Errorf("recorded = %d, want the reported 25", n)
	}
	if u := decode[map[string]any](t, e.do(req{url: "https://blog.eze.test/_eze/ai/usage", as: ana})); u["provider"] != "openai" {
		t.Errorf("usage = %v", u)
	}
	if w := chat(e, map[string]any{"model": "claude-opus-5-5", "messages": hello["messages"]}); w.Code != 400 {
		t.Errorf("a model outside EZE_AI_MODELS = %d", w.Code)
	}
}

func TestAIOpenAIServerWithoutUsageIsChargedTheReservation(t *testing.T) {
	e := openAIEnv(t, openAIReply("hi", false))
	if w := chat(e, hello); w.Code != 200 {
		t.Fatalf("chat = %d %s", w.Code, w.Body)
	}
	want := estimateInputTokens(&chatRequest{Messages: []ai.Message{{Role: "user", Content: "hi"}}}) + 500
	if n, _ := e.store.AITokensUsed(context.Background(), ana, time.Now()); n != want {
		t.Errorf("recorded = %d, want the reservation %d", n, want)
	}
}
