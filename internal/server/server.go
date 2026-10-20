// Package server routes requests for the apex host (index, CLI and deploy API) and for
// every site subdomain (static files plus the client-side APIs under /_q/).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
	"github.com/ahyield/eze/internal/uploads"
)

type Server struct {
	cfg     *config.Config
	authn   auth.Authenticator
	store   *store.Store
	sites   *sites.Store
	uploads *uploads.Store
	hub     *hub
	limiter *rateLimiter

	public *http.ServeMux // apex routes reachable without a user (login flow)
	apex   *http.ServeMux
	site   *http.ServeMux
}

func New(cfg *config.Config, authn auth.Authenticator, st *store.Store, ss *sites.Store) (*Server, error) {
	up, err := uploads.NewStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	s := &Server{
		cfg:     cfg,
		authn:   authn,
		store:   st,
		sites:   ss,
		uploads: up,
		hub:     newHub(),
		limiter: newRateLimiter(cfg.RateLimit, cfg.RateBurst),
		public:  http.NewServeMux(),
		apex:    http.NewServeMux(),
		site:    http.NewServeMux(),
	}
	authn.Mount(s.public)

	s.apex.HandleFunc("GET /{$}", s.handleIndex)
	s.apex.HandleFunc("GET /_q/sdk.js", s.handleSDK)
	s.apex.HandleFunc("GET /_q/me", s.handleMe)
	s.apex.HandleFunc("GET /_q/cli/authorize", s.handleCLIAuthorizePage)
	s.apex.HandleFunc("POST /_q/cli/authorize", s.handleCLIAuthorize)
	s.apex.HandleFunc("GET /_q/api/me", s.handleMe)
	s.apex.HandleFunc("GET /_q/api/sites", s.handleListSites)
	s.apex.HandleFunc("PUT /_q/api/sites/{name}", s.handleDeploy)
	s.apex.HandleFunc("DELETE /_q/api/sites/{name}", s.handleDeleteSite)

	s.site.HandleFunc("GET /_q/sdk.js", s.handleSDK)
	s.site.HandleFunc("GET /_q/me", s.handleMe)
	s.site.HandleFunc("GET /_q/db/{collection}", s.handleListDocs)
	s.site.HandleFunc("POST /_q/db/{collection}", s.handleCreateDoc)
	s.site.HandleFunc("GET /_q/db/{collection}/{id}", s.handleGetDoc)
	s.site.HandleFunc("PUT /_q/db/{collection}/{id}", s.handleSetDoc)
	s.site.HandleFunc("PATCH /_q/db/{collection}/{id}", s.handleUpdateDoc)
	s.site.HandleFunc("DELETE /_q/db/{collection}/{id}", s.handleDeleteDoc)
	s.site.HandleFunc("GET /_q/subscribe/{collection}", s.handleSubscribe)
	s.site.HandleFunc("POST /_q/files", s.handleUpload)
	s.site.HandleFunc("GET /_q/files", s.handleListFiles)
	s.site.HandleFunc("GET /_q/files/{id}", s.handleGetFile)
	s.site.HandleFunc("GET /_q/files/{id}/{name}", s.handleFileContent)
	s.site.HandleFunc("DELETE /_q/files/{id}", s.handleDeleteFile)
	// Must carry a method: a method-less "/_q/" would conflict with "GET /" in ServeMux.
	s.site.HandleFunc("GET /_q/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "unknown API endpoint")
	})
	s.site.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		s.sites.Serve(w, r, siteFrom(r.Context()))
	})
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" {
		w.Write([]byte("ok"))
		return
	}

	host := strings.ToLower(r.Host)
	var mux *http.ServeMux
	switch {
	case host == s.cfg.Domain:
		if strings.HasPrefix(r.URL.Path, "/_q/auth/") {
			s.public.ServeHTTP(w, r)
			return
		}
		mux = s.apex
	default:
		name, ok := strings.CutSuffix(host, "."+s.cfg.Domain)
		if !ok || !sites.ValidName(name) {
			http.Error(w, "unknown host", http.StatusNotFound)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), siteKey{}, name))
		mux = s.site
	}

	user := s.identify(r)
	if user == nil {
		if isAPI(r.URL.Path) {
			writeErr(w, http.StatusUnauthorized, "not authenticated")
		} else {
			s.authn.Challenge(w, r)
		}
		return
	}
	if user.Via != "token" && !sameOrigin(r, s.cfg.Scheme) {
		writeErr(w, http.StatusForbidden, "cross-origin request blocked")
		return
	}
	if isAPI(r.URL.Path) && !isFileContent(r) && !s.limiter.allow(user.Email) {
		writeErr(w, http.StatusTooManyRequests, "rate limit exceeded, slow down")
		return
	}
	mux.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
}

func (s *Server) identify(r *http.Request) *auth.User {
	if token, ok := auth.BearerToken(r); ok {
		owner, err := s.store.LookupToken(r.Context(), auth.HashToken(token))
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Error("token lookup", "err", err)
			}
			return nil
		}
		return &auth.User{Email: owner.Email, Name: owner.Name, Via: "token"}
	}
	return s.authn.Authenticate(r)
}

func isAPI(path string) bool {
	return strings.HasPrefix(path, "/_q/") && path != "/_q/cli/authorize"
}

// isFileContent matches GET /_q/files/{id}/{name}. Like static files, file bytes aren't rate
// limited: a gallery page legitimately loads dozens at once.
func isFileContent(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/_q/files/")
	return ok && r.Method == http.MethodGet && strings.Count(rest, "/") == 1
}

// sameOrigin blocks state-changing requests that a browser sent on behalf of another origin,
// including other sites on the same parent domain (which share the session cookie).
func sameOrigin(r *http.Request, scheme string) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	default:
		return false
	}
	if origin := r.Header.Get("Origin"); origin != "" && origin != scheme+"://"+r.Host {
		return false
	}
	return true
}

type siteKey struct{}

func siteFrom(ctx context.Context) string {
	name, _ := ctx.Value(siteKey{}).(string)
	return name
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) internalErr(w http.ResponseWriter, r *http.Request, err error) {
	slog.Error("request failed", "method", r.Method, "host", r.Host, "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, auth.UserFrom(r.Context()))
}
