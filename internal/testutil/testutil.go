// Package testutil holds helpers shared by tests across packages.
package testutil

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"io/fs"
	"testing"
)

// Entry is one member of a test archive. A zero Type means a regular file.
type Entry struct {
	Name string
	Body string
	Type byte
	Link string
}

// TarGz builds a gzipped tarball from entries, in order.
func TarGz(t testing.TB, entries ...Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		hdr := &tar.Header{Name: e.Name, Mode: 0o644, Typeflag: e.Type, Linkname: e.Link}
		if hdr.Typeflag == 0 {
			hdr.Typeflag = tar.TypeReg
			hdr.Size = int64(len(e.Body))
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// Files is shorthand for an archive of regular files keyed by path.
func Files(t testing.TB, files map[string]string) []byte {
	t.Helper()
	var entries []Entry
	for name, body := range files {
		entries = append(entries, Entry{Name: name, Body: body})
	}
	return TarGz(t, entries...)
}

// Zip builds a zip archive from entries, in order. Type tar.TypeSymlink marks a symlink
// (Body is its target); names ending in "/" are directories.
func Zip(t testing.TB, entries ...Entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, e := range entries {
		hdr := &zip.FileHeader{Name: e.Name, Method: zip.Deflate}
		if e.Type == tar.TypeSymlink {
			hdr.SetMode(0o777 | fs.ModeSymlink)
		}
		w, err := zw.CreateHeader(hdr)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(e.Body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
