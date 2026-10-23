// Package server routes requests for the apex host (index, CLI and deploy API) and for
// every site subdomain (static files plus the client-side APIs under /_eze/).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ahyield/eze/internal/ai"
	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
	"github.com/ahyield/eze/internal/uploads"
)

type Server struct {
	cfg      *config.Config
	authn    auth.Authenticator
	store    *store.Store
	sites    *sites.Store
	uploads  *uploads.Store
	hub      *hub
	rooms    *rooms
	ai       ai.Provider // nil when AI is not configured
	aiLedger *aiLedger
	limiter  *rateLimiter

	public *http.ServeMux // apex routes reachable without a user (login flow)
	apex   *http.ServeMux
	site   *http.ServeMux
}

func New(cfg *config.Config, authn auth.Authenticator, st *store.Store, ss *sites.Store, up *uploads.Store) (*Server, error) {
	s := &Server{
		cfg:      cfg,
		authn:    authn,
		store:    st,
		sites:    ss,
		uploads:  up,
		hub:      newHub(),
		rooms:    newRooms(),
		aiLedger: newAILedger(),
		limiter:  newRateLimiter(cfg.RateLimit, cfg.RateBurst),
		public:   http.NewServeMux(),
		apex:     http.NewServeMux(),
		site:     http.NewServeMux(),
	}
	switch cfg.AIProvider {
	case config.AIAnthropic:
		s.ai = ai.NewAnthropic(cfg.AnthropicAPIKey, cfg.AnthropicBaseURL)
	case config.AIOpenAI:
		s.ai = ai.NewOpenAI(cfg.OpenAIAPIKey, cfg.OpenAIBaseURL)
	}
	authn.Mount(s.public)

	s.apex.HandleFunc("GET /{$}", s.handleIndex)
	s.apex.HandleFunc("GET /sites/{name}", s.handleSitePage)
	s.apex.HandleFunc("GET /_eze/app.js", serveAsset("text/javascript; charset=utf-8", appJS))
	s.apex.HandleFunc("GET /_eze/app.css", serveAsset("text/css; charset=utf-8", appCSS))
	s.apex.HandleFunc("GET /_eze/sdk.js", s.handleSDK)
	s.apex.HandleFunc("GET /_eze/me", s.handleMe)
	s.apex.HandleFunc("GET /_eze/cli/authorize", s.handleCLIAuthorizePage)
	s.apex.HandleFunc("POST /_eze/cli/authorize", s.handleCLIAuthorize)
	s.apex.HandleFunc("GET /_eze/api/me", s.handleMe)
	s.apex.HandleFunc("GET /_eze/api/sites", s.handleListSites)
	s.apex.HandleFunc("GET /_eze/api/sites/{name}", s.handleGetSite)
	s.apex.HandleFunc("PUT /_eze/api/sites/{name}", s.handleDeploy)
	s.apex.HandleFunc("DELETE /_eze/api/sites/{name}", s.handleDeleteSite)
	s.apex.HandleFunc("GET /_eze/api/tokens", s.handleListTokens)
	s.apex.HandleFunc("DELETE /_eze/api/tokens/current", s.handleRevokeCurrentToken)
	s.apex.HandleFunc("DELETE /_eze/api/tokens/{id}", s.handleRevokeToken)
	s.apex.HandleFunc("DELETE /_eze/api/users/{email}/tokens", s.handleRevokeUserTokens)
	// The dashboard's own routes: behind normal sign-in, unlike /_eze/api/ (see identify).
	s.apex.HandleFunc("PUT /_eze/sites/{name}", s.handleDeploy)
	s.apex.HandleFunc("DELETE /_eze/sites/{name}", s.handleDeleteSite)

	s.site.HandleFunc("GET /_eze/sdk.js", s.handleSDK)
	s.site.HandleFunc("GET /_eze/me", s.handleMe)
	s.site.HandleFunc("GET /_eze/db/{collection}", s.handleListDocs)
	s.site.HandleFunc("POST /_eze/db/{collection}", s.handleCreateDoc)
	s.site.HandleFunc("GET /_eze/db/{collection}/{id}", s.handleGetDoc)
	s.site.HandleFunc("PUT /_eze/db/{collection}/{id}", s.handleSetDoc)
	s.site.HandleFunc("PATCH /_eze/db/{collection}/{id}", s.handleUpdateDoc)
	s.site.HandleFunc("DELETE /_eze/db/{collection}/{id}", s.handleDeleteDoc)
	s.site.HandleFunc("GET /_eze/subscribe/{collection}", s.handleSubscribe)
	s.site.HandleFunc("GET /_eze/ws/{room}", s.handleRoom)
	s.site.HandleFunc("POST /_eze/ai/chat", s.handleAIChat)
	s.site.HandleFunc("GET /_eze/ai/usage", s.handleAIUsage)
	s.site.HandleFunc("POST /_eze/files", s.handleUpload)
	s.site.HandleFunc("GET /_eze/files", s.handleListFiles)
	s.site.HandleFunc("GET /_eze/files/{id}", s.handleGetFile)
	s.site.HandleFunc("GET /_eze/files/{id}/{name}", s.handleFileContent)
	s.site.HandleFunc("DELETE /_eze/files/{id}", s.handleDeleteFile)
	// Must carry a method: a method-less "/_eze/" would conflict with "GET /" in ServeMux.
	s.site.HandleFunc("GET /_eze/", func(w http.ResponseWriter, r *http.Request) {
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
		if strings.HasPrefix(r.URL.Path, "/_eze/auth/") {
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
	// Site APIs only exist for deployed sites; otherwise any made-up name would be a free
	// database and upload quota.
	if site := siteFrom(r.Context()); site != "" && isAPI(r.URL.Path) && !s.sites.Exists(site) {
		writeErr(w, http.StatusNotFound, "no site named "+site+"; deploy it first")
		return
	}
	mux.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), user)))
}

// identify returns the user making r. The CLI's /_eze/api/ routes accept only API tokens:
// operators are told to let /_eze/api/ past their auth proxy, so identity headers or cookies
// arriving there must not count.
func (s *Server) identify(r *http.Request) *auth.User {
	token, hasToken := auth.BearerToken(r)
	if !hasToken && strings.ToLower(r.Host) == s.cfg.Domain && strings.HasPrefix(r.URL.Path, "/_eze/api/") {
		return nil
	}
	if hasToken {
		hash := auth.HashToken(token)
		owner, err := s.store.LookupToken(r.Context(), hash)
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				slog.Error("token lookup", "err", err)
			}
			return nil
		}
		return &auth.User{Email: owner.Email, Name: owner.Name, Via: "token", TokenHash: hash}
	}
	return s.authn.Authenticate(r)
}

func isAPI(path string) bool {
	return strings.HasPrefix(path, "/_eze/") && path != "/_eze/cli/authorize"
}

// isFileContent matches GET /_eze/files/{id}/{name}. Like static files, file bytes aren't rate
// limited: a gallery page legitimately loads dozens at once.
func isFileContent(r *http.Request) bool {
	rest, ok := strings.CutPrefix(r.URL.Path, "/_eze/files/")
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
	w.Header().Set("X-Content-Type-Options", "nosniff")
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
