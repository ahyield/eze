// Package sites stores each site as a plain directory of files and serves them.
package sites

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

var nameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Reserved names can't be deployed to.
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

// Deploy extracts a gzipped tarball into a staging directory and swaps it in place of the
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

func (s *Store) extract(dst string, archive io.Reader) (files int, total int64, err error) {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return 0, 0, deployErr("upload must be a gzipped tar archive")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return files, total, nil
		}
		if err != nil {
			return 0, 0, readErr(err)
		}
		rel, ok := cleanPath(hdr.Name)
		if !ok {
			return 0, 0, deployErr("invalid path in archive: %q", hdr.Name)
		}
		if rel == "" {
			continue
		}
		full := filepath.Join(dst, filepath.FromSlash(rel))
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(full, 0o755); err != nil {
				return 0, 0, err
			}
		case tar.TypeReg:
			files++
			if files > s.MaxSiteFiles {
				return 0, 0, deployErr("site has more than %d files", s.MaxSiteFiles)
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return 0, 0, err
			}
			f, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				return 0, 0, err
			}
			remaining := s.MaxSiteBytes - total
			n, err := io.Copy(f, io.LimitReader(tr, remaining+1))
			cerr := f.Close()
			if err != nil {
				return 0, 0, readErr(err)
			}
			if cerr != nil {
				return 0, 0, cerr
			}
			total += n
			if total > s.MaxSiteBytes {
				return 0, 0, deployErr("site is larger than %d bytes", s.MaxSiteBytes)
			}
		default:
			// Symlinks, devices and the like are silently skipped: a site is just files.
		}
	}
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
