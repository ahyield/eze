package sites

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log/slog"
	"mime"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gocloud.dev/blob"
	"golang.org/x/sync/errgroup"

	"github.com/ahyield/eze/internal/storage"
)

// bucketBackend keeps each deploy under sites/<name>/<deploy>/ in a bucket. A manifest at
// sites/<name>/current.json names the live deploy and lists its files; writing it is a
// single atomic PUT, which is what switches a site to a new deploy.
//
// Every manifest is held in memory. eze runs as a single instance and all writes go
// through it, so the cache stays correct, and serving a request never has to list or stat
// anything in the bucket: only the file's own bytes are fetched.
type bucketBackend struct {
	b *blob.Bucket // shared with uploads, so every key here starts with sites/

	mu        sync.RWMutex
	manifests map[string]*manifest
}

type manifest struct {
	Deploy string           `json:"deploy"`
	Time   time.Time        `json:"time"`
	Files  map[string]int64 `json:"files"` // slash path -> size

	dirs map[string]bool
}

func manifestKey(name string) string { return "sites/" + name + "/current.json" }

func deployPrefix(name, deploy string) string { return "sites/" + name + "/" + deploy + "/" }

const uploadWorkers = 8

func (m *manifest) index() {
	m.dirs = map[string]bool{".": true}
	for p := range m.Files {
		for d := path.Dir(p); d != "."; d = path.Dir(d) {
			m.dirs[d] = true
		}
	}
}

// newBucketBackend loads every site's manifest. It also returns every site name found,
// so the caller can clear out leftovers from deploys a crash interrupted.
func newBucketBackend(ctx context.Context, bucket *blob.Bucket) (*bucketBackend, []string, error) {
	bb := &bucketBackend{b: bucket, manifests: map[string]*manifest{}}
	var names []string
	it := bb.b.List(&blob.ListOptions{Prefix: "sites/", Delimiter: "/"})
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if obj.IsDir {
			names = append(names, strings.TrimSuffix(strings.TrimPrefix(obj.Key, "sites/"), "/"))
		}
	}

	var mu sync.Mutex
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(16)
	for _, name := range names {
		g.Go(func() error {
			data, err := bb.b.ReadAll(gctx, manifestKey(name))
			if storage.IsNotFound(err) {
				return nil // a first deploy that never finished; cleanup removes it
			}
			if err != nil {
				return err
			}
			var m manifest
			if err := json.Unmarshal(data, &m); err != nil {
				return errors.New(manifestKey(name) + ": " + err.Error())
			}
			m.index()
			mu.Lock()
			bb.manifests[name] = &m
			mu.Unlock()
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	return bb, names, nil
}

func newDeployID() string {
	var b [4]byte
	rand.Read(b[:])
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(b[:])
}

func (bb *bucketBackend) publish(name, staging string) error {
	ctx := context.Background()
	m := &manifest{Deploy: newDeployID(), Time: time.Now().UTC().Truncate(time.Second), Files: map[string]int64{}}
	err := filepath.WalkDir(staging, func(p string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(staging, p)
		if err != nil {
			return err
		}
		m.Files[filepath.ToSlash(rel)] = info.Size()
		return nil
	})
	if err != nil {
		return err
	}

	prefix := deployPrefix(name, m.Deploy)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(uploadWorkers)
	for rel := range m.Files {
		g.Go(func() error { return bb.upload(gctx, filepath.Join(staging, filepath.FromSlash(rel)), prefix+rel) })
	}
	if err := g.Wait(); err != nil {
		storage.DeletePrefix(ctx, bb.b, prefix)
		return err
	}

	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	// If this fails the PUT may still have landed, so the new files are left for cleanup
	// rather than deleted out from under a manifest that might point at them.
	if err := bb.b.WriteAll(ctx, manifestKey(name), data, &blob.WriterOptions{ContentType: "application/json"}); err != nil {
		return err
	}
	m.index()
	bb.mu.Lock()
	bb.manifests[name] = m
	bb.mu.Unlock()
	return nil
}

func (bb *bucketBackend) upload(ctx context.Context, src, key string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w, err := bb.b.NewWriter(ctx, key, &blob.WriterOptions{ContentType: mime.TypeByExtension(path.Ext(key))})
	if err != nil {
		return err
	}
	if _, err := io.Copy(w, f); err != nil {
		cancel()
		w.Close()
		return err
	}
	return w.Close()
}

func (bb *bucketBackend) remove(name string) error {
	bb.mu.RLock()
	_, ok := bb.manifests[name]
	bb.mu.RUnlock()
	if !ok {
		return os.ErrNotExist
	}
	if err := bb.b.Delete(context.Background(), manifestKey(name)); err != nil && !storage.IsNotFound(err) {
		return err
	}
	bb.mu.Lock()
	delete(bb.manifests, name)
	bb.mu.Unlock()
	return nil
}

// cleanup deletes every deploy of the site except the live one.
func (bb *bucketBackend) cleanup(name string) {
	ctx := context.Background()
	keep := ""
	bb.mu.RLock()
	if m, ok := bb.manifests[name]; ok {
		keep = deployPrefix(name, m.Deploy)
	}
	bb.mu.RUnlock()

	var stale []string
	it := bb.b.List(&blob.ListOptions{Prefix: "sites/" + name + "/", Delimiter: "/"})
	for {
		obj, err := it.Next(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			slog.Error("cleaning up old deploys", "site", name, "err", err)
			return
		}
		if obj.IsDir && obj.Key != keep {
			stale = append(stale, obj.Key)
		}
	}
	for _, prefix := range stale {
		if err := storage.DeletePrefix(ctx, bb.b, prefix); err != nil {
			slog.Error("cleaning up old deploys", "site", name, "err", err)
			return
		}
	}
}

func (bb *bucketBackend) current(ctx context.Context, name string) (fs.FS, bool) {
	bb.mu.RLock()
	m, ok := bb.manifests[name]
	bb.mu.RUnlock()
	if !ok {
		return nil, false
	}
	return &manifestFS{ctx: ctx, b: bb.b, prefix: deployPrefix(name, m.Deploy), m: m}, true
}

// manifestFS serves one deploy, answering Stat from the manifest and reading file bytes from the bucket.
type manifestFS struct {
	ctx    context.Context
	b      *blob.Bucket
	prefix string
	m      *manifest
}

func (f *manifestFS) Stat(name string) (fs.FileInfo, error) {
	if !fs.ValidPath(name) {
		return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrInvalid}
	}
	if size, ok := f.m.Files[name]; ok {
		return &fileInfo{name: path.Base(name), size: size, mod: f.m.Time}, nil
	}
	if f.m.dirs[name] {
		return &fileInfo{name: path.Base(name), dir: true, mod: f.m.Time}, nil
	}
	return nil, &fs.PathError{Op: "stat", Path: name, Err: fs.ErrNotExist}
}

func (f *manifestFS) Open(name string) (fs.File, error) {
	fi, err := f.Stat(name)
	if err != nil {
		return nil, err
	}
	if fi.IsDir() {
		return &bucketFile{info: fi}, nil
	}
	return &bucketFile{info: fi, ReadSeekCloser: storage.NewReader(f.ctx, f.b, f.prefix+name, fi.Size())}, nil
}

type bucketFile struct {
	io.ReadSeekCloser // nil for directories
	info              fs.FileInfo
}

func (f *bucketFile) Stat() (fs.FileInfo, error) { return f.info, nil }

func (f *bucketFile) Read(p []byte) (int, error) {
	if f.ReadSeekCloser == nil {
		return 0, &fs.PathError{Op: "read", Path: f.info.Name(), Err: errors.New("is a directory")}
	}
	return f.ReadSeekCloser.Read(p)
}

func (f *bucketFile) Close() error {
	if f.ReadSeekCloser == nil {
		return nil
	}
	return f.ReadSeekCloser.Close()
}

type fileInfo struct {
	name string
	size int64
	dir  bool
	mod  time.Time
}

func (i *fileInfo) Name() string       { return i.name }
func (i *fileInfo) Size() int64        { return i.size }
func (i *fileInfo) ModTime() time.Time { return i.mod }
func (i *fileInfo) IsDir() bool        { return i.dir }
func (i *fileInfo) Sys() any           { return nil }

func (i *fileInfo) Mode() fs.FileMode {
	if i.dir {
		return fs.ModeDir | 0o755
	}
	return 0o644
}
