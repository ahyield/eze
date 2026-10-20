package server

import (
	"bytes"
	"encoding/base64"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/ahyield/eze/internal/sites"
)

// The browser code (web/app.js, web/sdk.js) is checked with Node when it's installed.

func runNode(t *testing.T, script, target string) []byte {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed; skipping browser code checks")
	}
	var stdout, stderr bytes.Buffer
	cmd := exec.Command(node, script, target)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v\n%s", script, err, stderr.String())
	}
	return stdout.Bytes()
}

func TestSDKAIInBrowser(t *testing.T) {
	runNode(t, "jstest/sdk_ai.mjs", "web/sdk.js")
}

func TestDashboardTarballDeploys(t *testing.T) {
	out := runNode(t, "jstest/app_tar.mjs", "web/app.js")
	archive, err := base64.StdEncoding.DecodeString(string(out))
	if err != nil {
		t.Fatalf("decoding archive: %v", err)
	}
	ss, err := sites.NewStore(t.TempDir(), 1<<20, 100)
	if err != nil {
		t.Fatal(err)
	}
	files, _, err := ss.Deploy("blog", bytes.NewReader(archive))
	if err != nil || files != 5 {
		t.Fatalf("deploying the browser-built archive: files=%d err=%v", files, err)
	}
	long := "/deeply/" + "nested-directory-name/nested-directory-name/nested-directory-name/nested-directory-name/nested-directory-name/" + "with-a-rather-long-file-name-that-needs-the-prefix.txt"
	for path, want := range map[string]string{
		"/":              "<h1>hi</h1>",
		long:             "long",
		"/café/menü.txt": "ünïcode",
		"/empty.txt":     "",
		"/big.txt":       string(bytes.Repeat([]byte("x"), 1300)),
	} {
		w := httptest.NewRecorder()
		ss.Serve(w, httptest.NewRequest("GET", "http://blog.eze.test"+pathEscape(path), nil), "blog")
		if w.Code != 200 || w.Body.String() != want {
			t.Errorf("%s = %d (%d bytes)", path, w.Code, w.Body.Len())
		}
	}
}

// pathEscape escapes non-ASCII characters so the path can go in a request line.
func pathEscape(p string) string {
	var b bytes.Buffer
	for _, c := range []byte(p) {
		if c < 0x80 {
			b.WriteByte(c)
		} else {
			b.WriteString("%" + string("0123456789ABCDEF"[c>>4]) + string("0123456789ABCDEF"[c&15]))
		}
	}
	return b.String()
}
