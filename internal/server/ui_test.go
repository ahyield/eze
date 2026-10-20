package server

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ahyield/eze/internal/ai"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/testutil"
)

func page(t *testing.T, e *env, url, as string) (int, string) {
	t.Helper()
	w := e.do(req{url: url, as: as})
	return w.Code, w.Body.String()
}

func mustContain(t *testing.T, body string, wants ...string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(body, want) {
			t.Errorf("page is missing %q", want)
		}
	}
}

// knownSitesJSON extracts the data the drop zone uses to warn about overwrites.
func knownSitesJSON(t *testing.T, body string) map[string]knownSite {
	t.Helper()
	m := regexp.MustCompile(`(?s)<script type="application/json" id="known-sites">(.*?)</script>`).FindStringSubmatch(body)
	if m == nil {
		t.Fatal("known-sites data missing")
	}
	var known map[string]knownSite
	if err := json.Unmarshal([]byte(m[1]), &known); err != nil {
		t.Fatalf("known-sites is not JSON: %v\n%s", err, m[1])
	}
	return known
}

func TestIndexPage(t *testing.T) {
	e := newEnv(t)
	code, body := page(t, e, "https://eze.test/", ana)
	if code != 200 {
		t.Fatalf("index = %d", code)
	}
	mustContain(t, body, "data-drop", "Drop a folder to deploy it", "Nothing here yet", `href="/_q/app.css"`, `src="/_q/app.js"`, ana)
	if strings.Contains(body, "AI today") {
		t.Error("AI usage shown while AI is off")
	}

	e.deploy("blog", ana, map[string]string{"index.html": "x"})
	e.deploy("lunch-vote", bo, map[string]string{"index.html": "x"})
	_, body = page(t, e, "https://eze.test/", ana)
	mustContain(t, body, `href="/sites/blog"`, `href="https://lunch-vote.eze.test/"`, "data-site-search", `data-search="lunch-vote bo@example.com bo@example.com"`, "just now")
	known := knownSitesJSON(t, body)
	if known["blog"].DeployedBy != ana || !known["lunch-vote"].CanManage {
		t.Errorf("known = %+v", known)
	}
}

func TestIndexEscapesSiteData(t *testing.T) {
	e := newEnv(t)
	// Emails come from the identity provider; make sure they can't break out of the page.
	evil := `x</script><script>alert(1)</script>@example.com`
	e.deploy("blog", evil, map[string]string{"index.html": "x"})
	_, body := page(t, e, "https://eze.test/", ana)
	if strings.Contains(body, "<script>alert(1)") {
		t.Error("deployer email rendered unescaped")
	}
	if known := knownSitesJSON(t, body); known["blog"].DeployedBy != evil {
		t.Errorf("known-sites lost data: %+v", known)
	}
}

func TestIndexShowsAIUsage(t *testing.T) {
	e := newEnv(t)
	e.srv.ai = ai.NewAnthropic("sk-test", "http://127.0.0.1:1")
	e.store.RecordAIUsage(context.Background(), ana, "blog", time.Now(), 1200, 34)
	_, body := page(t, e, "https://eze.test/", ana)
	mustContain(t, body, "AI today: 1,234 of 1,000,000 tokens")
}

func TestSitePage(t *testing.T) {
	e := newEnv(t)
	e.srv.ai = ai.NewAnthropic("sk-test", "http://127.0.0.1:1")
	e.deploy("blog", ana, map[string]string{"index.html": "v1"})
	e.deploy("blog", bo, map[string]string{"index.html": "v2", "a.js": "1"})
	e.upload("blog", ana, "photo.png", "", []byte("png"))
	e.store.RecordAIUsage(context.Background(), bo, "blog", time.Now(), 5000, 120)

	code, body := page(t, e, "https://eze.test/sites/blog", ana)
	if code != 200 {
		t.Fatalf("site page = %d", code)
	}
	mustContain(t, body,
		"<h1>blog</h1>", `href="https://blog.eze.test/"`, "Open site",
		"Drop a new version of blog", `data-site="blog"`, // redeploy
		"Deploy history",
		"photo.png", "https://blog.eze.test/_q/files/", // uploads link to the site's origin
		"AI usage, last 30 days", "5,120 tokens", "5,000",
		`data-delete-site="blog"`, "Type blog to confirm",
	)
}

func TestSitePageEmptySections(t *testing.T) {
	e := newEnv(t)
	e.deploy("blog", ana, map[string]string{"index.html": "x"})
	_, body := page(t, e, "https://eze.test/sites/blog", ana)
	mustContain(t, body, "No uploads.")
	if strings.Contains(body, "AI usage") {
		t.Error("AI section shown while AI is off")
	}
}

func TestSitePageOwnerPolicy(t *testing.T) {
	e := newEnv(t, func(c *config.Config) { c.OverwritePolicy = config.OverwriteOwner })
	e.deploy("blog", ana, map[string]string{"index.html": "x"})

	_, body := page(t, e, "https://eze.test/sites/blog", bo)
	mustContain(t, body, "Only "+ana+" can redeploy or delete this site.")
	if strings.Contains(body, "data-drop") || strings.Contains(body, "data-delete-site") {
		t.Error("non-owner sees deploy or delete controls")
	}
	_, body = page(t, e, "https://eze.test/sites/blog", ana)
	mustContain(t, body, "data-drop", "data-delete-site")

	_, body = page(t, e, "https://eze.test/", bo)
	if known := knownSitesJSON(t, body); known["blog"].CanManage {
		t.Error("index tells a non-owner they can replace the site")
	}
}

func TestSitePageMissingAndInvalid(t *testing.T) {
	e := newEnv(t)
	code, body := page(t, e, "https://eze.test/sites/new-thing", ana)
	if code != 404 {
		t.Errorf("missing site = %d", code)
	}
	mustContain(t, body, "no site called new-thing yet", `data-site="new-thing"`)
	for _, name := range []string{"www", "Bad_Name", "-x"} {
		if code, _ := page(t, e, "https://eze.test/sites/"+name, ana); code != 404 {
			t.Errorf("/sites/%s = %d", name, code)
		}
	}
	if code, _ := page(t, e, "https://eze.test/sites/blog", ""); code != 401 {
		t.Errorf("anonymous = %d", code)
	}
}

func TestDashboardAssets(t *testing.T) {
	e := newEnv(t)
	for url, ct := range map[string]string{"https://eze.test/_q/app.js": "javascript", "https://eze.test/_q/app.css": "text/css"} {
		w := e.do(req{url: url, as: ana})
		if w.Code != 200 || !strings.Contains(w.Header().Get("Content-Type"), ct) || w.Body.Len() < 1000 {
			t.Errorf("%s = %d %q (%d bytes)", url, w.Code, w.Header().Get("Content-Type"), w.Body.Len())
		}
	}
}

func TestDeployZipThroughAPI(t *testing.T) {
	e := newEnv(t)
	zip := testutil.Zip(t,
		testutil.Entry{Name: "Site/index.html", Body: "from a zip"},
		testutil.Entry{Name: "__MACOSX/Site/._index.html", Body: "fork"},
	)
	w := e.do(req{method: "PUT", url: "https://eze.test/_q/api/sites/blog", as: ana, body: bytes.NewReader(zip),
		headers: map[string]string{"Content-Type": "application/zip", "Origin": "https://eze.test", "Sec-Fetch-Site": "same-origin"}})
	if w.Code != 200 {
		t.Fatalf("zip deploy = %d %s", w.Code, w.Body)
	}
	if w := e.do(req{url: "https://blog.eze.test/", as: ana}); w.Body.String() != "from a zip" {
		t.Errorf("site = %q", w.Body)
	}
	site, _ := e.store.GetSite(context.Background(), "blog")
	if site == nil || site.Files != 1 {
		t.Errorf("recorded site = %+v", site)
	}
}

func TestTemplateHelpers(t *testing.T) {
	for in, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567", -4200: "-4,200"} {
		if got := num(in); got != want {
			t.Errorf("num(%d) = %q", in, got)
		}
	}
	if num(42) != "42" {
		t.Error("num(int)")
	}
	for in, want := range map[int64]string{500: "500 B", 2048: "2 KB", 5 << 20: "5.0 MB", 3 << 30: "3.0 GB"} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q", in, got)
		}
	}
	now := time.Now()
	for d, want := range map[time.Duration]string{
		10 * time.Second: "just now", 90 * time.Second: "1 minute ago", 5 * time.Minute: "5 minutes ago",
		3 * time.Hour: "3 hours ago", 50 * time.Hour: "2 days ago",
	} {
		if got := ago(now.Add(-d)); got != want {
			t.Errorf("ago(-%v) = %q", d, got)
		}
	}
	old := time.Date(2025, 3, 4, 0, 0, 0, 0, time.UTC)
	if got := ago(old); got != "Mar 4, 2025" {
		t.Errorf("ago(old) = %q", got)
	}
}
