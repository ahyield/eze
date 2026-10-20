// Package sites stores each site as a plain directory of files and serves them.
package sites

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var reserved = map[string]bool{"www": true, "api": true, "admin": true, "auth": true, "login": true, "static": true}

func ValidName(name string) bool { return nameRe.MatchString(name) && !reserved[name] }

type Store struct {
	root string // <data>/sites
	tmp  string // <data>/tmp, same filesystem as root so renames are atomic

	MaxSiteBytes int64
	MaxSiteFiles int

	mu    sync.Mutex
	locks map[string]*sync.Mutex
}

func NewStore(dataDir string, maxBytes int64, maxFiles int) (*Store, error) {
	s := &Store{
		root:         filepath.Join(dataDir, "sites"),
		tmp:          filepath.Join(dataDir, "tmp"),
		MaxSiteBytes: maxBytes,
		MaxSiteFiles: maxFiles,
		locks:        map[string]*sync.Mutex{},
	}
	for _, d := range []string{s.root, s.tmp} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	// Leftovers from deploys interrupted by a crash.
	entries, _ := os.ReadDir(s.tmp)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(s.tmp, e.Name()))
	}
	return s, nil
}

func (s *Store) dir(name string) string { return filepath.Join(s.root, name) }

func (s *Store) Exists(name string) bool {
	fi, err := os.Stat(s.dir(name))
	return err == nil && fi.IsDir()
}

func (s *Store) lock(name string) func() {
	s.mu.Lock()
	l, ok := s.locks[name]
	if !ok {
		l = &sync.Mutex{}
		s.locks[name] = l
	}
	s.mu.Unlock()
	l.Lock()
	return l.Unlock
}

type DeployError struct{ msg string }

func (e *DeployError) Error() string { return e.msg }

func deployErr(format string, args ...any) error { return &DeployError{fmt.Sprintf(format, args...)} }

// readErr reports a corrupt archive as a DeployError, but passes an upload size limit
// through untouched so the handler can answer 413 instead of 400.
func readErr(err error) error {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return err
	}
	return deployErr("reading archive: %v", err)
}

// Deploy extracts a .tar.gz or .zip archive into a staging directory and swaps it in place of the
// current site, so visitors never see a half-written site.
func (s *Store) Deploy(name string, archive io.Reader) (files int, bytes int64, err error) {
	staging, err := os.MkdirTemp(s.tmp, "deploy-"+name+"-")
	if err != nil {
		return 0, 0, err
	}
	defer os.RemoveAll(staging)

	if files, bytes, err = s.extract(staging, archive); err != nil {
		return 0, 0, err
	}
	if files == 0 {
		return 0, 0, deployErr("archive contains no files")
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return 0, 0, err
	}

	unlock := s.lock(name)
	defer unlock()

	target := s.dir(name)
	old := ""
	if s.Exists(name) {
		if old, err = os.MkdirTemp(s.tmp, "old-"+name+"-"); err != nil {
			return 0, 0, err
		}
		// MkdirTemp creates the directory; Rename needs the destination to not exist.
		os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return 0, 0, err
		}
	}
	if err := os.Rename(staging, target); err != nil {
		if old != "" {
			os.Rename(old, target) // put the previous version back
		}
		return 0, 0, err
	}
	if old != "" {
		os.RemoveAll(old)
	}
	return files, bytes, nil
}

func (s *Store) Delete(name string) error {
	unlock := s.lock(name)
	defer unlock()
	if !s.Exists(name) {
		return os.ErrNotExist
	}
	old, err := os.MkdirTemp(s.tmp, "rm-"+name+"-")
	if err != nil {
		return err
	}
	os.Remove(old)
	if err := os.Rename(s.dir(name), old); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

// extract unpacks a .tar.gz or .zip upload, detected from its first bytes.
func (s *Store) extract(dst string, archive io.Reader) (files int, total int64, err error) {
	br := bufio.NewReader(archive)
	magic, _ := br.Peek(4)
	w := &writer{dst: dst, maxFiles: s.MaxSiteFiles, maxBytes: s.MaxSiteBytes}
	switch {
	case bytes.HasPrefix(magic, []byte{0x1f, 0x8b}):
		err = w.tarGz(br)
	case bytes.HasPrefix(magic, []byte("PK\x03\x04")), bytes.HasPrefix(magic, []byte("PK\x05\x06")):
		err = s.zip(w, br)
	default:
		err = deployErr("upload must be a .tar.gz or .zip archive")
	}
	return w.files, w.total, err
}

// writer writes extracted files under dst, enforcing the site's file and size limits.
type writer struct {
	dst      string
	maxFiles int
	maxBytes int64
	files    int // regular files, as reported for the site
	entries  int // files and folders, which both count toward maxFiles
	total    int64
}

func (w *writer) add(rel string, r io.Reader) error {
	w.files++
	w.entries++
	if w.entries > w.maxFiles {
		return deployErr("site has more than %d files and folders", w.maxFiles)
	}
	full := filepath.Join(w.dst, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return deployErr("conflicting paths in archive at %q", rel)
	}
	f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return deployErr("conflicting paths in archive at %q", rel)
	}
	// Never trust sizes declared in the archive: count what actually comes out.
	n, err := io.Copy(f, io.LimitReader(r, w.maxBytes-w.total+1))
	cerr := f.Close()
	if err != nil {
		return readErr(err)
	}
	if cerr != nil {
		return cerr
	}
	w.total += n
	if w.total > w.maxBytes {
		return deployErr("site is larger than %d bytes", w.maxBytes)
	}
	return nil
}

func (w *writer) tarGz(r io.Reader) error {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return readErr(err)
	}
	defer gz.Close()
	// Everything decompressed counts, including headers and skipped entries, so a small
	// upload can't make the server inflate gigabytes it then throws away.
	limit := w.maxBytes + int64(w.maxFiles+1)*1024 + 1<<20
	tr := tar.NewReader(&capReader{r: gz, left: limit})
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return readErr(err)
		}
		if isClutter(hdr.Name) {
			continue // e.g. the ._* AppleDouble entries macOS tar adds
		}
		rel, ok := cleanPath(hdr.Name)
		if !ok {
			return deployErr("invalid path in archive: %q", hdr.Name)
		}
		if rel == "" {
			continue
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			w.entries++ // directories cost inodes too
			if w.entries > w.maxFiles {
				return deployErr("site has more than %d files and folders", w.maxFiles)
			}
			if err := os.MkdirAll(filepath.Join(w.dst, filepath.FromSlash(rel)), 0o755); err != nil {
				return deployErr("conflicting paths in archive at %q", rel)
			}
		case tar.TypeReg:
			if err := w.add(rel, tr); err != nil {
				return err
			}
		default:
			// Symlinks, devices and the like are silently skipped: a site is just files.
		}
	}
}

// capReader fails once more than left bytes have been read.
type capReader struct {
	r    io.Reader
	left int64
}

func (c *capReader) Read(p []byte) (int, error) {
	if c.left <= 0 {
		return 0, deployErr("archive expands to more data than a site may hold")
	}
	if int64(len(p)) > c.left {
		p = p[:c.left]
	}
	n, err := c.r.Read(p)
	c.left -= int64(n)
	return n, err
}

// zip handles archives made by "Compress" in Finder or Explorer, or downloaded from
// elsewhere. OS clutter (__MACOSX, ._*, .DS_Store) is skipped, as for tarballs, and a
// single folder wrapping everything is unwrapped.
func (s *Store) zip(w *writer, r io.Reader) error {
	// zip needs random access, so spool the upload to disk first; its size is already
	// capped by the caller's request body limit.
	tmp, err := os.CreateTemp(s.tmp, "upload-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	size, err := io.Copy(tmp, r)
	if err != nil {
		return readErr(err)
	}
	zr, err := zip.NewReader(tmp, size)
	if err != nil {
		return deployErr("not a valid zip archive")
	}

	type entry struct {
		rel string
		f   *zip.File
	}
	var entries []entry
	for _, f := range zr.File {
		if strings.HasSuffix(f.Name, "/") || f.Mode()&fs.ModeSymlink != 0 || isClutter(f.Name) {
			continue
		}
		rel, ok := cleanPath(f.Name)
		if !ok {
			return deployErr("invalid path in archive: %q", f.Name)
		}
		if rel != "" {
			entries = append(entries, entry{rel, f})
		}
	}

	prefix := wrapperDir(len(entries), func(i int) string { return entries[i].rel })
	for _, e := range entries {
		rc, err := e.f.Open()
		if err != nil {
			return deployErr("reading %q from zip: %v", e.f.Name, err)
		}
		err = w.add(strings.TrimPrefix(e.rel, prefix), rc)
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// isClutter matches files operating systems add to archives: __MACOSX resource forks and
// hidden files such as .DS_Store or ._index.html.
func isClutter(name string) bool {
	for seg := range strings.SplitSeq(strings.ReplaceAll(name, "\\", "/"), "/") {
		if seg == "__MACOSX" || (strings.HasPrefix(seg, ".") && seg != "." && seg != ".." && seg != ".well-known") {
			return true
		}
	}
	return false
}

// wrapperDir returns "dir/" when every path sits inside one top-level folder and there is
// no top-level index.html, i.e. someone zipped the folder rather than its contents.
func wrapperDir(n int, path func(int) string) string {
	if n == 0 {
		return ""
	}
	first, _, ok := strings.Cut(path(0), "/")
	if !ok {
		return ""
	}
	for i := 1; i < n; i++ {
		dir, _, ok := strings.Cut(path(i), "/")
		if !ok || dir != first {
			return ""
		}
	}
	return first + "/"
}

// cleanPath normalises an archive or URL path into a relative slash path, rejecting
// anything that escapes the root or touches hidden files (except .well-known).
func cleanPath(p string) (string, bool) {
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", false
	}
	for seg := range strings.SplitSeq(p, "/") {
		if seg == ".." {
			return "", false
		}
	}
	p = path.Clean("/" + p)[1:]
	for seg := range strings.SplitSeq(p, "/") {
		if strings.HasPrefix(seg, ".") && seg != ".well-known" {
			return "", false
		}
	}
	return p, true
}

// Serve responds with a static file from the named site, falling back to index.html for
// directories and the site's own 404.html for missing files.
func (s *Store) Serve(w http.ResponseWriter, r *http.Request, name string) {
	if !s.Exists(name) {
		http.Error(w, "No site named "+name+" yet.", http.StatusNotFound)
		return
	}
	root := s.dir(name)
	rel, ok := cleanPath(strings.TrimPrefix(r.URL.Path, "/"))
	if !ok {
		s.notFound(w, r, root)
		return
	}
	full := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Stat(full)
	if err == nil && fi.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			target := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		full = filepath.Join(full, "index.html")
		fi, err = os.Stat(full)
	}
	if err != nil || fi.IsDir() {
		s.notFound(w, r, root)
		return
	}
	serveFile(w, r, full, fi, http.StatusOK)
}

func (s *Store) notFound(w http.ResponseWriter, r *http.Request, root string) {
	custom := filepath.Join(root, "404.html")
	if fi, err := os.Stat(custom); err == nil && !fi.IsDir() {
		serveFile(w, r, custom, fi, http.StatusNotFound)
		return
	}
	http.NotFound(w, r)
}

func serveFile(w http.ResponseWriter, r *http.Request, full string, fi os.FileInfo, status int) {
	f, err := os.Open(full)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if status != http.StatusOK {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		io.Copy(w, f)
		return
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), f)
}
