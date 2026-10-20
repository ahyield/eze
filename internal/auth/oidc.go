package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"
)

const (
	sessionCookie = "eze_session"
	loginCookie   = "eze_login"
	sessionTTL    = 7 * 24 * time.Hour
	loginTTL      = 10 * time.Minute

	LoginPath    = "/_q/auth/login"
	CallbackPath = "/_q/auth/callback"
	LogoutPath   = "/_q/auth/logout"
)

type OIDCConfig struct {
	Issuer         string
	ClientID       string
	ClientSecret   string
	AllowedDomains []string // lower-case email domains allowed to sign in
	Secret         []byte   // HMAC key for cookies
	Domain         string   // apex host, may include a port
	CookieDomain   string   // apex hostname without port
	Scheme         string
}

// OIDC signs users in with any OpenID Connect provider (Google Workspace, Okta, Entra ID, Keycloak...)
// and keeps them signed in with an HMAC-signed cookie scoped to the apex domain, so one login covers every site.
type OIDC struct {
	cfg      OIDCConfig
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
}

func NewOIDC(ctx context.Context, cfg OIDCConfig) (*OIDC, error) {
	provider, err := oidc.NewProvider(ctx, cfg.Issuer)
	if err != nil {
		return nil, err
	}
	return &OIDC{
		cfg: cfg,
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.Scheme + "://" + cfg.Domain + CallbackPath,
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
	}, nil
}

type session struct {
	Email string `json:"e"`
	Name  string `json:"n"`
	Exp   int64  `json:"x"`
}

type loginState struct {
	State    string `json:"s"`
	Nonce    string `json:"n"`
	Verifier string `json:"v"`
	Redirect string `json:"r"`
	Exp      int64  `json:"x"`
}

func (o *OIDC) Authenticate(r *http.Request) *User {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return nil
	}
	var s session
	if err := o.unseal(c.Value, &s); err != nil || time.Now().Unix() > s.Exp {
		return nil
	}
	return &User{Email: s.Email, Name: s.Name, Via: "oidc"}
}

func (o *OIDC) Challenge(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	current := o.cfg.Scheme + "://" + r.Host + r.URL.RequestURI()
	http.Redirect(w, r, o.cfg.Scheme+"://"+o.cfg.Domain+LoginPath+"?rd="+url.QueryEscape(current), http.StatusFound)
}

func (o *OIDC) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET "+LoginPath, o.login)
	mux.HandleFunc("GET "+CallbackPath, o.callback)
	mux.HandleFunc("POST "+LogoutPath, o.logout)
}

func (o *OIDC) login(w http.ResponseWriter, r *http.Request) {
	st := loginState{
		State:    randString(),
		Nonce:    randString(),
		Verifier: oauth2.GenerateVerifier(),
		Redirect: o.safeRedirect(r.URL.Query().Get("rd")),
		Exp:      time.Now().Add(loginTTL).Unix(),
	}
	http.SetCookie(w, &http.Cookie{
		Name:     loginCookie,
		Value:    o.seal(st),
		Path:     "/_q/auth/",
		MaxAge:   int(loginTTL.Seconds()),
		HttpOnly: true,
		Secure:   o.cfg.Scheme == "https",
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, o.oauth.AuthCodeURL(st.State, oidc.Nonce(st.Nonce), oauth2.S256ChallengeOption(st.Verifier)), http.StatusFound)
}

func (o *OIDC) callback(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(loginCookie)
	var st loginState
	if err != nil || o.unseal(c.Value, &st) != nil || time.Now().Unix() > st.Exp {
		http.Error(w, "Login expired, please try again.", http.StatusBadRequest)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: loginCookie, Path: "/_q/auth/", MaxAge: -1})

	q := r.URL.Query()
	if e := q.Get("error"); e != "" {
		http.Error(w, "Sign-in failed: "+e, http.StatusForbidden)
		return
	}
	if !hmac.Equal([]byte(q.Get("state")), []byte(st.State)) {
		http.Error(w, "Invalid login state, please try again.", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	tok, err := o.oauth.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(st.Verifier))
	if err != nil {
		slog.Warn("oidc exchange failed", "err", err)
		http.Error(w, "Sign-in failed.", http.StatusBadGateway)
		return
	}
	raw, _ := tok.Extra("id_token").(string)
	idToken, err := o.verifier.Verify(ctx, raw)
	if err != nil || idToken.Nonce != st.Nonce {
		slog.Warn("oidc id token rejected", "err", err)
		http.Error(w, "Sign-in failed.", http.StatusForbidden)
		return
	}
	var claims struct {
		Email         string `json:"email"`
		EmailVerified *bool  `json:"email_verified"`
		Name          string `json:"name"`
	}
	if err := idToken.Claims(&claims); err != nil {
		http.Error(w, "Sign-in failed.", http.StatusForbidden)
		return
	}
	email := strings.ToLower(claims.Email)
	_, domain, _ := strings.Cut(email, "@")
	if email == "" || (claims.EmailVerified != nil && !*claims.EmailVerified) || !slices.Contains(o.cfg.AllowedDomains, domain) {
		http.Error(w, "This account is not allowed to use eze.", http.StatusForbidden)
		return
	}
	name := claims.Name
	if name == "" {
		name = nameFromEmail(email)
	}

	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    o.seal(session{Email: email, Name: name, Exp: time.Now().Add(sessionTTL).Unix()}),
		Domain:   o.cfg.CookieDomain,
		Path:     "/",
		MaxAge:   int(sessionTTL.Seconds()),
		HttpOnly: true,
		Secure:   o.cfg.Scheme == "https",
		SameSite: http.SameSiteLaxMode,
	})
	http.Redirect(w, r, st.Redirect, http.StatusFound)
}

func (o *OIDC) logout(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Domain: o.cfg.CookieDomain, Path: "/", MaxAge: -1})
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// safeRedirect only allows redirects back to the apex or one of its sites.
func (o *OIDC) safeRedirect(rd string) string {
	home := o.cfg.Scheme + "://" + o.cfg.Domain + "/"
	u, err := url.Parse(rd)
	if err != nil || u.Scheme != o.cfg.Scheme || u.User != nil {
		return home
	}
	host := strings.ToLower(u.Host)
	if host != o.cfg.Domain && !strings.HasSuffix(host, "."+o.cfg.Domain) {
		return home
	}
	return u.String()
}

// Signed cookies: base64(json) + "." + base64(hmac-sha256).

func (o *OIDC) seal(v any) string {
	payload, _ := json.Marshal(v)
	p := base64.RawURLEncoding.EncodeToString(payload)
	return p + "." + base64.RawURLEncoding.EncodeToString(o.mac(p))
}

func (o *OIDC) unseal(value string, v any) error {
	p, sig, ok := strings.Cut(value, ".")
	if !ok {
		return errors.New("malformed cookie")
	}
	got, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil || !hmac.Equal(got, o.mac(p)) {
		return errors.New("bad signature")
	}
	payload, err := base64.RawURLEncoding.DecodeString(p)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, v)
}

func (o *OIDC) mac(s string) []byte {
	m := hmac.New(sha256.New, o.cfg.Secret)
	m.Write([]byte(s))
	return m.Sum(nil)
}

func randString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}
