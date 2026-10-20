package main

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gocloud.dev/blob/memblob"

	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/uploads"
)

func write(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestMigrate(t *testing.T) {
	ctx := context.Background()
	data := t.TempDir()
	write(t, filepath.Join(data, "sites", "blog", "index.html"), "blog home")
	write(t, filepath.Join(data, "sites", "blog", "css", "a.css"), "css")
	write(t, filepath.Join(data, "sites", "game", "index.html"), "game")
	write(t, filepath.Join(data, "uploads", "blog", "f1"), "photo")
	write(t, filepath.Join(data, "uploads", "blog", ".part-123"), "unfinished")
	write(t, filepath.Join(data, "uploads", "game", "f2"), "video")
	write(t, filepath.Join(data, "eze.db"), "not copied")

	b := memblob.OpenBucket(nil)
	defer b.Close()
	var out strings.Builder
	if err := migrate(ctx, data, b, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sites: 2 copied, 0 already") || !strings.Contains(out.String(), "uploads: 2 copied, 0 already") {
		t.Errorf("first run:\n%s", out.String())
	}

	// eze started on the bucket sees everything.
	ss, err := sites.NewStore(t.TempDir(), b, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{"/": "blog home", "/css/a.css": "css"} {
		w := httptest.NewRecorder()
		ss.Serve(w, httptest.NewRequest("GET", path, nil), "blog")
		if w.Body.String() != want {
			t.Errorf("blog %s = %d %q", path, w.Code, w.Body)
		}
	}
	up, _ := uploads.NewStore(t.TempDir(), b)
	f := up.Open(ctx, "game", "f2", 5)
	buf := make([]byte, 5)
	if _, err := f.Read(buf); err != nil || string(buf) != "video" {
		t.Errorf("upload = %q, %v", buf, err)
	}
	f.Close()
	if ok, _ := b.Exists(ctx, "uploads/blog/.part-123"); ok {
		t.Error("unfinished upload was copied")
	}
	if ok, _ := b.Exists(ctx, "eze.db"); ok {
		t.Error("the database was copied")
	}

	out.Reset()
	if err := migrate(ctx, data, b, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sites: 0 copied, 2 already") || !strings.Contains(out.String(), "uploads: 0 copied, 2 already") {
		t.Errorf("second run should copy nothing:\n%s", out.String())
	}
	if _, err := os.Stat(filepath.Join(data, "sites", "blog", "index.html")); err != nil {
		t.Error("local copy was removed")
	}
}

func TestMigrateEmptyDataDir(t *testing.T) {
	b := memblob.OpenBucket(nil)
	defer b.Close()
	var out strings.Builder
	if err := migrate(context.Background(), t.TempDir(), b, false, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sites: 0 copied") {
		t.Errorf("output:\n%s", out.String())
	}
}
