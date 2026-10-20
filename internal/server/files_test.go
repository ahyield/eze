package server

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ahyield/eze/internal/config"
)

// multipartBody builds a form with one file field (or another field name, to test errors).
func multipartBody(t *testing.T, field, filename, contentType string, content []byte) (io.Reader, string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("note", "fields before the file are skipped")
	h := textproto.MIMEHeader{}
	h.Set("Content-Disposition", `form-data; name="`+field+`"; filename="`+filename+`"`)
	if contentType != "" {
		h.Set("Content-Type", contentType)
	}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	part.Write(content)
	mw.Close()
	return &buf, mw.FormDataContentType()
}

func (e *env) upload(site, as, filename, contentType string, content []byte) *httptest.ResponseRecorder {
	e.t.Helper()
	body, ct := multipartBody(e.t, "file", filename, contentType, content)
	return e.do(req{method: "POST", url: "https://" + site + ".eze.test/_q/files", as: as, body: body, headers: map[string]string{"Content-Type": ct}})
}

func TestFileUploadAndServe(t *testing.T) {
	e := newEnv(t)
	png := append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{0}, 100)...)

	w := e.upload("blog", ana, "../../cat photo.png", "application/octet-stream", png)
	if w.Code != 201 {
		t.Fatalf("upload = %d %s", w.Code, w.Body)
	}
	f := decode[map[string]any](t, w)
	id := f["id"].(string)
	if f["name"] != "cat photo.png" || f["content_type"] != "image/png" || f["size"] != float64(len(png)) || f["_created_by"] != ana {
		t.Errorf("upload response = %v", f)
	}
	if f["url"] != "/_q/files/"+id+"/cat%20photo.png" {
		t.Errorf("url = %v", f["url"])
	}

	w = e.do(req{url: "https://blog.eze.test" + f["url"].(string), as: bo})
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), png) {
		t.Fatalf("content = %d (%d bytes)", w.Code, w.Body.Len())
	}
	h := w.Header()
	if h.Get("Content-Type") != "image/png" || !strings.HasPrefix(h.Get("Content-Disposition"), "inline") ||
		!strings.Contains(h.Get("Content-Security-Policy"), "sandbox") || h.Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers = %v", h)
	}
	// The name segment is cosmetic.
	if w := e.do(req{url: "https://blog.eze.test/_q/files/" + id + "/whatever.txt", as: bo}); w.Code != 200 {
		t.Errorf("other name = %d", w.Code)
	}
	// Range requests work, for video seeking.
	w = e.do(req{url: "https://blog.eze.test" + f["url"].(string), as: bo, headers: map[string]string{"Range": "bytes=0-3"}})
	if w.Code != 206 || w.Body.String() != "\x89PNG" {
		t.Errorf("range = %d %q", w.Code, w.Body)
	}

	if got := decode[map[string]any](t, e.do(req{url: "https://blog.eze.test/_q/files/" + id, as: bo})); got["id"] != id || got["url"] != f["url"] {
		t.Errorf("metadata = %v", got)
	}
	if list := decode[[]map[string]any](t, e.do(req{url: "https://blog.eze.test/_q/files", as: bo})); len(list) != 1 || list[0]["id"] != id {
		t.Errorf("list = %v", list)
	}
	if w := e.do(req{url: "https://other.eze.test/_q/files/" + id, as: ana}); w.Code != 404 {
		t.Errorf("file visible from another site: %d", w.Code)
	}

	if w := e.do(req{method: "DELETE", url: "https://blog.eze.test/_q/files/" + id, as: bo}); w.Code != 204 {
		t.Fatalf("delete = %d", w.Code)
	}
	if w := e.do(req{url: "https://blog.eze.test" + f["url"].(string), as: bo}); w.Code != 404 {
		t.Errorf("deleted content = %d", w.Code)
	}
	if w := e.do(req{method: "DELETE", url: "https://blog.eze.test/_q/files/" + id, as: bo}); w.Code != 404 {
		t.Errorf("delete twice = %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "uploads", "blog", id)); !os.IsNotExist(err) {
		t.Error("bytes left on disk after delete")
	}
}

func TestUploadedHTMLCannotRunAsTheSite(t *testing.T) {
	e := newEnv(t)
	for _, tt := range []struct {
		name, declared, disposition string
	}{
		{"evil.html", "text/html", "attachment"},
		{"evil", "text/html", "attachment"},
		{"evil.svg", "image/svg+xml", "inline"}, // inline, but the sandbox CSP stops its scripts
	} {
		f := decode[map[string]any](t, e.upload("blog", ana, tt.name, tt.declared, []byte("<script>alert(1)</script>")))
		w := e.do(req{url: "https://blog.eze.test" + f["url"].(string), as: bo})
		if !strings.HasPrefix(w.Header().Get("Content-Disposition"), tt.disposition) {
			t.Errorf("%s disposition = %q", tt.name, w.Header().Get("Content-Disposition"))
		}
		if !strings.HasPrefix(w.Header().Get("Content-Security-Policy"), "sandbox") {
			t.Errorf("%s missing sandbox CSP", tt.name)
		}
	}
}

func TestUploadLimits(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.MaxUploadBytes = 100; c.UploadQuota = 250 })
	if w := e.upload("blog", ana, "big.bin", "", make([]byte, 101)); w.Code != 413 || !strings.Contains(w.Body.String(), "100 bytes") {
		t.Errorf("over per-file limit = %d %s", w.Code, w.Body)
	}
	if w := e.upload("blog", ana, "exact.bin", "", make([]byte, 100)); w.Code != 201 {
		t.Errorf("at per-file limit = %d %s", w.Code, w.Body)
	}
	if w := e.upload("blog", ana, "two.bin", "", make([]byte, 100)); w.Code != 201 {
		t.Errorf("second file = %d", w.Code)
	}
	// 200 of 250 used: a 60-byte file fits the per-file limit but not the quota.
	if w := e.upload("blog", ana, "three.bin", "", make([]byte, 60)); w.Code != 413 || !strings.Contains(w.Body.String(), "quota") {
		t.Errorf("over quota = %d %s", w.Code, w.Body)
	}
	if w := e.upload("blog", ana, "fits.bin", "", make([]byte, 50)); w.Code != 201 {
		t.Errorf("fits quota = %d %s", w.Code, w.Body)
	}
	if w := e.upload("blog", ana, "full.bin", "", []byte("x")); w.Code != 413 || !strings.Contains(w.Body.String(), "quota") {
		t.Errorf("quota full = %d %s", w.Code, w.Body)
	}
	// Quotas are per site.
	if w := e.upload("other", ana, "a.bin", "", make([]byte, 100)); w.Code != 201 {
		t.Errorf("other site = %d", w.Code)
	}
	// Rejected uploads leave nothing behind.
	entries, _ := os.ReadDir(filepath.Join(e.cfg.DataDir, "uploads", "blog"))
	if len(entries) != 3 {
		t.Errorf("blog has %d files on disk, want 3", len(entries))
	}
}

func TestUploadBadRequests(t *testing.T) {
	e := newEnv(t)
	url := "https://blog.eze.test/_q/files"
	if w := e.do(req{method: "POST", url: url, as: ana, body: strings.NewReader("raw"), headers: map[string]string{"Content-Type": "text/plain"}}); w.Code != 400 {
		t.Errorf("not multipart = %d", w.Code)
	}
	body, ct := multipartBody(t, "attachment", "a.txt", "", []byte("x"))
	if w := e.do(req{method: "POST", url: url, as: ana, body: body, headers: map[string]string{"Content-Type": ct}}); w.Code != 400 || !strings.Contains(w.Body.String(), "file") {
		t.Errorf("wrong field = %d %s", w.Code, w.Body)
	}
	body, ct = multipartBody(t, "file", "a.txt", "", []byte("x"))
	truncated, _ := io.ReadAll(body)
	truncated = truncated[:len(truncated)-20]
	if w := e.do(req{method: "POST", url: url, as: ana, body: bytes.NewReader(truncated), headers: map[string]string{"Content-Type": ct}}); w.Code != 400 {
		t.Errorf("truncated = %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/_q/files/a.b", "/_q/files/a.b/x.png"} {
		if w := e.do(req{url: "https://blog.eze.test" + path, as: ana}); w.Code != 400 {
			t.Errorf("GET %s = %d", path, w.Code)
		}
	}
	if w := e.do(req{url: url + "?limit=0", as: ana}); w.Code != 400 {
		t.Errorf("bad limit = %d", w.Code)
	}
	// Cross-origin uploads are blocked like any other write.
	body, ct = multipartBody(t, "file", "a.txt", "", []byte("x"))
	if w := e.do(req{method: "POST", url: url, as: ana, body: body, headers: map[string]string{"Content-Type": ct, "Origin": "https://evil.eze.test"}}); w.Code != 403 {
		t.Errorf("cross-origin upload = %d", w.Code)
	}
}

func TestUploadsSurviveRedeployAndDieWithSite(t *testing.T) {
	e := newEnv(t)
	e.deploy("blog", ana, map[string]string{"index.html": "v1"})
	f := decode[map[string]any](t, e.upload("blog", ana, "a.txt", "", []byte("hello")))
	e.deploy("blog", ana, map[string]string{"index.html": "v2"})
	if w := e.do(req{url: "https://blog.eze.test" + f["url"].(string), as: ana}); w.Body.String() != "hello" {
		t.Errorf("upload lost on redeploy: %d", w.Code)
	}
	if w := e.do(req{method: "DELETE", url: "https://eze.test/_q/sites/blog", as: ana}); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if w := e.do(req{url: "https://blog.eze.test" + f["url"].(string), as: ana}); w.Code != 404 {
		t.Errorf("upload survived site delete: %d", w.Code)
	}
	if _, err := os.Stat(filepath.Join(e.cfg.DataDir, "uploads", "blog")); !os.IsNotExist(err) {
		t.Error("upload dir survived site delete")
	}
}

func TestFileContentNotRateLimited(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.RateLimit = 0.001; c.RateBurst = 2 })
	f := decode[map[string]any](t, e.upload("blog", ana, "a.png", "", []byte("x")))
	for i := range 10 {
		if w := e.do(req{url: "https://blog.eze.test" + f["url"].(string), as: ana}); w.Code != 200 {
			t.Fatalf("content request %d = %d", i, w.Code)
		}
	}
	if w := e.do(req{url: "https://blog.eze.test/_q/files", as: ana}); w.Code != 200 {
		t.Errorf("second API call = %d", w.Code)
	}
	if w := e.do(req{url: "https://blog.eze.test/_q/files", as: ana}); w.Code != 429 {
		t.Errorf("third API call = %d, want 429", w.Code)
	}
}
