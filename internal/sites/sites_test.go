package sites

import (
	"archive/tar"
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahyield/eze/internal/testutil"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func get(s *Store, site, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Serve(w, httptest.NewRequest("GET", path, nil), site)
	return w
}

func TestValidName(t *testing.T) {
	for name, want := range map[string]bool{
		"blog": true, "my-site": true, "a": true, "a1": true, strings.Repeat("a", 63): true,
		"": false, "-blog": false, "blog-": false, "My-Site": false, "my_site": false, "my.site": false,
		strings.Repeat("a", 64): false, "www": false, "api": false,
	} {
		if got := ValidName(name); got != want {
			t.Errorf("ValidName(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestCleanPath(t *testing.T) {
	tests := []struct {
		in   string
		want string
		ok   bool
	}{
		{"index.html", "index.html", true},
		{"./index.html", "index.html", true},
		{"a/b/../c.js", "a/c.js", false}, // any ".." segment is rejected, even if it stays inside
		{"assets/app.js", "assets/app.js", true},
		{"dir/", "dir", true},
		{"", "", true},
		{".well-known/security.txt", ".well-known/security.txt", true},
		{"../etc/passwd", "", false},
		{"/etc/passwd", "", false},
		{"a/../../x", "", false},
		{"..\\windows", "", false},
		{".env", "", false},
		{".git/config", "", false},
		{"assets/.secret", "", false},
	}
	for _, tt := range tests {
		got, ok := cleanPath(tt.in)
		if ok != tt.ok || (ok && got != tt.want) {
			t.Errorf("cleanPath(%q) = %q, %v; want %q, %v", tt.in, got, ok, tt.want, tt.ok)
		}
	}
}

func TestDeployAndServe(t *testing.T) {
	s := newStore(t)
	archive := testutil.TarGz(t,
		testutil.Entry{Name: "index.html", Body: "<h1>home</h1>"},
		testutil.Entry{Name: "docs/", Type: tar.TypeDir},
		testutil.Entry{Name: "docs/index.html", Body: "docs"},
		testutil.Entry{Name: "app.js", Body: "console.log(1)"},
		testutil.Entry{Name: "404.html", Body: "custom missing"},
	)
	files, size, err := s.Deploy("blog", bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if files != 4 || size != int64(len("<h1>home</h1>docsconsole.log(1)custom missing")) {
		t.Fatalf("got files=%d bytes=%d", files, size)
	}

	if w := get(s, "blog", "/"); w.Code != 200 || w.Body.String() != "<h1>home</h1>" {
		t.Errorf("/ = %d %q", w.Code, w.Body)
	}
	if w := get(s, "blog", "/app.js"); w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), "javascript") {
		t.Errorf("/app.js = %d %q", w.Code, w.Header().Get("Content-Type"))
	}
	if w := get(s, "blog", "/app.js"); w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if w := get(s, "blog", "/docs"); w.Code != http.StatusMovedPermanently || w.Header().Get("Location") != "/docs/" {
		t.Errorf("/docs = %d %q", w.Code, w.Header().Get("Location"))
	}
	if w := get(s, "blog", "/docs?x=1"); w.Header().Get("Location") != "/docs/?x=1" {
		t.Errorf("/docs?x=1 redirect lost query: %q", w.Header().Get("Location"))
	}
	if w := get(s, "blog", "/docs/"); w.Code != 200 || w.Body.String() != "docs" {
		t.Errorf("/docs/ = %d %q", w.Code, w.Body)
	}
	if w := get(s, "blog", "/nope"); w.Code != 404 || w.Body.String() != "custom missing" {
		t.Errorf("/nope = %d %q", w.Code, w.Body)
	}
	if w := get(s, "missing", "/"); w.Code != 404 {
		t.Errorf("unknown site = %d", w.Code)
	}
}

func TestServeBlocksHiddenFiles(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "hi"}))); err != nil {
		t.Fatal(err)
	}
	// Plant a dotfile directly on disk, as if put there some other way.
	if err := os.WriteFile(filepath.Join(s.dir("blog"), ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := get(s, "blog", "/.env"); w.Code != 404 || strings.Contains(w.Body.String(), "SECRET") {
		t.Errorf("/.env = %d %q", w.Code, w.Body)
	}
}

func TestRedeployReplacesWholeSite(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "v1", "old.html": "old"}))); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "v2"}))); err != nil {
		t.Fatal(err)
	}
	if w := get(s, "blog", "/"); w.Body.String() != "v2" {
		t.Errorf("/ = %q, want v2", w.Body)
	}
	if w := get(s, "blog", "/old.html"); w.Code != 404 {
		t.Errorf("old file still served: %d", w.Code)
	}
	if entries, _ := os.ReadDir(s.tmp); len(entries) != 0 {
		t.Errorf("tmp not cleaned up: %d entries", len(entries))
	}
}

func TestFailedDeployKeepsPreviousVersion(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "v1"}))); err != nil {
		t.Fatal(err)
	}
	bad := testutil.TarGz(t, testutil.Entry{Name: "index.html", Body: "v2"}, testutil.Entry{Name: "../escape", Body: "x"})
	if _, _, err := s.Deploy("blog", bytes.NewReader(bad)); err == nil {
		t.Fatal("expected error")
	}
	if w := get(s, "blog", "/"); w.Body.String() != "v1" {
		t.Errorf("/ = %q, want v1 after failed deploy", w.Body)
	}
}

func TestDeployRejects(t *testing.T) {
	tests := map[string][]byte{
		"not gzip":   []byte("plain text"),
		"traversal":  testutil.TarGz(t, testutil.Entry{Name: "../../etc/cron.d/x", Body: "x"}),
		"absolute":   testutil.TarGz(t, testutil.Entry{Name: "/etc/passwd", Body: "x"}),
		"hidden":     testutil.TarGz(t, testutil.Entry{Name: ".env", Body: "x"}),
		"empty":      testutil.TarGz(t),
		"only links": testutil.TarGz(t, testutil.Entry{Name: "link", Type: tar.TypeSymlink, Link: "/etc/passwd"}),
	}
	for name, archive := range tests {
		t.Run(name, func(t *testing.T) {
			s := newStore(t)
			_, _, err := s.Deploy("blog", bytes.NewReader(archive))
			var de *DeployError
			if !errors.As(err, &de) {
				t.Fatalf("got %v, want DeployError", err)
			}
			if s.Exists("blog") {
				t.Error("site created despite rejected deploy")
			}
		})
	}
}

func TestDeploySkipsSymlinks(t *testing.T) {
	s := newStore(t)
	archive := testutil.TarGz(t,
		testutil.Entry{Name: "index.html", Body: "hi"},
		testutil.Entry{Name: "passwd", Type: tar.TypeSymlink, Link: "/etc/passwd"},
	)
	files, _, err := s.Deploy("blog", bytes.NewReader(archive))
	if err != nil || files != 1 {
		t.Fatalf("files=%d err=%v", files, err)
	}
	if _, err := os.Lstat(filepath.Join(s.dir("blog"), "passwd")); !os.IsNotExist(err) {
		t.Error("symlink was written")
	}
}

func TestDeployLimits(t *testing.T) {
	s, err := NewStore(t.TempDir(), 10, 2)
	if err != nil {
		t.Fatal(err)
	}
	var de *DeployError
	_, _, err = s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"a": "1", "b": "2", "c": "3"})))
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "files") {
		t.Errorf("too many files: %v", err)
	}
	_, _, err = s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"a": "123456", "b": "123456"})))
	if !errors.As(err, &de) || !strings.Contains(err.Error(), "larger") {
		t.Errorf("too large: %v", err)
	}
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"a": "1234567890"}))); err != nil {
		t.Errorf("exactly at limit: %v", err)
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	if err := s.Delete("blog"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("delete missing = %v", err)
	}
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "hi"}))); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("blog"); err != nil {
		t.Fatal(err)
	}
	if s.Exists("blog") {
		t.Error("site still exists")
	}
}

func TestNewStoreCleansStaleTmp(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "tmp", "deploy-blog-123")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, 1<<20, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale staging dir not removed")
	}
}
