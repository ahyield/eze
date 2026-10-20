package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"gocloud.dev/blob"

	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/storage"
)

// openBucket opens EZE_STORAGE, or returns nil to keep files on local disk.
func openBucket(rawURL string) (*blob.Bucket, error) {
	if rawURL == "" {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := storage.Open(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	slog.Info("storing sites and uploads in a bucket", "bucket", storage.Describe(rawURL))
	return b, nil
}

func storageCmd(args []string) error {
	if len(args) == 0 || args[0] != "migrate" {
		return errors.New("usage: eze storage migrate [--force]")
	}
	force := slices.Contains(args[1:], "--force")
	dataDir := os.Getenv("EZE_DATA_DIR")
	if dataDir == "" {
		dataDir = "./data"
	}
	rawURL := os.Getenv("EZE_STORAGE")
	if rawURL == "" {
		return errors.New("set EZE_STORAGE to the bucket to copy into, and EZE_DATA_DIR to the data folder to copy from")
	}
	if err := config.CheckStorage(rawURL); err != nil {
		return err
	}
	bucket, err := openBucket(rawURL)
	if err != nil {
		return err
	}
	defer bucket.Close()
	return migrate(context.Background(), dataDir, bucket, force, os.Stdout)
}

// migrate copies the sites and uploads under dataDir into bucket. It is safe to run again:
// sites already in the bucket are skipped unless force is set, and uploads already there
// with the same size are skipped. Run it with the server stopped.
func migrate(ctx context.Context, dataDir string, bucket *blob.Bucket, force bool, out io.Writer) error {
	ss, err := sites.NewStore(dataDir, bucket, 0, 0)
	if err != nil {
		return err
	}
	var copied, skipped int
	entries, err := os.ReadDir(filepath.Join(dataDir, "sites"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !sites.ValidName(name) {
			continue
		}
		if ss.Exists(name) && !force {
			skipped++
			continue
		}
		if err := ss.Import(name, filepath.Join(dataDir, "sites", name)); err != nil {
			return fmt.Errorf("site %s: %w", name, err)
		}
		fmt.Fprintf(out, "copied site %s\n", name)
		copied++
	}
	ss.Wait()
	fmt.Fprintf(out, "sites: %d copied, %d already in the bucket\n", copied, skipped)

	copied, skipped = 0, 0
	root := filepath.Join(dataDir, "uploads")
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if errors.Is(err, os.ErrNotExist) && p == root {
			return filepath.SkipDir
		}
		if err != nil || !d.Type().IsRegular() {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		site, id, ok := strings.Cut(filepath.ToSlash(rel), "/")
		// Unfinished uploads are named .part-* (older versions) or *.tmp.
		if !ok || strings.Contains(id, "/") || strings.HasPrefix(id, ".") || strings.HasSuffix(id, ".tmp") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		key := "uploads/" + site + "/" + id
		if attrs, err := bucket.Attributes(ctx, key); err == nil && attrs.Size == info.Size() {
			skipped++
			return nil
		}
		if err := copyFile(ctx, bucket, p, key); err != nil {
			return fmt.Errorf("upload %s/%s: %w", site, id, err)
		}
		copied++
		return nil
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "uploads: %d copied, %d already in the bucket\n", copied, skipped)
	fmt.Fprintf(out, "Done. The local copies in %s are untouched; delete sites/ and uploads/ there once eze works from the bucket.\n", dataDir)
	return nil
}

func copyFile(ctx context.Context, bucket *blob.Bucket, src, key string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	w, err := bucket.NewWriter(ctx, key, nil)
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
