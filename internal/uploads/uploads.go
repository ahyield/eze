// Package uploads stores files that people upload through a site, separate from the
// site's deployed files so a redeploy never wipes them.
package uploads

import (
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
)

// ErrTooLarge means the upload went over the limit passed to Save.
var ErrTooLarge = errors.New("file too large")

type Store struct {
	root string // <data>/uploads; each site gets <root>/<site>/<id>
}

func NewStore(dataDir string) (*Store, error) {
	root := filepath.Join(dataDir, "uploads")
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: root}, nil
}

func (s *Store) path(site, id string) string { return filepath.Join(s.root, site, id) }

// Save streams r to disk under id, failing with ErrTooLarge past max bytes. It returns
// the size and up to the first 512 bytes, for content sniffing. Callers validate site and id.
func (s *Store) Save(site, id string, r io.Reader, max int64) (size int64, head []byte, err error) {
	dir := filepath.Join(s.root, site)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return 0, nil, err
	}
	tmp, err := os.CreateTemp(dir, ".part-*")
	if err != nil {
		return 0, nil, err
	}
	defer func() {
		if err != nil {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()

	sniff := &headWriter{max: 512}
	size, err = io.Copy(io.MultiWriter(tmp, sniff), io.LimitReader(r, max+1))
	if err != nil {
		return 0, nil, err
	}
	if size > max {
		return 0, nil, ErrTooLarge
	}
	if err = tmp.Close(); err != nil {
		return 0, nil, err
	}
	if err = os.Rename(tmp.Name(), s.path(site, id)); err != nil {
		return 0, nil, err
	}
	return size, sniff.buf, nil
}

func (s *Store) Open(site, id string) (*os.File, error) { return os.Open(s.path(site, id)) }

func (s *Store) Delete(site, id string) error {
	err := os.Remove(s.path(site, id))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func (s *Store) DeleteSite(site string) error { return os.RemoveAll(filepath.Join(s.root, site)) }

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
