package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
	"github.com/ahyield/eze/internal/testutil"
)

const (
	ana = "ana@example.com"
	bo  = "bo@example.com"
)

type env struct {
	t     *testing.T
	cfg   *config.Config
	store *store.Store
	srv   *Server
}

// newEnv builds a server for domain eze.test using header auth: requests from the
// default httptest address (192.0.2.1) or loopback are trusted to carry X-Email.
func newEnv(t *testing.T, mutate ...func(*config.Config)) *env {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Domain:          "eze.test",
		Scheme:          "https",
		DataDir:         dir,
		AuthMode:        config.AuthHeader,
		OverwritePolicy: config.OverwriteAnyone,
		MaxDeployBytes:  1 << 20,
		MaxSiteBytes:    1 << 20,
		MaxSiteFiles:    100,
		MaxDocBytes:     1 << 12,
		MaxUploadBytes:  1 << 16,
		UploadQuota:     1 << 18,
		RateLimit:       1000,
		RateBurst:       1000,
	}
	for _, m := range mutate {
		m(cfg)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ss, err := sites.NewStore(dir, cfg.MaxSiteBytes, cfg.MaxSiteFiles)
	if err != nil {
		t.Fatal(err)
	}
	authn := &auth.Header{
		EmailHeader:    "X-Email",
		NameHeader:     "X-Name",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("192.0.2.1/32"), netip.MustParsePrefix("127.0.0.1/32")},
	}
	srv, err := New(cfg, authn, st, ss)
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, cfg: cfg, store: st, srv: srv}
}

type req struct {
	method  string
	url     string
	body    io.Reader
	as      string // X-Email; empty means anonymous
	headers map[string]string
	remote  string
}

func (e *env) do(r req) *httptest.ResponseRecorder {
	e.t.Helper()
	if r.method == "" {
		r.method = "GET"
	}
	hr := httptest.NewRequest(r.method, r.url, r.body)
	if r.as != "" {
		hr.Header.Set("X-Email", r.as)
	}
	if r.remote != "" {
		hr.RemoteAddr = r.remote
	}
	for k, v := range r.headers {
		hr.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	e.srv.ServeHTTP(w, hr)
	return w
}

func (e *env) deploy(site, as string, files map[string]string) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.do(req{method: "PUT", url: "https://eze.test/_q/api/sites/" + site, as: as, body: bytes.NewReader(testutil.Files(e.t, files))})
}

func decode[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatalf("decoding %q: %v", w.Body, err)
	}
	return v
}

func jsonBody(v any) io.Reader {
	b, _ := json.Marshal(v)
	return bytes.NewReader(b)
}

func TestHostRouting(t *testing.T) {
	e := newEnv(t)
	e.deploy("blog", ana, map[string]string{"index.html": "hi"})
	for _, tt := range []struct {
		url  string
		code int
	}{
		{"https://blog.eze.test/", 200},
		{"https://BLOG.eze.test/", 200},
		{"https://eze.test/", 200},
		{"https://other.com/", 404},
		{"https://a.blog.eze.test/", 404},
		{"https://www.eze.test/", 404},
		{"https://-x.eze.test/", 404},
		{"https://evil-eze.test/", 404},
	} {
		if w := e.do(req{url: tt.url, as: ana}); w.Code != tt.code {
			t.Errorf("%s = %d, want %d", tt.url, w.Code, tt.code)
		}
	}
	if w := e.do(req{url: "https://anything/healthz"}); w.Code != 200 {
		t.Errorf("healthz = %d", w.Code)
	}
}

func TestRequiresAuth(t *testing.T) {
	e := newEnv(t)
	e.deploy("blog", ana, map[string]string{"index.html": "secret"})
	for _, u := range []string{"https://blog.eze.test/", "https://blog.eze.test/_q/db/posts", "https://eze.test/", "https://eze.test/_q/api/sites"} {
		w := e.do(req{url: u})
		if w.Code != 401 || strings.Contains(w.Body.String(), "secret") {
			t.Errorf("anonymous %s = %d", u, w.Code)
		}
	}
	// A spoofed header from an untrusted address is ignored.
	if w := e.do(req{url: "https://blog.eze.test/", as: ana, remote: "203.0.113.7:1234"}); w.Code != 401 {
		t.Errorf("untrusted peer = %d", w.Code)
	}
	if w := e.do(req{url: "https://blog.eze.test/_q/db/posts", headers: map[string]string{"Authorization": "Bearer eze_bogus"}}); w.Code != 401 {
		t.Errorf("bogus token = %d", w.Code)
	}
}

func TestDeployListDelete(t *testing.T) {
	e := newEnv(t)
	w := e.deploy("blog", ana, map[string]string{"index.html": "v1", "a.js": "1"})
	if w.Code != 200 {
		t.Fatalf("deploy = %d %s", w.Code, w.Body)
	}
	res := decode[map[string]any](t, w)
	if res["url"] != "https://blog.eze.test/" || res["owner"] != ana || res["files"] != 2.0 {
		t.Errorf("deploy response = %v", res)
	}
	if w := e.do(req{url: "https://blog.eze.test/", as: bo}); w.Body.String() != "v1" {
		t.Errorf("site = %q", w.Body)
	}

	// Anyone may overwrite by default.
	if w := e.deploy("blog", bo, map[string]string{"index.html": "v2"}); w.Code != 200 {
		t.Fatalf("overwrite = %d %s", w.Code, w.Body)
	}
	list := decode[[]map[string]any](t, e.do(req{url: "https://eze.test/_q/api/sites", as: ana}))
	if len(list) != 1 || list[0]["deployed_by"] != bo || list[0]["owner"] != ana {
		t.Errorf("list = %v", list)
	}
	if w := e.do(req{url: "https://eze.test/", as: ana}); !strings.Contains(w.Body.String(), "https://blog.eze.test/") {
		t.Errorf("index page does not link the site")
	}

	e.do(req{method: "POST", url: "https://blog.eze.test/_q/db/posts", as: ana, body: jsonBody(map[string]any{"x": 1})})
	if w := e.do(req{method: "DELETE", url: "https://eze.test/_q/api/sites/blog", as: bo}); w.Code != 204 {
		t.Fatalf("delete = %d %s", w.Code, w.Body)
	}
	if w := e.do(req{url: "https://blog.eze.test/", as: ana}); w.Code != 404 {
		t.Errorf("deleted site = %d", w.Code)
	}
	if docs := decode[[]any](t, e.do(req{url: "https://blog.eze.test/_q/db/posts", as: ana})); len(docs) != 0 {
		t.Errorf("deleted site still has docs: %v", docs)
	}
	if w := e.do(req{method: "DELETE", url: "https://eze.test/_q/api/sites/blog", as: bo}); w.Code != 404 {
		t.Errorf("delete again = %d", w.Code)
	}
}

func TestDeployErrors(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.MaxDeployBytes = 512 })
	if w := e.deploy("www", ana, map[string]string{"index.html": "x"}); w.Code != 400 {
		t.Errorf("reserved name = %d", w.Code)
	}
	if w := e.deploy("Bad_Name", ana, map[string]string{"index.html": "x"}); w.Code != 400 {
		t.Errorf("invalid name = %d", w.Code)
	}
	if w := e.do(req{method: "PUT", url: "https://eze.test/_q/api/sites/blog", as: ana, body: strings.NewReader("not a tarball")}); w.Code != 400 {
		t.Errorf("bad archive = %d", w.Code)
	}
	noise := make([]byte, 8<<10) // random bytes don't compress, so the archive exceeds 512 bytes
	rand.Read(noise)
	big := testutil.Files(t, map[string]string{"blob.bin": string(noise)})
	if w := e.do(req{method: "PUT", url: "https://eze.test/_q/api/sites/blog", as: ana, body: bytes.NewReader(big)}); w.Code != 413 {
		t.Errorf("too large = %d %s", w.Code, w.Body)
	}
}

func TestOwnerPolicy(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.OverwritePolicy = config.OverwriteOwner })
	if w := e.deploy("blog", ana, map[string]string{"index.html": "v1"}); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if w := e.deploy("blog", bo, map[string]string{"index.html": "v2"}); w.Code != 403 {
		t.Errorf("non-owner deploy = %d", w.Code)
	}
	if w := e.do(req{method: "DELETE", url: "https://eze.test/_q/api/sites/blog", as: bo}); w.Code != 403 {
		t.Errorf("non-owner delete = %d", w.Code)
	}
	if w := e.deploy("blog", ana, map[string]string{"index.html": "v3"}); w.Code != 200 {
		t.Errorf("owner redeploy = %d", w.Code)
	}
	if w := e.do(req{url: "https://blog.eze.test/", as: bo}); w.Body.String() != "v3" {
		t.Errorf("site = %q", w.Body)
	}
}

func TestTokenAuth(t *testing.T) {
	e := newEnv(t)
	token, hash := auth.NewToken()
	if err := e.store.CreateToken(context.Background(), hash, ana, "Ana", "cli"); err != nil {
		t.Fatal(err)
	}
	// Tokens work from any address, e.g. a laptop going around the auth proxy.
	bearer := map[string]string{"Authorization": "Bearer " + token}
	w := e.do(req{url: "https://eze.test/_q/api/me", headers: bearer, remote: "203.0.113.7:1234"})
	if me := decode[map[string]any](t, w); me["email"] != ana || me["name"] != "Ana" {
		t.Errorf("me = %v", me)
	}
	w = e.do(req{method: "PUT", url: "https://eze.test/_q/api/sites/blog", headers: bearer, remote: "203.0.113.7:1234",
		body: bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "x"}))})
	if w.Code != 200 {
		t.Errorf("token deploy = %d %s", w.Code, w.Body)
	}
}

func TestCrossOriginWrites(t *testing.T) {
	e := newEnv(t)
	post := func(headers map[string]string) int {
		return e.do(req{method: "POST", url: "https://blog.eze.test/_q/db/posts", as: ana, headers: headers, body: jsonBody(map[string]any{"a": 1})}).Code
	}
	if c := post(nil); c != 201 {
		t.Errorf("no origin headers = %d", c)
	}
	if c := post(map[string]string{"Origin": "https://blog.eze.test", "Sec-Fetch-Site": "same-origin"}); c != 201 {
		t.Errorf("same origin = %d", c)
	}
	if c := post(map[string]string{"Origin": "https://evil.eze.test"}); c != 403 {
		t.Errorf("sibling site origin = %d", c)
	}
	if c := post(map[string]string{"Sec-Fetch-Site": "same-site"}); c != 403 {
		t.Errorf("same-site fetch = %d", c)
	}
	if c := post(map[string]string{"Sec-Fetch-Site": "cross-site"}); c != 403 {
		t.Errorf("cross-site fetch = %d", c)
	}
	// Reads are not blocked; the browser's same-origin policy keeps responses private.
	if w := e.do(req{url: "https://blog.eze.test/_q/db/posts", as: ana, headers: map[string]string{"Origin": "https://evil.eze.test"}}); w.Code != 200 {
		t.Errorf("cross-origin read = %d", w.Code)
	}
	// Deleting a site from another site's page is blocked too.
	e.deploy("blog", ana, map[string]string{"index.html": "x"})
	if w := e.do(req{method: "DELETE", url: "https://eze.test/_q/api/sites/blog", as: ana, headers: map[string]string{"Origin": "https://evil.eze.test"}}); w.Code != 403 {
		t.Errorf("cross-origin site delete = %d", w.Code)
	}
}

func TestDocAPI(t *testing.T) {
	e := newEnv(t)
	base := "https://blog.eze.test/_q/db/posts"

	w := e.do(req{method: "POST", url: base, as: ana, body: jsonBody(map[string]any{"title": "Hi", "status": "draft"})})
	if w.Code != 201 {
		t.Fatalf("create = %d %s", w.Code, w.Body)
	}
	doc := decode[map[string]any](t, w)
	id := doc["id"].(string)
	if doc["_created_by"] != ana || doc["title"] != "Hi" {
		t.Errorf("created = %v", doc)
	}

	if got := decode[map[string]any](t, e.do(req{url: base + "/" + id, as: bo})); got["title"] != "Hi" {
		t.Errorf("get = %v", got)
	}
	w = e.do(req{method: "PATCH", url: base + "/" + id, as: bo, body: jsonBody(map[string]any{"status": "live"})})
	if got := decode[map[string]any](t, w); w.Code != 200 || got["status"] != "live" || got["title"] != "Hi" {
		t.Errorf("patch = %d %v", w.Code, got)
	}
	time.Sleep(2 * time.Millisecond) // created_at has millisecond precision; make "pinned" strictly newer
	w = e.do(req{method: "PUT", url: base + "/pinned", as: ana, body: jsonBody(map[string]any{"title": "Pinned"})})
	if w.Code != 201 {
		t.Errorf("put new = %d", w.Code)
	}
	w = e.do(req{method: "PUT", url: base + "/pinned", as: ana, body: jsonBody(map[string]any{"title": "Pinned 2"})})
	if w.Code != 200 {
		t.Errorf("put existing = %d", w.Code)
	}

	where := url.QueryEscape(`{"status":"live"}`)
	if list := decode[[]map[string]any](t, e.do(req{url: base + "?where=" + where, as: ana})); len(list) != 1 || list[0]["id"] != id {
		t.Errorf("filtered list = %v", list)
	}
	if list := decode[[]map[string]any](t, e.do(req{url: base + "?limit=1&order=desc", as: ana})); len(list) != 1 || list[0]["id"] != "pinned" {
		t.Errorf("desc limit list = %v", list)
	}

	if w := e.do(req{method: "DELETE", url: base + "/" + id, as: ana}); w.Code != 204 {
		t.Errorf("delete = %d", w.Code)
	}
	if w := e.do(req{url: base + "/" + id, as: ana}); w.Code != 404 {
		t.Errorf("get deleted = %d", w.Code)
	}
	if w := e.do(req{method: "PATCH", url: base + "/" + id, as: ana, body: jsonBody(map[string]any{"a": 1})}); w.Code != 404 {
		t.Errorf("patch deleted = %d", w.Code)
	}
}

func TestDocAPIValidation(t *testing.T) {
	e := newEnv(t)
	base := "https://blog.eze.test/_q/db"
	for _, tt := range []struct {
		name string
		r    req
		code int
	}{
		{"bad collection", req{url: base + "/bad.name"}, 400},
		{"bad id", req{url: base + "/posts/a.b"}, 400},
		{"limit too big", req{url: base + "/posts?limit=5000"}, 400},
		{"limit zero", req{url: base + "/posts?limit=0"}, 400},
		{"negative offset", req{url: base + "/posts?offset=-1"}, 400},
		{"bad order", req{url: base + "/posts?order=sideways"}, 400},
		{"bad where json", req{url: base + "/posts?where=nope"}, 400},
		{"bad where field", req{url: base + "/posts?where=" + url.QueryEscape(`{"a;drop":1}`)}, 400},
		{"object filter", req{url: base + "/posts?where=" + url.QueryEscape(`{"a":{"b":1}}`)}, 400},
		{"array body", req{method: "POST", url: base + "/posts", body: strings.NewReader(`[1,2]`)}, 400},
		{"null body", req{method: "POST", url: base + "/posts", body: strings.NewReader(`null`)}, 400},
		{"invalid json", req{method: "POST", url: base + "/posts", body: strings.NewReader(`{`)}, 400},
		{"too large", req{method: "POST", url: base + "/posts", body: jsonBody(map[string]any{"x": strings.Repeat("a", 5000)})}, 413},
		{"unknown endpoint", req{url: "https://blog.eze.test/_q/nope"}, 404},
	} {
		tt.r.as = ana
		if w := e.do(tt.r); w.Code != tt.code {
			t.Errorf("%s = %d, want %d (%s)", tt.name, w.Code, tt.code, w.Body)
		}
	}
}

func TestDocsIsolatedPerSite(t *testing.T) {
	e := newEnv(t)
	e.do(req{method: "POST", url: "https://a.eze.test/_q/db/posts", as: ana, body: jsonBody(map[string]any{"x": 1})})
	if list := decode[[]any](t, e.do(req{url: "https://b.eze.test/_q/db/posts", as: ana})); len(list) != 0 {
		t.Errorf("site b sees site a's docs: %v", list)
	}
}

func TestMeAndSDK(t *testing.T) {
	e := newEnv(t)
	w := e.do(req{url: "https://blog.eze.test/_q/me", as: ana, headers: map[string]string{"X-Name": "Ana Lima"}})
	if me := decode[map[string]any](t, w); me["email"] != ana || me["name"] != "Ana Lima" {
		t.Errorf("me = %v", me)
	}
	w = e.do(req{url: "https://blog.eze.test/_q/sdk.js", as: ana})
	if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") || !strings.Contains(w.Body.String(), "window.eze") {
		t.Errorf("sdk = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
}

func TestRateLimit(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.RateLimit = 0.001; c.RateBurst = 2 })
	e.deploy("blog", ana, map[string]string{"index.html": "x"}) // uses one token from ana's bucket
	codes := []int{}
	for range 3 {
		codes = append(codes, e.do(req{url: "https://blog.eze.test/_q/db/posts", as: ana}).Code)
	}
	if codes[0] != 200 || codes[1] != 429 || codes[2] != 429 {
		t.Errorf("codes = %v", codes)
	}
	if w := e.do(req{url: "https://blog.eze.test/_q/db/posts", as: bo}); w.Code != 200 {
		t.Errorf("other user limited: %d", w.Code)
	}
	if w := e.do(req{url: "https://blog.eze.test/", as: ana}); w.Code != 200 {
		t.Errorf("static files should not be rate limited: %d", w.Code)
	}
}

func TestCLIAuthorize(t *testing.T) {
	e := newEnv(t)
	state := "abcdefghijklmnopqrstuvwxyz"
	page := "https://eze.test/_q/cli/authorize?port=50123&state=" + state

	if w := e.do(req{url: page}); w.Code != 401 {
		t.Errorf("anonymous = %d", w.Code)
	}
	for _, bad := range []string{"?port=80&state=" + state, "?port=50123&state=short", "?port=x&state=" + state} {
		if w := e.do(req{url: "https://eze.test/_q/cli/authorize" + bad, as: ana}); w.Code != 400 {
			t.Errorf("%s = %d", bad, w.Code)
		}
	}
	if w := e.do(req{url: page, as: ana}); w.Code != 200 || !strings.Contains(w.Body.String(), ana) {
		t.Errorf("page = %d", w.Code)
	}

	form := url.Values{"port": {"50123"}, "state": {state}}.Encode()
	formHeaders := map[string]string{"Content-Type": "application/x-www-form-urlencoded"}
	w := e.do(req{method: "POST", url: "https://eze.test/_q/cli/authorize", as: ana, headers: formHeaders, body: strings.NewReader(form)})
	if w.Code != http.StatusSeeOther {
		t.Fatalf("authorize = %d %s", w.Code, w.Body)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Host != "127.0.0.1:50123" || loc.Path != "/callback" || loc.Query().Get("state") != state {
		t.Errorf("redirect = %s", loc)
	}
	token := loc.Query().Get("token")
	w = e.do(req{url: "https://eze.test/_q/api/me", headers: map[string]string{"Authorization": "Bearer " + token}, remote: "203.0.113.7:1"})
	if me := decode[map[string]any](t, w); me["email"] != ana {
		t.Errorf("minted token me = %v", me)
	}

	// Another site can't mint tokens by auto-submitting the form.
	formHeaders["Origin"] = "https://evil.eze.test"
	w = e.do(req{method: "POST", url: "https://eze.test/_q/cli/authorize", as: ana, headers: formHeaders, body: strings.NewReader(form)})
	if w.Code != 403 {
		t.Errorf("cross-origin authorize = %d", w.Code)
	}
}

// Realtime

// dialer routes every connection to the test server, whatever host the URL names.
func dialer(ts *httptest.Server) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "tcp", ts.Listener.Addr().String())
		},
	}}
}

func waitSubscribers(t *testing.T, h *hub, site, collection string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.mu.Lock()
		got := len(h.subs[hubKey(site, collection)])
		h.mu.Unlock()
		if got == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d subscribers", n)
}

func TestSubscribe(t *testing.T) {
	e := newEnv(t)
	ts := httptest.NewServer(e.srv)
	defer ts.Close()
	client := dialer(ts)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws://blog.eze.test/_q/subscribe/posts", &websocket.DialOptions{
		HTTPClient: client,
		HTTPHeader: http.Header{"X-Email": {ana}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	waitSubscribers(t, e.srv.hub, "blog", "posts", 1)

	send := func(method, path string, body any) {
		r, _ := http.NewRequest(method, "http://blog.eze.test/_q/db/posts"+path, jsonBody(body))
		r.Header.Set("X-Email", bo)
		res, err := client.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	next := func() event {
		_, msg, err := conn.Read(ctx)
		if err != nil {
			t.Fatal(err)
		}
		var ev struct {
			Type string         `json:"type"`
			Doc  map[string]any `json:"doc"`
			ID   string         `json:"id"`
		}
		if err := json.Unmarshal(msg, &ev); err != nil {
			t.Fatal(err)
		}
		return event{Type: ev.Type, Doc: ev.Doc, ID: ev.ID}
	}

	// Writes to another site or collection must not leak into this subscription.
	r, _ := http.NewRequest("POST", "http://other.eze.test/_q/db/posts", jsonBody(map[string]any{"x": 1}))
	r.Header.Set("X-Email", bo)
	if res, err := client.Do(r); err == nil {
		res.Body.Close()
	}

	send("PUT", "/p1", map[string]any{"v": 1})
	if ev := next(); ev.Type != "create" || ev.Doc.(map[string]any)["id"] != "p1" {
		t.Errorf("event 1 = %+v", ev)
	}
	send("PATCH", "/p1", map[string]any{"v": 2})
	if ev := next(); ev.Type != "update" || ev.Doc.(map[string]any)["v"] != 2.0 {
		t.Errorf("event 2 = %+v", ev)
	}
	send("DELETE", "/p1", nil)
	if ev := next(); ev.Type != "delete" || ev.ID != "p1" {
		t.Errorf("event 3 = %+v", ev)
	}

	conn.Close(websocket.StatusNormalClosure, "")
	waitSubscribers(t, e.srv.hub, "blog", "posts", 0)
}

func TestSubscribeRejectsCrossOrigin(t *testing.T) {
	e := newEnv(t)
	ts := httptest.NewServer(e.srv)
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, res, err := websocket.Dial(ctx, "ws://blog.eze.test/_q/subscribe/posts", &websocket.DialOptions{
		HTTPClient: dialer(ts),
		HTTPHeader: http.Header{"X-Email": {ana}, "Origin": {"https://evil.eze.test"}},
	})
	if err == nil {
		t.Fatal("cross-origin websocket accepted")
	}
	if res != nil && res.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d", res.StatusCode)
	}
}

func TestHub(t *testing.T) {
	h := newHub()
	fast := h.subscribe("blog", "posts")
	slow := h.subscribe("blog", "posts")
	other := h.subscribe("blog", "comments")

	// Fill the slow subscriber's buffer while draining the fast one.
	for i := range cap(slow.ch) + 1 {
		h.publish("blog", "posts", event{Type: "delete", ID: string(rune('a' + i%26))})
		<-fast.ch
	}
	if _, open := drain(slow.ch); open {
		t.Error("slow subscriber should have been dropped")
	}
	h.publish("blog", "posts", event{Type: "delete", ID: "z"}) // must not panic on the closed channel
	if _, open := <-fast.ch; !open {
		t.Error("fast subscriber dropped")
	}
	h.unsubscribe("blog", "posts", slow) // already dropped: no double close

	h.dropSite("blog")
	if _, open := drain(fast.ch); open {
		t.Error("dropSite left a subscriber open")
	}
	if _, open := drain(other.ch); open {
		t.Error("dropSite left another collection open")
	}
	h.unsubscribe("blog", "posts", fast)
	if len(h.subs) != 0 {
		t.Errorf("subs not empty: %v", h.subs)
	}
}

// drain empties a channel and reports how many messages it held and whether it is still open.
func drain(ch chan []byte) (n int, open bool) {
	for {
		select {
		case _, ok := <-ch:
			if !ok {
				return n, false
			}
			n++
		default:
			return n, true
		}
	}
}
