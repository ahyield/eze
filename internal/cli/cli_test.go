package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestParseFlags(t *testing.T) {
	fs := flag.NewFlagSet("x", flag.ContinueOnError)
	site := fs.String("site", "", "")
	yes := fs.Bool("yes", false, "")
	pos, err := parseFlags(fs, []string{"dist", "--site", "blog", "extra", "--yes"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(pos, ",") != "dist,extra" || *site != "blog" || !*yes {
		t.Errorf("pos=%v site=%q yes=%v", pos, *site, *yes)
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"My Site":                "my-site",
		"lunch_vote!!":           "lunch-vote",
		"--x--":                  "x",
		"日本":                     "",
		strings.Repeat("ab", 40): strings.Repeat("ab", 40)[:63],
		strings.Repeat("a-", 40): strings.TrimRight(strings.Repeat("a-", 40)[:63], "-"),
	} {
		if got := slug(in); got != want {
			t.Errorf("slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func write(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPack(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, map[string]string{
		"index.html":               "hi",
		"assets/app.js":            "js",
		".well-known/security.txt": "sec",
		".env":                     "SECRET",
		".git/config":              "git",
		"node_modules/x/index.js":  "dep",
		"sub/node_modules/y.js":    "dep",
		"AGENTS.md":                "agent",
		"CLAUDE.md":                "agent",
		"eze.json":                 "{}",
		"docs/AGENTS.md":           "nested files with skipped names are real content",
		"assets/.DS_Store":         "junk",
	})
	if err := os.Symlink("/etc/passwd", filepath.Join(dir, "passwd")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	n, err := pack(dir, &buf)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	var files []string
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			files = append(files, hdr.Name)
		}
	}
	sort.Strings(files)
	want := []string{".well-known/security.txt", "assets/app.js", "docs/AGENTS.md", "index.html"}
	if strings.Join(files, ",") != strings.Join(want, ",") || n != len(want) {
		t.Errorf("packed %d: %v, want %v", n, files, want)
	}
}

func TestProjectFile(t *testing.T) {
	dir := t.TempDir()
	if p, err := readProject(dir); p != nil || err != nil {
		t.Fatalf("missing project = %v, %v", p, err)
	}
	if err := writeProject(dir, &project{Site: "blog"}); err != nil {
		t.Fatal(err)
	}
	if p, err := readProject(dir); err != nil || p.Site != "blog" {
		t.Fatalf("roundtrip = %v, %v", p, err)
	}
	os.WriteFile(filepath.Join(dir, projectFile), []byte("{"), 0o644)
	if _, err := readProject(dir); err == nil {
		t.Error("corrupt eze.json accepted")
	}
}

func TestInit(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Lunch Vote")
	if err := Init([]string{dir}); err != nil {
		t.Fatal(err)
	}
	p, _ := readProject(dir)
	if p == nil || p.Site != "lunch-vote" {
		t.Fatalf("project = %+v", p)
	}
	for _, f := range []string{"index.html", "AGENTS.md", "CLAUDE.md"} {
		b, err := os.ReadFile(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "{{SITE}}") {
			t.Errorf("%s still has the {{SITE}} placeholder", f)
		}
	}

	// Re-running never clobbers the user's work.
	write(t, dir, map[string]string{"index.html": "mine"})
	if err := Init([]string{dir, "--site", "other"}); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "index.html")); string(b) != "mine" {
		t.Error("index.html overwritten")
	}
	if p, _ := readProject(dir); p.Site != "lunch-vote" {
		t.Errorf("eze.json overwritten: %+v", p)
	}

	if err := Init([]string{filepath.Join(root, "x"), "--site", "Bad_Name"}); err == nil {
		t.Error("invalid site name accepted")
	}
}

func TestConfigEnvOverride(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("EZE_SERVER", "")
	t.Setenv("EZE_TOKEN", "")
	if _, err := loadConfig(); err == nil || !strings.Contains(err.Error(), "eze login") {
		t.Errorf("no config = %v", err)
	}
	// A config left world-readable (e.g. by an older version or a manual edit) gets fixed on save.
	p, _ := configPath()
	os.MkdirAll(filepath.Dir(p), 0o755)
	os.WriteFile(p, []byte("{}"), 0o644)
	if _, err := saveConfig(&Config{Server: "https://eze.page/", Token: "eze_saved"}); err != nil {
		t.Fatal(err)
	}
	c, err := loadConfig()
	if err != nil || c.Server != "https://eze.page" || c.Token != "eze_saved" {
		t.Errorf("saved = %+v, %v", c, err)
	}
	t.Setenv("EZE_TOKEN", "eze_env")
	if c, _ := loadConfig(); c.Token != "eze_env" {
		t.Errorf("env override = %+v", c)
	}
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config file mode = %v, %v", fi.Mode().Perm(), err)
	}
}

func TestClientErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer eze_t" {
			t.Errorf("auth header = %q", r.Header.Get("Authorization"))
		}
		switch r.URL.Path {
		case "/ok":
			w.Write([]byte(`{"email":"ana@example.com"}`))
		case "/api-error":
			w.WriteHeader(403)
			w.Write([]byte(`{"error":"blog belongs to bo@example.com"}`))
		case "/proxy-login":
			http.Redirect(w, r, "https://idp.example.com/login", http.StatusFound)
		default:
			w.WriteHeader(502)
			w.Write([]byte("bad gateway"))
		}
	}))
	defer ts.Close()
	c := &client{cfg: &Config{Server: ts.URL, Token: "eze_t"}, http: &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}

	var me user
	if err := c.do("GET", "/ok", nil, "", &me); err != nil || me.Email != "ana@example.com" {
		t.Errorf("ok = %+v, %v", me, err)
	}
	if err := c.do("GET", "/api-error", nil, "", nil); err == nil || !strings.Contains(err.Error(), "belongs to bo@example.com") {
		t.Errorf("api error = %v", err)
	}
	if err := c.do("GET", "/proxy-login", nil, "", nil); err == nil || !strings.Contains(err.Error(), "auth proxy") {
		t.Errorf("proxy redirect = %v", err)
	}
	if err := c.do("GET", "/other", nil, "", nil); err == nil || !strings.Contains(err.Error(), "bad gateway") {
		t.Errorf("plain error = %v", err)
	}
}
