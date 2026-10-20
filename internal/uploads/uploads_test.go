package uploads

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSaveOpenDelete(t *testing.T) {
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Repeat("x", 1000)
	size, head, err := s.Save("blog", "f1", strings.NewReader(body), 1000)
	if err != nil || size != 1000 || len(head) != 512 {
		t.Fatalf("size=%d head=%d err=%v", size, len(head), err)
	}
	f, err := s.Open("blog", "f1")
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(f)
	f.Close()
	if string(got) != body {
		t.Error("content mismatch")
	}
	if err := s.Delete("blog", "f1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete("blog", "f1"); err != nil {
		t.Errorf("deleting a missing file should be a no-op: %v", err)
	}
	if _, err := s.Open("blog", "f1"); !os.IsNotExist(err) {
		t.Errorf("open deleted = %v", err)
	}
}

func TestSaveTooLargeLeavesNothing(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	if _, _, err := s.Save("blog", "f1", strings.NewReader("12345"), 4); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(s.root, "blog"))
	if len(entries) != 0 {
		t.Errorf("leftover files: %v", entries)
	}
}

func TestDeleteSite(t *testing.T) {
	s, _ := NewStore(t.TempDir())
	s.Save("a", "f1", strings.NewReader("x"), 10)
	s.Save("b", "f1", strings.NewReader("y"), 10)
	if err := s.DeleteSite("a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Open("a", "f1"); !os.IsNotExist(err) {
		t.Error("site a file survived")
	}
	if f, err := s.Open("b", "f1"); err != nil {
		t.Error("site b file removed")
	} else {
		f.Close()
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
