package server

import (
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
)

//go:embed web
var webFS embed.FS

var (
	templates = template.Must(template.ParseFS(webFS, "web/*.html"))
	sdkJS, _  = webFS.ReadFile("web/sdk.js")
	stateRe   = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
)

func (s *Server) handleSDK(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(sdkJS)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSites(r.Context())
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	type row struct {
		store.Site
		URL string
	}
	rows := make([]row, len(list))
	for i, site := range list {
		rows[i] = row{site, s.cfg.SiteURL(site.Name)}
	}
	s.render(w, "index.html", map[string]any{
		"User":   auth.UserFrom(r.Context()),
		"Sites":  rows,
		"Server": s.cfg.ApexURL(""),
		"Scheme": s.cfg.Scheme,
		"Domain": s.cfg.Domain,
		"OIDC":   s.cfg.AuthMode == config.AuthOIDC,
	})
}

func (s *Server) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if err := templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("render", "template", name, "err", err)
	}
}

type siteResponse struct {
	store.Site
	URL string `json:"url"`
}

func (s *Server) handleListSites(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListSites(r.Context())
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	out := make([]siteResponse, len(list))
	for i, site := range list {
		out[i] = siteResponse{site, s.cfg.SiteURL(site.Name)}
	}
	writeJSON(w, http.StatusOK, out)
}

// checkOwner enforces EZE_OVERWRITE=owner. In the default "anyone" mode every employee can
// overwrite or delete any site, which is the point: there is nothing to administer.
func (s *Server) checkOwner(w http.ResponseWriter, r *http.Request, name string) bool {
	if s.cfg.OverwritePolicy != config.OverwriteOwner {
		return true
	}
	site, err := s.store.GetSite(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		s.internalErr(w, r, err)
		return false
	}
	if site.Owner != auth.UserFrom(r.Context()).Email {
		writeErr(w, http.StatusForbidden, name+" belongs to "+site.Owner)
		return false
	}
	return true
}

func (s *Server) handleDeploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sites.ValidName(name) {
		writeErr(w, http.StatusBadRequest, "site names must be lowercase letters, digits and hyphens (max 63), and not reserved")
		return
	}
	if !s.checkOwner(w, r, name) {
		return
	}
	user := auth.UserFrom(r.Context())
	body := http.MaxBytesReader(w, r.Body, s.cfg.MaxDeployBytes)
	files, bytes, err := s.sites.Deploy(name, body)
	var deployErr *sites.DeployError
	var tooBig *http.MaxBytesError
	switch {
	case errors.As(err, &tooBig):
		writeErr(w, http.StatusRequestEntityTooLarge, "upload is larger than "+strconv.FormatInt(s.cfg.MaxDeployBytes, 10)+" bytes")
		return
	case errors.As(err, &deployErr):
		writeErr(w, http.StatusBadRequest, deployErr.Error())
		return
	case err != nil:
		s.internalErr(w, r, err)
		return
	}
	if err := s.store.RecordDeploy(r.Context(), name, user.Email, files, bytes); err != nil {
		s.internalErr(w, r, err)
		return
	}
	slog.Info("deploy", "site", name, "by", user.Email, "files", files, "bytes", bytes)
	site, err := s.store.GetSite(r.Context(), name)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, siteResponse{*site, s.cfg.SiteURL(name)})
}

func (s *Server) handleDeleteSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !sites.ValidName(name) {
		writeErr(w, http.StatusBadRequest, "invalid site name")
		return
	}
	if !s.checkOwner(w, r, name) {
		return
	}
	err := s.sites.Delete(name)
	if errors.Is(err, os.ErrNotExist) {
		writeErr(w, http.StatusNotFound, "no site named "+name)
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	if err := s.store.DeleteSite(r.Context(), name); err != nil {
		s.internalErr(w, r, err)
		return
	}
	if err := s.uploads.DeleteSite(name); err != nil {
		s.internalErr(w, r, err)
		return
	}
	s.hub.dropSite(name)
	s.rooms.dropSite(name)
	slog.Info("delete", "site", name, "by", auth.UserFrom(r.Context()).Email)
	w.WriteHeader(http.StatusNoContent)
}

// CLI login: `eze login` listens on a loopback port and opens this page in the browser.
// The user confirms, we mint a token and redirect it back to the CLI. The confirm step is
// a POST so a link alone can't mint tokens.

func cliParams(v url.Values) (port int, state string, ok bool) {
	port, err := strconv.Atoi(v.Get("port"))
	state = v.Get("state")
	return port, state, err == nil && port >= 1024 && port <= 65535 && stateRe.MatchString(state)
}

func (s *Server) handleCLIAuthorizePage(w http.ResponseWriter, r *http.Request) {
	port, state, ok := cliParams(r.URL.Query())
	if !ok {
		http.Error(w, "invalid CLI login request", http.StatusBadRequest)
		return
	}
	s.render(w, "authorize.html", map[string]any{
		"User":  auth.UserFrom(r.Context()),
		"Port":  port,
		"State": state,
	})
}

func (s *Server) handleCLIAuthorize(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	port, state, ok := cliParams(r.PostForm)
	if !ok {
		http.Error(w, "invalid CLI login request", http.StatusBadRequest)
		return
	}
	user := auth.UserFrom(r.Context())
	token, hash := auth.NewToken()
	if err := s.store.CreateToken(r.Context(), hash, user.Email, user.Name, "cli"); err != nil {
		s.internalErr(w, r, err)
		return
	}
	slog.Info("cli token issued", "by", user.Email)
	q := url.Values{"token": {token}, "state": {state}}
	http.Redirect(w, r, "http://127.0.0.1:"+strconv.Itoa(port)+"/callback?"+q.Encode(), http.StatusSeeOther)
}
