package sites

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
)

// localBackend keeps each site as a directory under root and swaps deploys in with a rename.
type localBackend struct {
	root string // <data>/sites
	tmp  string // <data>/tmp, same filesystem as root so renames are atomic
}

func newLocalBackend(root, tmp string) (*localBackend, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &localBackend{root: root, tmp: tmp}, nil
}

func (l *localBackend) dir(name string) string { return filepath.Join(l.root, name) }

func (l *localBackend) exists(name string) bool {
	fi, err := os.Stat(l.dir(name))
	return err == nil && fi.IsDir()
}

func (l *localBackend) publish(name, staging string) error {
	if err := os.Chmod(staging, 0o755); err != nil {
		return err
	}
	target := l.dir(name)
	old := ""
	if l.exists(name) {
		var err error
		if old, err = os.MkdirTemp(l.tmp, "old-"+name+"-"); err != nil {
			return err
		}
		// MkdirTemp creates the directory; Rename needs the destination to not exist.
		os.Remove(old)
		if err := os.Rename(target, old); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, target); err != nil {
		if old != "" {
			os.Rename(old, target) // put the previous version back
		}
		return err
	}
	if old != "" {
		os.RemoveAll(old)
	}
	return nil
}

func (l *localBackend) remove(name string) error {
	if !l.exists(name) {
		return os.ErrNotExist
	}
	old, err := os.MkdirTemp(l.tmp, "rm-"+name+"-")
	if err != nil {
		return err
	}
	os.Remove(old)
	if err := os.Rename(l.dir(name), old); err != nil {
		return err
	}
	return os.RemoveAll(old)
}

func (l *localBackend) cleanup(string) {}

func (l *localBackend) current(_ context.Context, name string) (fs.FS, bool) {
	if !l.exists(name) {
		return nil, false
	}
	return os.DirFS(l.dir(name)), true
}
