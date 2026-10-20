package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/server"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
)

// mcpEnv runs a real eze server and connects an MCP client to newMCPServer through it.
type mcpEnv struct {
	t     *testing.T
	http  *http.Client
	token string
	cs    *mcp.ClientSession
}

func newMCPEnv(t *testing.T, loggedIn bool) *mcpEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Domain: "eze.test", Scheme: "http", DataDir: dir, AuthMode: config.AuthDev,
		OverwritePolicy: config.OverwriteAnyone, MaxDeployBytes: 1 << 20, MaxSiteBytes: 1 << 20,
		MaxSiteFiles: 100, MaxDocBytes: 1 << 16, MaxUploadBytes: 1 << 16, UploadQuota: 1 << 20,
		RateLimit: 1000, RateBurst: 1000, TokenTTL: time.Hour, AIMaxConcurrent: 3,
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
	srv, err := server.New(cfg, &auth.Dev{Email: "dev@example.com"}, st, ss)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	// Route every host (eze.test, blog.eze.test...) to the test server.
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "tcp", ts.Listener.Addr().String())
	}}}
	token, hash := auth.NewToken()
	if err := st.CreateToken(context.Background(), hash, "dev@example.com", "Dev", "test", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}

	connect := func() (*client, error) {
		if !loggedIn {
			return nil, errors.New("not logged in: run `eze login <server-url>` first")
		}
		return &client{cfg: &Config{Server: "http://eze.test", Token: token}, http: hc}, nil
	}
	ctx := context.Background()
	st1, st2 := mcp.NewInMemoryTransports()
	if _, err := newMCPServer(connect).Connect(ctx, st1, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(ctx, st2, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return &mcpEnv{t: t, http: hc, token: token, cs: cs}
}

// call runs a tool and returns its structured result, failing on tool errors unless wantErr.
func (e *mcpEnv) call(name string, args any, wantErr bool) (map[string]any, string) {
	e.t.Helper()
	res, err := e.cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: protocol error: %v", name, err)
	}
	var text strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			text.WriteString(tc.Text)
		}
	}
	if res.IsError != wantErr {
		e.t.Fatalf("%s: IsError = %v, want %v: %s", name, res.IsError, wantErr, text.String())
	}
	var out map[string]any
	if res.StructuredContent != nil {
		b, _ := json.Marshal(res.StructuredContent)
		json.Unmarshal(b, &out)
	}
	return out, text.String()
}

func (e *mcpEnv) get(url string) string {
	e.t.Helper()
	res, err := e.http.Get(url)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	return string(b)
}

func TestMCPTools(t *testing.T) {
	e := newMCPEnv(t, true)

	tools, err := e.cs.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
		if tool.Description == "" {
			t.Errorf("%s has no description", tool.Name)
		}
		if strings.Contains(tool.Name, "delete") {
			t.Errorf("agents must not get a delete tool: %s", tool.Name)
		}
	}
	sort.Strings(names)
	if strings.Join(names, ",") != "deploy_files,deploy_folder,get_site,list_sites,query_collection,sdk_docs,whoami" {
		t.Errorf("tools = %v", names)
	}

	if out, _ := e.call("whoami", map[string]any{}, false); out["email"] != "dev@example.com" || out["server"] != "http://eze.test" {
		t.Errorf("whoami = %v", out)
	}

	// Deploy inline files, including a binary one.
	png := []byte("\x89PNG\r\n\x1a\nfake")
	out, _ := e.call("deploy_files", map[string]any{"site": "demo", "files": []map[string]any{
		{"path": "index.html", "content": "<h1>from mcp</h1>"},
		{"path": "img/logo.png", "content": base64.StdEncoding.EncodeToString(png), "base64": true},
	}}, false)
	if out["url"] != "http://demo.eze.test/" || out["files"] != 2.0 || out["warning"] != nil {
		t.Errorf("deploy_files = %v", out)
	}
	if got := e.get("http://demo.eze.test/"); got != "<h1>from mcp</h1>" {
		t.Errorf("deployed page = %q", got)
	}
	if got := e.get("http://demo.eze.test/img/logo.png"); got != string(png) {
		t.Errorf("binary file = %q", got)
	}

	// Deploy a folder, which also records the site name in eze.json.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "index.html"), []byte("folder site"), 0o644)
	os.WriteFile(filepath.Join(dir, ".env"), []byte("SECRET=1"), 0o644)
	out, _ = e.call("deploy_folder", map[string]any{"path": dir, "site": "folder-site"}, false)
	if out["url"] != "http://folder-site.eze.test/" || out["files"] != 1.0 {
		t.Errorf("deploy_folder = %v", out)
	}
	if p, _ := readProject(dir); p == nil || p.Site != "folder-site" {
		t.Errorf("eze.json = %+v", p)
	}
	if got := e.get("http://folder-site.eze.test/.env"); strings.Contains(got, "SECRET") {
		t.Error("deploy_folder uploaded a hidden file")
	}

	out, _ = e.call("list_sites", map[string]any{}, false)
	if sites, _ := out["sites"].([]any); len(sites) != 2 {
		t.Errorf("list_sites = %v", out)
	}
	out, _ = e.call("get_site", map[string]any{"site": "demo"}, false)
	if deploys, _ := out["deploys"].([]any); out["name"] != "demo" || len(deploys) != 1 {
		t.Errorf("get_site = %v", out)
	}

	// Read back data the site stored.
	for _, status := range []string{"draft", "live", "draft"} {
		req, _ := http.NewRequest("POST", "http://demo.eze.test/_q/db/posts", strings.NewReader(`{"status":"`+status+`"}`))
		req.Header.Set("Authorization", "Bearer "+e.token)
		req.Header.Set("Content-Type", "application/json")
		res, err := e.http.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	out, _ = e.call("query_collection", map[string]any{"site": "demo", "collection": "posts", "where": map[string]any{"status": "draft"}}, false)
	if docs, _ := out["documents"].([]any); len(docs) != 2 {
		t.Errorf("query_collection = %v", out)
	}

	if _, text := e.call("sdk_docs", map[string]any{}, false); !strings.Contains(text, "window.eze") || strings.Contains(text, "{{SITE}}") {
		t.Errorf("sdk_docs = %.100q", text)
	}
	res, err := e.cs.ReadResource(context.Background(), &mcp.ReadResourceParams{URI: "eze://docs/sdk"})
	if err != nil || len(res.Contents) != 1 || !strings.Contains(res.Contents[0].Text, "eze.db.collection") {
		t.Errorf("resource = %v, %v", res, err)
	}
}

func TestMCPToolErrors(t *testing.T) {
	e := newMCPEnv(t, true)
	for _, tt := range []struct {
		tool string
		args map[string]any
		want string
	}{
		{"deploy_folder", map[string]any{"path": "relative/dir"}, "absolute"},
		{"deploy_folder", map[string]any{"path": "/definitely/not/here"}, "not a directory"},
		{"deploy_files", map[string]any{"site": "Bad_Name", "files": []map[string]any{{"path": "index.html", "content": "x"}}}, "not a valid site name"},
		{"deploy_files", map[string]any{"site": "ok", "files": []map[string]any{{"path": "../escape", "content": "x"}}}, "invalid file path"},
		{"deploy_files", map[string]any{"site": "ok", "files": []map[string]any{{"path": "a", "content": "x"}, {"path": "./a", "content": "y"}}}, "given twice"},
		{"deploy_files", map[string]any{"site": "ok", "files": []map[string]any{{"path": "a.png", "content": "%%%", "base64": true}}}, "base64"},
		{"deploy_files", map[string]any{"site": "ok", "files": []map[string]any{}}, "no files"},
		{"get_site", map[string]any{"site": "nope"}, "no site named nope"},
		{"query_collection", map[string]any{"site": "nope", "collection": "posts"}, "deploy it first"},
	} {
		if _, text := e.call(tt.tool, tt.args, true); !strings.Contains(text, tt.want) {
			t.Errorf("%s %v: error %q, want it to mention %q", tt.tool, tt.args, text, tt.want)
		}
	}
	out, _ := e.call("deploy_files", map[string]any{"site": "no-index", "files": []map[string]any{{"path": "about.html", "content": "x"}}}, false)
	if !strings.Contains(out["warning"].(string), "index.html") {
		t.Errorf("missing index.html warning: %v", out)
	}
}

func TestMCPNotLoggedIn(t *testing.T) {
	e := newMCPEnv(t, false)
	if _, text := e.call("list_sites", map[string]any{}, true); !strings.Contains(text, "eze login") {
		t.Errorf("error = %q", text)
	}
	// Docs don't need a login.
	if _, text := e.call("sdk_docs", map[string]any{}, false); !strings.Contains(text, "window.eze") {
		t.Errorf("sdk_docs = %.80q", text)
	}
}

func TestSiteBase(t *testing.T) {
	for in, want := range map[string]string{
		"https://eze.example.com":  "https://blog.eze.example.com",
		"https://eze.example.com/": "https://blog.eze.example.com",
		"http://localhost:8080":    "http://blog.localhost:8080",
	} {
		if got, err := siteBase(in, "blog"); err != nil || got != want {
			t.Errorf("siteBase(%q) = %q, %v", in, got, err)
		}
	}
	if _, err := siteBase("https://eze.example.com", "../x"); err == nil {
		t.Error("invalid site name accepted")
	}
}
