package sites

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/ahyield/eze/internal/testutil"
)

type newStoreFunc func(t *testing.T, maxBytes int64, maxFiles int) *Store

// backends makes stores on local disk and in an in-memory bucket, so behaviour tests run on both.
var backends = map[string]newStoreFunc{
	"local": func(t *testing.T, maxBytes int64, maxFiles int) *Store {
		t.Helper()
		s, err := NewStore(t.TempDir(), nil, maxBytes, maxFiles)
		if err != nil {
			t.Fatal(err)
		}
		return s
	},
	"bucket": func(t *testing.T, maxBytes int64, maxFiles int) *Store {
		t.Helper()
		b := memblob.OpenBucket(nil)
		t.Cleanup(func() { b.Close() })
		s, err := NewStore(t.TempDir(), b, maxBytes, maxFiles)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Wait)
		return s
	},
}

func eachBackend(t *testing.T, test func(t *testing.T, newStore func() *Store, mk newStoreFunc)) {
	for name, mk := range backends {
		t.Run(name, func(t *testing.T) { test(t, func() *Store { return mk(t, 1<<20, 100) }, mk) })
	}
}

// newStore makes a store on local disk, for tests that look at the files it writes.
func newStore(t *testing.T) *Store { return backends["local"](t, 1<<20, 100) }

func localDir(s *Store, name string) string { return s.backend.(*localBackend).dir(name) }

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
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		s := newStore()
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
	})
}

func TestServeBlocksHiddenFiles(t *testing.T) {
	s := newStore(t)
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "hi"}))); err != nil {
		t.Fatal(err)
	}
	// Plant a dotfile directly on disk, as if put there some other way.
	if err := os.WriteFile(filepath.Join(localDir(s, "blog"), ".env"), []byte("SECRET=1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if w := get(s, "blog", "/.env"); w.Code != 404 || strings.Contains(w.Body.String(), "SECRET") {
		t.Errorf("/.env = %d %q", w.Code, w.Body)
	}
}

func TestRedeployReplacesWholeSite(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		s := newStore()
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
	})
}

func TestFailedDeployKeepsPreviousVersion(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		s := newStore()
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
	})
}

func TestDeployRejects(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
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
				s := newStore()
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
	})
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
	if _, err := os.Lstat(filepath.Join(localDir(s, "blog"), "passwd")); !os.IsNotExist(err) {
		t.Error("symlink was written")
	}
}

func TestDeployLimits(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		s := mk(t, 10, 2)
		var de *DeployError
		_, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"a": "1", "b": "2", "c": "3"})))
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
	})
}

func TestDelete(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		s := newStore()
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
	})
}

func TestNewStoreCleansStaleTmp(t *testing.T) {
	dir := t.TempDir()
	stale := filepath.Join(dir, "tmp", "deploy-blog-123")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(dir, nil, 1<<20, 100); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale staging dir not removed")
	}
}

func TestDeployZip(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		s := newStore()
		archive := testutil.Zip(t,
			testutil.Entry{Name: "index.html", Body: "zip home"},
			testutil.Entry{Name: "assets/", Body: ""},
			testutil.Entry{Name: "assets/app.js", Body: "js"},
		)
		files, size, err := s.Deploy("blog", bytes.NewReader(archive))
		if err != nil || files != 2 || size != int64(len("zip homejs")) {
			t.Fatalf("files=%d size=%d err=%v", files, size, err)
		}
		if w := get(s, "blog", "/"); w.Body.String() != "zip home" {
			t.Errorf("/ = %q", w.Body)
		}
		if w := get(s, "blog", "/assets/app.js"); w.Body.String() != "js" {
			t.Errorf("/assets/app.js = %q", w.Body)
		}
	})
}

func TestDeployZipFromFinder(t *testing.T) {
	// What macOS "Compress" produces for a folder: everything wrapped in the folder, plus clutter.
	s := newStore(t)
	archive := testutil.Zip(t,
		testutil.Entry{Name: "my-site/", Body: ""},
		testutil.Entry{Name: "my-site/index.html", Body: "home"},
		testutil.Entry{Name: "my-site/.DS_Store", Body: "junk"},
		testutil.Entry{Name: "my-site/css/site.css", Body: "css"},
		testutil.Entry{Name: "__MACOSX/my-site/._index.html", Body: "fork"},
		testutil.Entry{Name: "my-site/link", Type: tar.TypeSymlink, Body: "/etc/passwd"},
	)
	files, _, err := s.Deploy("blog", bytes.NewReader(archive))
	if err != nil || files != 2 {
		t.Fatalf("files=%d err=%v", files, err)
	}
	if w := get(s, "blog", "/"); w.Body.String() != "home" {
		t.Errorf("wrapper folder not unwrapped: / = %d %q", w.Code, w.Body)
	}
	if w := get(s, "blog", "/css/site.css"); w.Body.String() != "css" {
		t.Errorf("/css/site.css = %q", w.Body)
	}
	for _, p := range []string{"__MACOSX", ".DS_Store", "link"} {
		if _, err := os.Lstat(filepath.Join(localDir(s, "blog"), p)); !os.IsNotExist(err) {
			t.Errorf("%s was extracted", p)
		}
	}
}

func TestWrapperDir(t *testing.T) {
	for _, tt := range []struct {
		paths []string
		want  string
	}{
		{[]string{"site/index.html", "site/a.js"}, "site/"},
		{[]string{"site/index.html"}, "site/"},
		{[]string{"index.html", "site/a.js"}, ""},
		{[]string{"a/index.html", "b/index.html"}, ""},
		{[]string{"index.html"}, ""},
		{nil, ""},
	} {
		if got := wrapperDir(len(tt.paths), func(i int) string { return tt.paths[i] }); got != tt.want {
			t.Errorf("wrapperDir(%v) = %q, want %q", tt.paths, got, tt.want)
		}
	}
}

func TestDeployZipRejects(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		tests := map[string][]byte{
			"traversal":    testutil.Zip(t, testutil.Entry{Name: "../../etc/cron.d/x", Body: "x"}),
			"absolute":     testutil.Zip(t, testutil.Entry{Name: "/etc/passwd", Body: "x"}),
			"only clutter": testutil.Zip(t, testutil.Entry{Name: ".DS_Store", Body: "x"}, testutil.Entry{Name: "__MACOSX/._a", Body: "x"}),
			"truncated":    testutil.Zip(t, testutil.Entry{Name: "index.html", Body: "hello"})[:30],
			"unknown":      []byte("GIF89a not an archive"),
		}
		for name, archive := range tests {
			t.Run(name, func(t *testing.T) {
				s := newStore()
				var de *DeployError
				if _, _, err := s.Deploy("blog", bytes.NewReader(archive)); !errors.As(err, &de) {
					t.Fatalf("got %v, want DeployError", err)
				}
				if s.Exists("blog") {
					t.Error("site created")
				}
				if entries, _ := os.ReadDir(s.tmp); len(entries) != 0 {
					t.Errorf("tmp not cleaned: %d entries", len(entries))
				}
			})
		}
	})
}

func TestDeployZipLimits(t *testing.T) {
	eachBackend(t, func(t *testing.T, newStore func() *Store, mk newStoreFunc) {
		s := mk(t, 10, 2)
		var de *DeployError
		_, _, err := s.Deploy("blog", bytes.NewReader(testutil.Zip(t,
			testutil.Entry{Name: "a", Body: "1"}, testutil.Entry{Name: "b", Body: "2"}, testutil.Entry{Name: "c", Body: "3"})))
		if !errors.As(err, &de) || !strings.Contains(err.Error(), "files") {
			t.Errorf("too many files: %v", err)
		}
		// A zip bomb: tiny compressed, large uncompressed. The limit counts real bytes.
		_, _, err = s.Deploy("blog", bytes.NewReader(testutil.Zip(t, testutil.Entry{Name: "a", Body: strings.Repeat("0", 1<<20)})))
		if !errors.As(err, &de) || !strings.Contains(err.Error(), "larger") {
			t.Errorf("zip bomb: %v", err)
		}
	})
}

func TestDeployTarFromMacTar(t *testing.T) {
	// bsdtar on macOS adds AppleDouble entries for extended attributes.
	s := newStore(t)
	archive := testutil.TarGz(t,
		testutil.Entry{Name: "./", Type: tar.TypeDir},
		testutil.Entry{Name: "./._.", Body: "xattrs"},
		testutil.Entry{Name: "./index.html", Body: "home"},
		testutil.Entry{Name: "./._index.html", Body: "xattrs"},
		testutil.Entry{Name: "./.env", Body: "SECRET=1"},
	)
	files, _, err := s.Deploy("blog", bytes.NewReader(archive))
	if err != nil || files != 1 {
		t.Fatalf("files=%d err=%v", files, err)
	}
	if w := get(s, "blog", "/"); w.Body.String() != "home" {
		t.Errorf("/ = %q", w.Body)
	}
	if _, err := os.Stat(filepath.Join(localDir(s, "blog"), ".env")); !os.IsNotExist(err) {
		t.Error(".env was extracted")
	}
}

func TestDeployTarLimitsCountEverything(t *testing.T) {
	var de *DeployError
	// Directories count toward the file limit.
	s, _ := NewStore(t.TempDir(), nil, 1<<20, 3)
	dirs := []testutil.Entry{}
	for i := range 5 {
		dirs = append(dirs, testutil.Entry{Name: fmt.Sprintf("d%d/", i), Type: tar.TypeDir})
	}
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.TarGz(t, dirs...))); !errors.As(err, &de) {
		t.Errorf("more folders than the limit: %v", err)
	}
	// Skipped entries still count toward how much may be decompressed.
	s, _ = NewStore(t.TempDir(), nil, 1000, 10)
	huge := testutil.TarGz(t,
		testutil.Entry{Name: "index.html", Body: "x"},
		testutil.Entry{Name: "._junk", Body: strings.Repeat("0", 4<<20)},
	)
	if _, _, err := s.Deploy("blog", bytes.NewReader(huge)); !errors.As(err, &de) || !strings.Contains(err.Error(), "expands") {
		t.Errorf("large skipped entry: %v", err)
	}
}
