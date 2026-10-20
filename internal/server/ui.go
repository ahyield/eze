package server

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
)

//go:embed web
var webFS embed.FS

var (
	templates = template.Must(template.New("").Funcs(template.FuncMap{
		"bytes": humanBytes,
		"ago":   ago,
		"num":   num,
		"add":   func(a, b int64) int64 { return a + b },
		"json": func(v any) (template.JS, error) {
			b, err := json.Marshal(v)
			return template.JS(b), err
		},
		"percent": func(part, whole int64) int64 {
			if whole <= 0 {
				return 0
			}
			return min(100, part*100/whole)
		},
	}).ParseFS(webFS, "web/*.html"))
	sdkJS, _  = webFS.ReadFile("web/sdk.js")
	appJS, _  = webFS.ReadFile("web/app.js")
	appCSS, _ = webFS.ReadFile("web/app.css")
)

func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.1f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func ago(t time.Time) string {
	d := time.Since(t)
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit + " ago"
		}
		return strconv.Itoa(n) + " " + unit + "s ago"
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d.Minutes()), "minute")
	case d < 24*time.Hour:
		return plural(int(d.Hours()), "hour")
	case d < 30*24*time.Hour:
		return plural(int(d.Hours()/24), "day")
	default:
		return t.Format("Jan 2, 2006")
	}
}

// num formats an integer with thousands separators.
func num(v any) string {
	var n int64
	switch v := v.(type) {
	case int:
		n = int64(v)
	case int64:
		n = v
	default:
		return fmt.Sprint(v)
	}
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func (s *Server) render(w http.ResponseWriter, status int, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// Sites share the session cookie, so one could frame the dashboard and trick clicks
	// (deploy, delete, authorize the CLI). The dashboard is never framed.
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Header().Set("X-Frame-Options", "DENY")
	w.WriteHeader(status)
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "template", name, "err", err)
	}
}

func serveAsset(contentType string, body []byte) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(body)
	}
}

func (s *Server) handleSDK(w http.ResponseWriter, r *http.Request) {
	serveAsset("text/javascript; charset=utf-8", sdkJS)(w, r)
}

// canManage reports whether user may redeploy or delete site (nil means it doesn't exist yet).
func (s *Server) canManage(user *auth.User, site *store.Site) bool {
	return s.cfg.OverwritePolicy != config.OverwriteOwner || site == nil || site.Owner == user.Email
}

// page holds what every page's header needs.
func (s *Server) page(r *http.Request, title string) map[string]any {
	return map[string]any{
		"Title":  title,
		"User":   auth.UserFrom(r.Context()),
		"OIDC":   s.cfg.AuthMode == config.AuthOIDC,
		"Domain": s.cfg.Domain,
		"Scheme": s.cfg.Scheme,
		"Server": s.cfg.ApexURL(""),
	}
}

type siteRow struct {
	store.Site
	URL       string
	CanManage bool
}

// knownSite is what the drop zone needs to warn before replacing a site.
type knownSite struct {
	DeployedBy string `json:"deployed_by"`
	CanManage  bool   `json:"can_manage"`
	Owner      string `json:"owner"`
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	list, err := s.store.ListSites(r.Context())
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	rows := make([]siteRow, len(list))
	known := make(map[string]knownSite, len(list))
	for i := range list {
		site := &list[i]
		rows[i] = siteRow{*site, s.cfg.SiteURL(site.Name), s.canManage(user, site)}
		known[site.Name] = knownSite{site.DeployedBy, rows[i].CanManage, site.Owner}
	}
	data := s.page(r, "eze")
	data["Name"] = "" // the drop zone is not tied to a site here
	data["Sites"] = rows
	data["Known"] = known
	data["MaxDeployBytes"] = s.cfg.MaxDeployBytes
	if s.ai != nil {
		used, err := s.store.AITokensUsed(r.Context(), user.Email, time.Now())
		if err != nil {
			s.internalErr(w, r, err)
			return
		}
		data["AI"] = map[string]int64{"Used": used, "Limit": s.cfg.AIDailyTokens}
	}
	s.render(w, http.StatusOK, "index.html", data)
}

const (
	sitePageDeploys = 20
	sitePageFiles   = 50
	aiUsageDays     = 30
)

func (s *Server) handleSitePage(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sites.ValidName(name) {
		http.Error(w, "Not a valid site name.", http.StatusNotFound)
		return
	}
	ctx := r.Context()
	user := auth.UserFrom(ctx)
	data := s.page(r, name)
	data["Name"] = name
	data["URL"] = s.cfg.SiteURL(name)
	data["MaxDeployBytes"] = s.cfg.MaxDeployBytes

	site, err := s.store.GetSite(ctx, name)
	if errors.Is(err, store.ErrNotFound) {
		// Not deployed yet: the page offers to create it.
		data["CanManage"] = true
		s.render(w, http.StatusNotFound, "site.html", data)
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	deploys, err := s.store.ListDeploys(ctx, name, sitePageDeploys)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	files, err := s.store.ListFiles(ctx, name, sitePageFiles, 0)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	uploaded, err := s.store.UploadedBytes(ctx, name)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	fileLinks := make([]map[string]any, len(files))
	for i := range files {
		fileLinks[i] = map[string]any{"File": files[i], "URL": strings.TrimSuffix(data["URL"].(string), "/") + fileURL(&files[i])}
	}

	data["Site"] = site
	data["CanManage"] = s.canManage(user, site)
	data["Policy"] = s.cfg.OverwritePolicy
	data["Deploys"] = deploys
	data["Files"] = fileLinks
	data["FilesShown"] = sitePageFiles
	data["Uploaded"] = uploaded
	data["UploadQuota"] = s.cfg.UploadQuota
	if s.ai != nil {
		usage, err := s.store.AIUsageBySite(ctx, name, time.Now().AddDate(0, 0, -(aiUsageDays-1)))
		if err != nil {
			s.internalErr(w, r, err)
			return
		}
		var total int64
		for _, u := range usage {
			total += u.InputTokens + u.OutputTokens
		}
		data["AIUsage"] = usage
		data["AITotal"] = total
		data["AIDays"] = aiUsageDays
	}
	s.render(w, http.StatusOK, "site.html", data)
}
