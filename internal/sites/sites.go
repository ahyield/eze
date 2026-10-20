// Package sites stores each site's deployed files, on local disk or in a bucket, and serves them.
package sites

import (
	"archive/tar"
	"archive/zip"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
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

	"gocloud.dev/blob"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

var reserved = map[string]bool{"www": true, "api": true, "admin": true, "auth": true, "login": true, "static": true}

func ValidName(name string) bool { return nameRe.MatchString(name) && !reserved[name] }

// backend holds the live version of each site. Deploys are unpacked into a local staging
// directory first, so a backend only has to swap a finished site in.
type backend interface {
	// publish makes the files in staging the live site, all at once.
	publish(name, staging string) error
	// remove deletes the site, returning os.ErrNotExist if there is none.
	remove(name string) error
	// cleanup deletes anything left of earlier versions. It runs in the background, under the site's lock.
	cleanup(name string)
	// current returns the live site's files.
	current(ctx context.Context, name string) (fs.FS, bool)
}

type Store struct {
	tmp     string // <data>/tmp, where uploads are unpacked
	backend backend

	MaxSiteBytes int64
	MaxSiteFiles int

	mu    sync.Mutex
	locks map[string]*sync.Mutex
	bg    sync.WaitGroup
}

// NewStore keeps sites in bucket when it isn't nil, and under <dataDir>/sites otherwise.
func NewStore(dataDir string, bucket *blob.Bucket, maxBytes int64, maxFiles int) (*Store, error) {
	s := &Store{
		tmp:          filepath.Join(dataDir, "tmp"),
		MaxSiteBytes: maxBytes,
		MaxSiteFiles: maxFiles,
		locks:        map[string]*sync.Mutex{},
	}
	if err := os.MkdirAll(s.tmp, 0o755); err != nil {
		return nil, err
	}
	// Leftovers from deploys interrupted by a crash.
	entries, _ := os.ReadDir(s.tmp)
	for _, e := range entries {
		os.RemoveAll(filepath.Join(s.tmp, e.Name()))
	}
	if bucket == nil {
		lb, err := newLocalBackend(filepath.Join(dataDir, "sites"), s.tmp)
		if err != nil {
			return nil, err
		}
		s.backend = lb
		return s, nil
	}
	bb, stale, err := newBucketBackend(context.Background(), bucket)
	if err != nil {
		return nil, err
	}
	s.backend = bb
	for _, name := range stale {
		s.cleanupLater(name)
	}
	return s, nil
}

func (s *Store) Exists(name string) bool {
	_, ok := s.backend.current(context.Background(), name)
	return ok
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

// cleanupLater removes a site's leftovers in the background. Taking the site's lock keeps it
// from touching the files of a deploy that is still uploading.
func (s *Store) cleanupLater(name string) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		unlock := s.lock(name)
		defer unlock()
		s.backend.cleanup(name)
	}()
}

// Wait blocks until background cleanups have finished.
func (s *Store) Wait() { s.bg.Wait() }

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

// Deploy unpacks a .tar.gz or .zip archive into a staging directory, then swaps it in place
// of the current site, so visitors never see a half-written site.
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

	unlock := s.lock(name)
	defer unlock()
	if err := s.backend.publish(name, staging); err != nil {
		return 0, 0, err
	}
	s.cleanupLater(name)
	return files, bytes, nil
}

// Import publishes an existing directory of site files as is, for moving local sites into a bucket.
func (s *Store) Import(name, dir string) error {
	unlock := s.lock(name)
	defer unlock()
	if err := s.backend.publish(name, dir); err != nil {
		return err
	}
	s.cleanupLater(name)
	return nil
}

func (s *Store) Delete(name string) error {
	unlock := s.lock(name)
	defer unlock()
	if err := s.backend.remove(name); err != nil {
		return err
	}
	s.cleanupLater(name)
	return nil
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
	fsys, ok := s.backend.current(r.Context(), name)
	if !ok {
		http.Error(w, "No site named "+name+" yet.", http.StatusNotFound)
		return
	}
	rel, ok := cleanPath(strings.TrimPrefix(r.URL.Path, "/"))
	if !ok {
		notFound(w, r, fsys)
		return
	}
	if rel == "" {
		rel = "."
	}
	fi, err := fs.Stat(fsys, rel)
	if err == nil && fi.IsDir() {
		if !strings.HasSuffix(r.URL.Path, "/") {
			target := r.URL.Path + "/"
			if r.URL.RawQuery != "" {
				target += "?" + r.URL.RawQuery
			}
			http.Redirect(w, r, target, http.StatusMovedPermanently)
			return
		}
		rel = path.Join(rel, "index.html")
		fi, err = fs.Stat(fsys, rel)
	}
	if err != nil || fi.IsDir() {
		notFound(w, r, fsys)
		return
	}
	serveFile(w, r, fsys, rel, fi, http.StatusOK)
}

func notFound(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	if fi, err := fs.Stat(fsys, "404.html"); err == nil && !fi.IsDir() {
		serveFile(w, r, fsys, "404.html", fi, http.StatusNotFound)
		return
	}
	http.NotFound(w, r)
}

func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, rel string, fi fs.FileInfo, status int) {
	f, err := fsys.Open(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	rs, seekable := f.(io.ReadSeeker)
	if status != http.StatusOK || !seekable {
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
		}
		w.WriteHeader(status)
		io.Copy(w, f)
		return
	}
	http.ServeContent(w, r, fi.Name(), fi.ModTime(), rs)
}
