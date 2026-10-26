package uploads

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
	"unicode/utf8"

	"gocloud.dev/blob/memblob"
)

// stores returns a store on local disk and one in a memory bucket, which also holds sites.
func stores(t *testing.T) map[string]*Store {
	local, err := NewStore(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	b := memblob.OpenBucket(nil)
	t.Cleanup(func() { b.Close() })
	bucket, err := NewStore(t.TempDir(), b)
	if err != nil {
		t.Fatal(err)
	}
	return map[string]*Store{"local": local, "bucket": bucket}
}

func read(t *testing.T, s *Store, site, id string, size int64) (string, error) {
	t.Helper()
	f := s.Open(context.Background(), site, id, size)
	defer f.Close()
	b, err := io.ReadAll(f)
	return string(b), err
}

func TestSaveOpenDelete(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			body := strings.Repeat("x", 1000)
			size, head, err := s.Save(ctx, "blog", "f1", strings.NewReader(body), 1000)
			if err != nil || size != 1000 || len(head) != 512 {
				t.Fatalf("size=%d head=%d err=%v", size, len(head), err)
			}
			if got, err := read(t, s, "blog", "f1", size); err != nil || got != body {
				t.Errorf("read back %d bytes, err %v", len(got), err)
			}
			f := s.Open(ctx, "blog", "f1", size)
			if _, err := f.Seek(998, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			if tail, _ := io.ReadAll(f); string(tail) != "xx" {
				t.Errorf("after seek = %q", tail)
			}
			f.Close()
			if err := s.Delete(ctx, "blog", "f1"); err != nil {
				t.Fatal(err)
			}
			if err := s.Delete(ctx, "blog", "f1"); err != nil {
				t.Errorf("deleting a missing file should be a no-op: %v", err)
			}
			if _, err := read(t, s, "blog", "f1", size); err == nil {
				t.Error("deleted file still readable")
			}
		})
	}
}

func TestSaveTooLargeLeavesNothing(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			if _, _, err := s.Save(ctx, "blog", "f1", strings.NewReader("12345"), 4); !errors.Is(err, ErrTooLarge) {
				t.Fatalf("err = %v", err)
			}
			if ok, err := s.b.Exists(ctx, s.key("blog", "f1")); ok || err != nil {
				t.Errorf("file stored despite being too large (err %v)", err)
			}
		})
	}
}

func TestSaveReadErrorLeavesNothing(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			r := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(errors.New("connection reset")))
			if _, _, err := s.Save(ctx, "blog", "f1", r, 100); err == nil || !strings.Contains(err.Error(), "connection reset") {
				t.Fatalf("err = %v", err)
			}
			if ok, _ := s.b.Exists(ctx, s.key("blog", "f1")); ok {
				t.Error("partial file stored")
			}
		})
	}
}

func TestDeleteSite(t *testing.T) {
	ctx := context.Background()
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			for site, body := range map[string]string{"a": "x", "b": "y"} {
				if _, _, err := s.Save(ctx, site, "f1", strings.NewReader(body), 10); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.DeleteSite(ctx, "a"); err != nil {
				t.Fatal(err)
			}
			if _, err := read(t, s, "a", "f1", 1); err == nil {
				t.Error("site a file survived")
			}
			if got, err := read(t, s, "b", "f1", 1); err != nil || got != "y" {
				t.Errorf("site b file = %q, %v", got, err)
			}
		})
	}
}

// Files saved before uploads went through gocloud live at <data>/uploads/<site>/<id>, with
// no metadata files beside them. They must keep working, and new files must look the same.
func TestLocalLayoutUnchanged(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "uploads", "blog"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "uploads", "blog", "old"), []byte("from before"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := read(t, s, "blog", "old", 11); err != nil || got != "from before" {
		t.Errorf("old file = %q, %v", got, err)
	}
	if _, _, err := s.Save(context.Background(), "blog", "new", strings.NewReader("hi"), 10); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "uploads", "blog"))
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "new,old" {
		t.Errorf("files on disk = %v", names)
	}
}

func TestCleanName(t *testing.T) {
	for in, want := range map[string]string{
		"photo.png":           "photo.png",
		"../../etc/passwd":    "passwd",
		`C:\Users\ana\cv.pdf`: "cv.pdf",
		"dir/":                "dir",
		"":                    "file",
		"..":                  "file",
		"  spaced.txt  ":      "spaced.txt",
		"bad\x00name\n.txt":   "badname.txt",
		"résumé.pdf":          "résumé.pdf",
		"bad\xffbyte.txt":     "badbyte.txt",
	} {
		if got := CleanName(in); got != want {
			t.Errorf("CleanName(%q) = %q, want %q", in, got, want)
		}
	}
	long := CleanName(strings.Repeat("é", 150) + ".jpeg")
	if len(long) > 200 || !strings.HasSuffix(long, ".jpeg") || !utf8.ValidString(long) {
		t.Errorf("long name = %d bytes %q", len(long), long[len(long)-10:])
	}
}

func TestDetectType(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n")
	tests := []struct {
		name, declared string
		head           []byte
		want           string
	}{
		{"a.png", "", png, "image/png"},
		{"a.png", "text/html", nil, "image/png"}, // extension wins over the client's claim
		{"noext", "image/webp", nil, "image/webp"},
		{"noext", "application/octet-stream", png, "image/png"},
		{"noext", "", []byte("hello"), "text/plain; charset=utf-8"},
		{"noext", "garbage;;", png, "image/png"},
	}
	for _, tt := range tests {
		if got := DetectType(tt.name, tt.declared, tt.head); got != tt.want {
			t.Errorf("DetectType(%q, %q) = %q, want %q", tt.name, tt.declared, got, tt.want)
		}
	}
}

func TestInline(t *testing.T) {
	for ct, want := range map[string]bool{
		"image/png": true, "image/svg+xml": true, "video/mp4": true, "audio/mpeg": true,
		"application/pdf": true, "text/plain; charset=utf-8": true, "text/csv": true, "application/json": true,
		"text/html; charset=utf-8": false, "text/javascript": false, "application/xhtml+xml": false,
		"application/octet-stream": false, "application/zip": false, "": false,
	} {
		if got := Inline(ct); got != want {
			t.Errorf("Inline(%q) = %v, want %v", ct, got, want)
		}
	}
}
