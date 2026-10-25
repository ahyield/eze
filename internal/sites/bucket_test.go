package sites

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"
	"gocloud.dev/blob/memblob"

	"github.com/ahyield/eze/internal/testutil"
)

func bucketStore(t *testing.T, b *blob.Bucket) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir(), b, 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Wait)
	return s
}

func keys(t *testing.T, b *blob.Bucket, prefix string) []string {
	t.Helper()
	var out []string
	it := b.List(&blob.ListOptions{Prefix: prefix})
	for {
		obj, err := it.Next(context.Background())
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, obj.Key)
	}
}

func deploy(t *testing.T, s *Store, files map[string]string) {
	t.Helper()
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, files))); err != nil {
		t.Fatal(err)
	}
}

func TestBucketRangeRequest(t *testing.T) {
	s := bucketStore(t, memblob.OpenBucket(nil))
	deploy(t, s, map[string]string{"index.html": "home", "clip.mp4": "0123456789"})
	r := httptest.NewRequest("GET", "/clip.mp4", nil)
	r.Header.Set("Range", "bytes=3-5")
	w := httptest.NewRecorder()
	s.Serve(w, r, "blog")
	if w.Code != 206 || w.Body.String() != "345" || w.Header().Get("Content-Range") != "bytes 3-5/10" {
		t.Errorf("range = %d %q %q", w.Code, w.Body, w.Header().Get("Content-Range"))
	}
	if ct := w.Header().Get("Content-Type"); ct != "video/mp4" {
		t.Errorf("content type = %q", ct)
	}
}

func TestBucketRedeployRemovesOldFiles(t *testing.T) {
	b := memblob.OpenBucket(nil)
	ctx := context.Background()
	// Left by a deploy that crashed before writing its manifest.
	if err := b.WriteAll(ctx, "sites/blog/20200101T000000Z-dead/index.html", []byte("orphan"), nil); err != nil {
		t.Fatal(err)
	}
	s := bucketStore(t, b)
	deploy(t, s, map[string]string{"index.html": "v1", "old.html": "old"})
	deploy(t, s, map[string]string{"index.html": "v2"})
	s.Wait()

	got := keys(t, b, "sites/blog/")
	if len(got) != 2 || !slices.Contains(got, "sites/blog/current.json") || !strings.HasSuffix(got[0], "/index.html") {
		t.Errorf("bucket after redeploy = %v, want the live index.html and current.json", got)
	}
	if w := get(s, "blog", "/"); w.Body.String() != "v2" {
		t.Errorf("/ = %q", w.Body)
	}
}

func TestBucketDeleteHidesSiteAtOnce(t *testing.T) {
	b := memblob.OpenBucket(nil)
	s := bucketStore(t, b)
	deploy(t, s, map[string]string{"index.html": "hi"})

	unlock := s.lock("blog") // hold back the background cleanup
	if err := s.backend.remove("blog"); err != nil {
		t.Fatal(err)
	}
	if s.Exists("blog") || slices.Contains(keys(t, b, "sites/blog/"), "sites/blog/current.json") {
		t.Error("site still live after its manifest was deleted")
	}
	s.cleanupLater("blog")
	unlock()
	s.Wait()
	if left := keys(t, b, "sites/"); len(left) != 0 {
		t.Errorf("files left after delete: %v", left)
	}
}

func TestBucketSurvivesRestart(t *testing.T) {
	b := memblob.OpenBucket(nil)
	ctx := context.Background()
	deploy(t, bucketStore(t, b), map[string]string{"index.html": "home", "docs/index.html": "docs"})
	// A first deploy of another site that never got its manifest.
	b.WriteAll(ctx, "sites/ghost/20200101T000000Z-dead/index.html", []byte("x"), nil)

	s := bucketStore(t, b) // as if eze restarted
	if w := get(s, "blog", "/docs/"); w.Code != 200 || w.Body.String() != "docs" {
		t.Errorf("/docs/ after restart = %d %q", w.Code, w.Body)
	}
	if s.Exists("ghost") {
		t.Error("half-deployed site is live")
	}
	s.Wait()
	if left := keys(t, b, "sites/ghost/"); len(left) != 0 {
		t.Errorf("half-deployed site not cleaned up: %v", left)
	}
}

func TestBucketFailedUploadKeepsPreviousVersion(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory the test can't write to")
	}
	dir := t.TempDir()
	b, err := fileblob.OpenBucket(dir, &fileblob.Options{NoTempDir: true})
	if err != nil {
		t.Fatal(err)
	}
	s := bucketStore(t, b)
	deploy(t, s, map[string]string{"index.html": "v1"})
	s.Wait()

	// The bucket now refuses new files under sites/blog/.
	site := filepath.Join(dir, "sites", "blog")
	if err := os.Chmod(site, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(site, 0o755); err != nil {
			t.Error(err)
		}
	})
	if _, _, err := s.Deploy("blog", bytes.NewReader(testutil.Files(t, map[string]string{"index.html": "v2"}))); err == nil {
		t.Fatal("deploy succeeded on a read-only bucket")
	}
	if w := get(s, "blog", "/"); w.Code != 200 || w.Body.String() != "v1" {
		t.Errorf("/ after failed deploy = %d %q", w.Code, w.Body)
	}
}

func TestImport(t *testing.T) {
	src := t.TempDir()
	if err := os.MkdirAll(filepath.Join(src, "a"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": "home", "a/b.txt": "b"} {
		if err := os.WriteFile(filepath.Join(src, filepath.FromSlash(name)), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	s := bucketStore(t, memblob.OpenBucket(nil))
	if err := s.Import("blog", src); err != nil {
		t.Fatal(err)
	}
	if w := get(s, "blog", "/a/b.txt"); w.Body.String() != "b" {
		t.Errorf("/a/b.txt = %d %q", w.Code, w.Body)
	}
	if _, err := os.Stat(filepath.Join(src, "index.html")); err != nil {
		t.Error("import changed the source folder")
	}
}
