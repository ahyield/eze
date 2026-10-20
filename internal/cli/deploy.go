package cli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

const projectFile = "eze.json"

var siteNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// skipped top-level files never get uploaded: agent instructions and project config stay local.
var skipped = map[string]bool{projectFile: true, "AGENTS.md": true, "CLAUDE.md": true}

type project struct {
	Site string `json:"site"`
}

func readProject(dir string) (*project, error) {
	b, err := os.ReadFile(filepath.Join(dir, projectFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var p project
	if err := json.Unmarshal(b, &p); err != nil {
		return nil, fmt.Errorf("%s: %w", projectFile, err)
	}
	return &p, nil
}

func writeProject(dir string, p *project) error {
	b, _ := json.MarshalIndent(p, "", "  ")
	return os.WriteFile(filepath.Join(dir, projectFile), append(b, '\n'), 0o644)
}

// slug turns a directory name into a valid site name, or "" if nothing usable is left.
func slug(s string) string {
	s = strings.ToLower(s)
	s = regexp.MustCompile(`[^a-z0-9-]+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = strings.TrimRight(s[:63], "-")
	}
	return s
}

func Deploy(args []string) error {
	fs := flag.NewFlagSet("deploy", flag.ContinueOnError)
	siteFlag := fs.String("site", "", "site name (defaults to eze.json, then the directory name)")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) > 1 {
		return errors.New("usage: eze deploy [dir] [--site name]")
	}
	dir := "."
	if len(pos) == 1 {
		dir = pos[0]
	}
	c, err := newClient()
	if err != nil {
		return err
	}
	res, err := deployDir(c, dir, *siteFlag, func(files int, size int64, name string) {
		noun := "files"
		if files == 1 {
			noun = "file"
		}
		fmt.Printf("Deploying %d %s (%s) to %s...\n", files, noun, humanBytes(size), name)
	})
	if err != nil {
		return err
	}
	if res.Warning != "" {
		fmt.Fprintln(os.Stderr, "warning:", res.Warning)
	}
	fmt.Println("Live at", res.Site.URL)
	return nil
}

type deployResult struct {
	Site    site
	Files   int
	Warning string
}

// deployDir packs and uploads dir. The site name comes from siteName, then eze.json, then
// the directory name; it is saved to eze.json so later deploys go to the same site.
func deployDir(c *client, dir, siteName string, starting func(files int, size int64, name string)) (*deployResult, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	proj, err := readProject(abs)
	if err != nil {
		return nil, err
	}
	name := siteName
	if name == "" && proj != nil {
		name = proj.Site
	}
	if name == "" {
		name = slug(filepath.Base(abs))
	}
	if !siteNameRe.MatchString(name) {
		return nil, fmt.Errorf("%q is not a valid site name; use lowercase letters, digits and hyphens", name)
	}

	var buf bytes.Buffer
	files, err := pack(abs, &buf)
	if err != nil {
		return nil, err
	}
	if files == 0 {
		return nil, fmt.Errorf("nothing to deploy in %s", dir)
	}
	res := &deployResult{Files: files}
	if _, err := os.Stat(filepath.Join(abs, "index.html")); err != nil {
		res.Warning = "no index.html at the top level, so the site root will show \"not found\""
	}
	if starting != nil {
		starting(files, int64(buf.Len()), name)
	}
	if err := c.do("PUT", "/_q/api/sites/"+url.PathEscape(name), &buf, "application/gzip", &res.Site); err != nil {
		return nil, err
	}
	if proj == nil || proj.Site != name {
		if err := writeProject(abs, &project{Site: name}); err != nil {
			res.Warning = strings.TrimPrefix(res.Warning+"; could not save eze.json: "+err.Error(), "; ")
		}
	}
	return res, nil
}

// pack writes dir as a gzipped tarball, skipping hidden files (except .well-known), symlinks and local-only files.
func pack(dir string, w io.Writer) (int, error) {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	files := 0
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		name := d.Name()
		hidden := strings.HasPrefix(name, ".") && name != ".well-known"
		localOnly := skipped[name] && filepath.Dir(p) == dir
		if hidden || localOnly || name == "node_modules" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() && !d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			return tw.WriteHeader(&tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755})
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := tw.WriteHeader(&tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}); err != nil {
			return err
		}
		if _, err := io.Copy(tw, f); err != nil {
			return err
		}
		files++
		return nil
	})
	if err != nil {
		return 0, err
	}
	if err := tw.Close(); err != nil {
		return 0, err
	}
	return files, gz.Close()
}

func humanBytes(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
