package server

import (
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
)

var stateRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

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

func (s *Server) handleGetSite(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	site, err := s.store.GetSite(r.Context(), name)
	if errors.Is(err, store.ErrNotFound) || !sites.ValidName(name) {
		writeErr(w, http.StatusNotFound, "no site named "+name)
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	deploys, err := s.store.ListDeploys(r.Context(), name, sitePageDeploys)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		siteResponse
		Deploys []store.Deploy `json:"deploys"`
	}{siteResponse{*site, s.cfg.SiteURL(name)}, deploys})
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
	s.render(w, http.StatusOK, "authorize.html", map[string]any{
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
	if err := s.store.CreateToken(r.Context(), hash, user.Email, user.Name, "cli", time.Now().Add(s.cfg.TokenTTL)); err != nil {
		s.internalErr(w, r, err)
		return
	}
	slog.Info("cli token issued", "by", user.Email)
	s.render(w, http.StatusOK, "cli-done.html", map[string]any{
		"Callback": "http://127.0.0.1:" + strconv.Itoa(port) + "/callback",
		"Token":    token,
		"State":    state,
	})
}

// API tokens. Listing and revoking need a token (these are /_q/api/ routes), so they work
// from the CLI. Anyone may revoke anyone's tokens: offboarding someone shouldn't wait for an
// admin, and under eze's trust model the worst case is making a colleague run eze login.

type tokenResponse struct {
	store.Token
	Current bool `json:"current"`
}

func (s *Server) handleListTokens(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	tokens, err := s.store.ListTokens(r.Context(), user.Email)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	out := make([]tokenResponse, len(tokens))
	for i, t := range tokens {
		out[i] = tokenResponse{t, user.TokenHash != "" && t.ID == store.TokenID(user.TokenHash)}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleRevokeCurrentToken(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	if user.TokenHash == "" {
		writeErr(w, http.StatusBadRequest, "this request was not made with an API token")
		return
	}
	if err := s.store.RevokeToken(r.Context(), user.Email, store.TokenID(user.TokenHash)); err != nil && !errors.Is(err, store.ErrNotFound) {
		s.internalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	user := auth.UserFrom(r.Context())
	err := s.store.RevokeToken(r.Context(), user.Email, r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "you have no token with that id")
		return
	}
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleRevokeUserTokens(w http.ResponseWriter, r *http.Request) {
	email := strings.ToLower(r.PathValue("email"))
	n, err := s.store.RevokeTokensFor(r.Context(), email)
	if err != nil {
		s.internalErr(w, r, err)
		return
	}
	slog.Info("tokens revoked", "for", email, "by", auth.UserFrom(r.Context()).Email, "count", n)
	writeJSON(w, http.StatusOK, map[string]int64{"revoked": n})
}
