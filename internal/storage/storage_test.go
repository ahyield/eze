package storage

import (
	"context"
	"io"
	"os"
	"runtime"
	"strings"
	"testing"

	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"
)

func TestReader(t *testing.T) {
	ctx := context.Background()
	b := memblob.OpenBucket(nil)
	defer b.Close()
	if err := b.WriteAll(ctx, "k", []byte("0123456789"), nil); err != nil {
		t.Fatal(err)
	}

	r := NewReader(ctx, b, "k", 10)
	defer r.Close()
	if n, _ := r.Seek(0, io.SeekEnd); n != 10 {
		t.Errorf("SeekEnd = %d", n)
	}
	if n, _ := r.Seek(-4, io.SeekEnd); n != 6 {
		t.Errorf("Seek(-4, End) = %d", n)
	}
	if got, _ := io.ReadAll(r); string(got) != "6789" {
		t.Errorf("read from 6 = %q", got)
	}
	if _, err := r.Seek(2, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(r, buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != "234" {
		t.Errorf("read from 2 = %q", buf)
	}
	if n, _ := r.Seek(1, io.SeekCurrent); n != 6 {
		t.Errorf("Seek(1, Current) = %d", n)
	}
	if _, err := r.Seek(-1, io.SeekStart); err == nil {
		t.Error("seek before start allowed")
	}

	missing := NewReader(ctx, b, "nope", 5)
	if _, err := io.ReadAll(missing); !IsNotFound(err) {
		t.Errorf("missing object err = %v", err)
	}
}

func TestDeletePrefix(t *testing.T) {
	ctx := context.Background()
	b := memblob.OpenBucket(nil)
	defer b.Close()
	for _, k := range []string{"a/1", "a/2/3", "ab/1", "b/1"} {
		if err := b.WriteAll(ctx, k, []byte("x"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := DeletePrefix(ctx, b, "a/"); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]bool{"a/1": false, "a/2/3": false, "ab/1": true, "b/1": true} {
		if ok, _ := b.Exists(ctx, k); ok != want {
			t.Errorf("%s exists = %v, want %v", k, ok, want)
		}
	}
}

func TestProbe(t *testing.T) {
	ctx := context.Background()
	b := memblob.OpenBucket(nil)
	defer b.Close()
	if err := probe(ctx, b); err != nil {
		t.Fatal(err)
	}
	if ok, _ := b.Exists(ctx, probeKey); ok {
		t.Error("probe object left behind")
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	dir := t.TempDir()
	ro, err := fileblob.OpenBucket(dir, &fileblob.Options{NoTempDir: true})
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Error(err)
		}
	})
	if err := probe(ctx, ro); err == nil || !strings.Contains(err.Error(), "writing") {
		t.Errorf("read-only bucket: %v", err)
	}
}

func TestOpenRejectsUnknownScheme(t *testing.T) {
	if _, err := Open(context.Background(), "ftp://bucket"); err == nil {
		t.Error("ftp:// accepted")
	}
}

func TestDescribe(t *testing.T) {
	for in, want := range map[string]string{
		"s3://bucket?region=us-east-1&endpoint=https://x": "s3://bucket",
		"gs://bucket/":            "gs://bucket/",
		"s3://bucket?prefix=eze/": "s3://bucket",
	} {
		if got := Describe(in); got != want {
			t.Errorf("Describe(%q) = %q, want %q", in, got, want)
		}
	}
}
