// Package uploads stores files that people upload through a site, separate from the
// site's deployed files so a redeploy never wipes them.
package uploads

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"gocloud.dev/blob"
	"gocloud.dev/blob/fileblob"

	"github.com/ahyield/eze/internal/storage"
)

// ErrTooLarge means the upload went over the limit passed to Save.
var ErrTooLarge = errors.New("file too large")

// Store keeps each file at <site>/<id> in a bucket: <data>/uploads on local disk, or the
// uploads/ prefix of the EZE_STORAGE bucket.
type Store struct {
	b      *blob.Bucket
	prefix string
	dir    string // <data>/uploads in local mode, or ""
}

// NewStore uses bucket when it isn't nil, and <dataDir>/uploads otherwise.
func NewStore(dataDir string, bucket *blob.Bucket) (*Store, error) {
	if bucket != nil {
		return &Store{b: bucket, prefix: "uploads/"}, nil
	}
	dir := filepath.Join(dataDir, "uploads")
	b, err := fileblob.OpenBucket(dir, &fileblob.Options{
		CreateDir: true,
		NoTempDir: true, // the system temp dir may be on another filesystem, which breaks the final rename
		Metadata:  fileblob.MetadataDontWrite,
	})
	if err != nil {
		return nil, err
	}
	return &Store{b: b, dir: dir}, nil
}

func (s *Store) key(site, id string) string { return s.prefix + site + "/" + id }

// Save streams r into storage under id, failing with ErrTooLarge past max bytes. It returns
// the size and up to the first 512 bytes, for content sniffing. Callers validate site and id.
func (s *Store) Save(ctx context.Context, site, id string, r io.Reader, max int64) (size int64, head []byte, err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w, err := s.b.NewWriter(ctx, s.key(site, id), nil)
	if err != nil {
		return 0, nil, err
	}
	sniff := &headWriter{max: 512}
	size, err = io.Copy(io.MultiWriter(w, sniff), io.LimitReader(r, max+1))
	if err == nil && size > max {
		err = ErrTooLarge
	}
	if err != nil {
		cancel() // makes Close abort the write, so nothing is stored
		w.Close()
		return 0, nil, err
	}
	if err := w.Close(); err != nil {
		return 0, nil, err
	}
	return size, sniff.buf, nil
}

// Open returns the file's contents. size comes from the file's record, so opening costs no request.
func (s *Store) Open(ctx context.Context, site, id string, size int64) io.ReadSeekCloser {
	return storage.NewReader(ctx, s.b, s.key(site, id), size)
}

func (s *Store) Delete(ctx context.Context, site, id string) error {
	err := s.b.Delete(ctx, s.key(site, id))
	if storage.IsNotFound(err) {
		return nil
	}
	return err
}

func (s *Store) DeleteSite(ctx context.Context, site string) error {
	if s.dir != "" {
		return os.RemoveAll(filepath.Join(s.dir, site))
	}
	return storage.DeletePrefix(ctx, s.b, s.prefix+site+"/")
}

type headWriter struct {
	buf []byte
	max int
}

func (h *headWriter) Write(p []byte) (int, error) {
	if room := h.max - len(h.buf); room > 0 {
		h.buf = append(h.buf, p[:min(room, len(p))]...)
	}
	return len(p), nil
}

// CleanName turns a client-supplied filename into something safe to store and show:
// no directories, no control characters, at most 200 bytes, never empty.
func CleanName(name string) string {
	name = path.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' || r == utf8.RuneError {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "file"
	}
	if len(name) > 200 {
		ext := path.Ext(name)
		if len(ext) > 20 {
			ext = ""
		}
		stem := name[:200-len(ext)]
		for !utf8.ValidString(stem) {
			stem = stem[:len(stem)-1]
		}
		name = stem + ext
	}
	return name
}

// DetectType picks a content type from the filename, then the client's declared type,
// then the file's first bytes.
func DetectType(name, declared string, head []byte) string {
	if t := mime.TypeByExtension(path.Ext(name)); t != "" {
		return t
	}
	if mt, params, err := mime.ParseMediaType(declared); err == nil && mt != "application/octet-stream" {
		return mime.FormatMediaType(mt, params)
	}
	return http.DetectContentType(head)
}

// Inline reports whether a type is safe to display in the browser. Everything else
// (HTML, scripts, unknown types) is served as a download.
func Inline(contentType string) bool {
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return false
	}
	switch {
	case strings.HasPrefix(mt, "image/"), strings.HasPrefix(mt, "video/"), strings.HasPrefix(mt, "audio/"):
		return true
	}
	switch mt {
	case "application/pdf", "text/plain", "text/csv", "application/json":
		return true
	}
	return false
}
