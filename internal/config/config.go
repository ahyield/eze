// Package config loads server configuration from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"strconv"
	"strings"
)

const (
	AuthHeader = "header"
	AuthOIDC   = "oidc"
	AuthDev    = "dev"

	OverwriteAnyone = "anyone"
	OverwriteOwner  = "owner"
)

type Config struct {
	Addr    string
	Domain  string // apex host, e.g. "eze.page" or "localhost:8080"; sites live at <site>.<Domain>
	Scheme  string // "https" or "http"
	DataDir string

	AuthMode string

	// Header mode: identity comes from headers set by an upstream auth proxy.
	EmailHeader    string
	NameHeader     string
	TrustedProxies []netip.Prefix

	// OIDC mode.
	OIDCIssuer          string
	OIDCClientID        string
	OIDCClientSecret    string
	AllowedEmailDomains []string
	SessionSecret       []byte

	// Dev mode: a fixed identity, loopback only.
	DevEmail string

	OverwritePolicy string

	MaxDeployBytes int64 // compressed upload size
	MaxSiteBytes   int64 // uncompressed site size
	MaxSiteFiles   int
	MaxDocBytes    int64

	MaxUploadBytes int64 // single uploaded file
	UploadQuota    int64 // total uploaded bytes per site

	RateLimit float64 // API requests per second per user
	RateBurst int
}

func Load() (*Config, error) {
	c := &Config{
		Addr:             env("EZE_ADDR", ":8080"),
		Domain:           strings.ToLower(env("EZE_DOMAIN", "")),
		Scheme:           env("EZE_SCHEME", "https"),
		DataDir:          env("EZE_DATA_DIR", "./data"),
		AuthMode:         env("EZE_AUTH", ""),
		EmailHeader:      env("EZE_AUTH_EMAIL_HEADER", "X-Forwarded-Email"),
		NameHeader:       env("EZE_AUTH_NAME_HEADER", "X-Forwarded-User"),
		OIDCIssuer:       env("EZE_OIDC_ISSUER", ""),
		OIDCClientID:     env("EZE_OIDC_CLIENT_ID", ""),
		OIDCClientSecret: env("EZE_OIDC_CLIENT_SECRET", ""),
		DevEmail:         env("EZE_DEV_EMAIL", "dev@localhost"),
		OverwritePolicy:  env("EZE_OVERWRITE", OverwriteAnyone),
	}

	var err error
	if c.MaxDeployBytes, err = envInt("EZE_MAX_DEPLOY_BYTES", 50<<20); err != nil {
		return nil, err
	}
	if c.MaxSiteBytes, err = envInt("EZE_MAX_SITE_BYTES", 200<<20); err != nil {
		return nil, err
	}
	maxFiles, err := envInt("EZE_MAX_SITE_FILES", 10000)
	if err != nil {
		return nil, err
	}
	c.MaxSiteFiles = int(maxFiles)
	if c.MaxDocBytes, err = envInt("EZE_MAX_DOC_BYTES", 1<<20); err != nil {
		return nil, err
	}
	if c.MaxUploadBytes, err = envInt("EZE_MAX_UPLOAD_BYTES", 25<<20); err != nil {
		return nil, err
	}
	if c.UploadQuota, err = envInt("EZE_UPLOAD_QUOTA_BYTES", 1<<30); err != nil {
		return nil, err
	}
	rate, err := envInt("EZE_RATE_LIMIT", 20)
	if err != nil {
		return nil, err
	}
	c.RateLimit = float64(rate)
	burst, err := envInt("EZE_RATE_BURST", 60)
	if err != nil {
		return nil, err
	}
	c.RateBurst = int(burst)

	if c.Domain == "" {
		return nil, errors.New("EZE_DOMAIN is required (e.g. eze.page)")
	}
	if c.Scheme != "https" && c.Scheme != "http" {
		return nil, fmt.Errorf("EZE_SCHEME must be https or http, got %q", c.Scheme)
	}
	if c.OverwritePolicy != OverwriteAnyone && c.OverwritePolicy != OverwriteOwner {
		return nil, fmt.Errorf("EZE_OVERWRITE must be %q or %q", OverwriteAnyone, OverwriteOwner)
	}

	switch c.AuthMode {
	case AuthHeader:
		proxies := env("EZE_TRUSTED_PROXIES", "127.0.0.1/32,::1/128")
		for _, s := range splitList(proxies) {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				return nil, fmt.Errorf("EZE_TRUSTED_PROXIES: %w", err)
			}
			c.TrustedProxies = append(c.TrustedProxies, p)
		}
	case AuthOIDC:
		if c.OIDCIssuer == "" || c.OIDCClientID == "" || c.OIDCClientSecret == "" {
			return nil, errors.New("oidc auth requires EZE_OIDC_ISSUER, EZE_OIDC_CLIENT_ID and EZE_OIDC_CLIENT_SECRET")
		}
		c.AllowedEmailDomains = splitList(strings.ToLower(env("EZE_OIDC_ALLOWED_DOMAINS", "")))
		if len(c.AllowedEmailDomains) == 0 {
			return nil, errors.New("oidc auth requires EZE_OIDC_ALLOWED_DOMAINS so only your organisation can sign in")
		}
		secret, err := base64.StdEncoding.DecodeString(env("EZE_SESSION_SECRET", ""))
		if err != nil || len(secret) < 32 {
			return nil, errors.New("oidc auth requires EZE_SESSION_SECRET: at least 32 random bytes, base64 encoded (openssl rand -base64 32)")
		}
		c.SessionSecret = secret
	case AuthDev:
		if !loopbackAddr(c.Addr) {
			return nil, errors.New("EZE_AUTH=dev is only allowed when EZE_ADDR binds to a loopback address (e.g. 127.0.0.1:8080)")
		}
	case "":
		return nil, errors.New("EZE_AUTH is required: \"header\" (behind an auth proxy) or \"oidc\"; eze never runs without authentication")
	default:
		return nil, fmt.Errorf("unknown EZE_AUTH %q", c.AuthMode)
	}

	return c, nil
}

// CookieDomain is the apex hostname without port, used so a session covers every site.
func (c *Config) CookieDomain() string {
	if h, _, err := net.SplitHostPort(c.Domain); err == nil {
		return h
	}
	return c.Domain
}

// ApexURL returns the absolute URL of a path on the apex host.
func (c *Config) ApexURL(path string) string {
	return c.Scheme + "://" + c.Domain + path
}

// SiteURL returns the public URL of a site.
func (c *Config) SiteURL(site string) string {
	return c.Scheme + "://" + site + "." + c.Domain + "/"
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int64) (int64, error) {
	v := env(key, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func splitList(s string) []string {
	var out []string
	for part := range strings.SplitSeq(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

func loopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.IsLoopback()
}
