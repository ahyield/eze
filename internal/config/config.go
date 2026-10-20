// Package config loads server configuration from environment variables.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

const (
	AuthHeader = "header"
	AuthOIDC   = "oidc"
	AuthDev    = "dev"

	AIAnthropic = "anthropic"
	AIOpenAI    = "openai"

	OverwriteAnyone = "anyone"
	OverwriteOwner  = "owner"
)

type Config struct {
	Addr    string
	Domain  string // apex host, e.g. "eze.example.com" or "localhost:8080"; sites live at <site>.<Domain>
	Scheme  string
	DataDir string
	Storage string // "" keeps sites and uploads in DataDir; otherwise an s3:// or gs:// bucket URL

	AuthMode string

	// Header mode: identity comes from headers set by an upstream auth proxy.
	EmailHeader    string
	NameHeader     string
	TrustedProxies []netip.Prefix

	// OIDC mode.
	OIDCIssuer           string
	OIDCClientID         string
	OIDCClientSecret     string
	AllowedEmailDomains  []string
	AllowUnverifiedEmail bool
	SessionSecret        []byte

	// Dev mode: a fixed identity, loopback only.
	DevEmail string

	OverwritePolicy string

	MaxDeployBytes int64 // compressed upload size
	MaxSiteBytes   int64 // uncompressed site size
	MaxSiteFiles   int
	MaxDocBytes    int64

	MaxUploadBytes int64 // single uploaded file
	UploadQuota    int64 // total uploaded bytes per site

	// AI proxy; AIProvider is "" when AI is off.
	AIProvider        string
	AnthropicAPIKey   string
	AnthropicBaseURL  string // optional, e.g. a corporate gateway
	OpenAIAPIKey      string
	OpenAIBaseURL     string // an OpenAI-compatible server; empty means api.openai.com
	AIModels          []string
	AIDefaultModel    string
	AIDailyTokens     int64 // per user, input + output, across all sites
	AIMaxOutputTokens int64 // per request
	AIMaxConcurrent   int   // AI calls in flight per user

	TokenTTL time.Duration // lifetime of CLI API tokens

	RateLimit float64 // API requests per second per user
	RateBurst int
}

func Load() (*Config, error) {
	c := &Config{
		Addr:             env("EZE_ADDR", ":8080"),
		Domain:           strings.ToLower(env("EZE_DOMAIN", "")),
		Scheme:           env("EZE_SCHEME", "https"),
		DataDir:          env("EZE_DATA_DIR", "./data"),
		Storage:          env("EZE_STORAGE", ""),
		AuthMode:         env("EZE_AUTH", ""),
		EmailHeader:      env("EZE_AUTH_EMAIL_HEADER", "X-Forwarded-Email"),
		NameHeader:       env("EZE_AUTH_NAME_HEADER", "X-Forwarded-User"),
		OIDCIssuer:       env("EZE_OIDC_ISSUER", ""),
		OIDCClientID:     env("EZE_OIDC_CLIENT_ID", ""),
		OIDCClientSecret: env("EZE_OIDC_CLIENT_SECRET", ""),
		DevEmail:         env("EZE_DEV_EMAIL", "dev@localhost"),
		OverwritePolicy:  env("EZE_OVERWRITE", OverwriteAnyone),
	}

	if err := CheckStorage(c.Storage); err != nil {
		return nil, err
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
	if err := c.loadAI(); err != nil {
		return nil, err
	}
	if c.AIDailyTokens, err = envInt("EZE_AI_DAILY_TOKENS", 1_000_000); err != nil {
		return nil, err
	}
	if c.AIMaxOutputTokens, err = envInt("EZE_AI_MAX_OUTPUT_TOKENS", 16_000); err != nil {
		return nil, err
	}
	maxConcurrent, err := envInt("EZE_AI_MAX_CONCURRENT", 3)
	if err != nil {
		return nil, err
	}
	c.AIMaxConcurrent = int(maxConcurrent)
	ttlDays, err := envInt("EZE_TOKEN_TTL_DAYS", 90)
	if err != nil {
		return nil, err
	}
	c.TokenTTL = time.Duration(ttlDays) * 24 * time.Hour
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
		return nil, errors.New("EZE_DOMAIN is required (e.g. eze.example.com)")
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
		if env("EZE_OIDC_ALLOW_PUBLIC_DOMAINS", "") != "true" {
			for _, d := range c.AllowedEmailDomains {
				if publicEmailDomains[d] {
					return nil, fmt.Errorf("EZE_OIDC_ALLOWED_DOMAINS includes %s, which would let anyone on the internet sign in. "+
						"eze trusts every signed-in person (they can deploy, overwrite sites and spend AI budget), so it is meant "+
						"for one organisation's own domain. Set EZE_OIDC_ALLOW_PUBLIC_DOMAINS=true only if you really mean it", d)
				}
			}
		}
		c.AllowUnverifiedEmail = env("EZE_OIDC_ALLOW_UNVERIFIED_EMAIL", "") == "true"
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

func (c *Config) loadAI() error {
	c.AnthropicAPIKey = env("EZE_ANTHROPIC_API_KEY", "")
	c.AnthropicBaseURL = env("EZE_ANTHROPIC_BASE_URL", "")
	c.OpenAIAPIKey = env("EZE_OPENAI_API_KEY", "")
	c.OpenAIBaseURL = env("EZE_OPENAI_BASE_URL", "")
	anthropicSet := c.AnthropicAPIKey != ""
	openAISet := c.OpenAIAPIKey != "" || c.OpenAIBaseURL != ""

	c.AIProvider = env("EZE_AI_PROVIDER", "")
	switch c.AIProvider {
	case "":
		switch {
		case anthropicSet && openAISet:
			return errors.New("both Anthropic and OpenAI settings are present; set EZE_AI_PROVIDER to anthropic or openai")
		case anthropicSet:
			c.AIProvider = AIAnthropic
		case openAISet:
			c.AIProvider = AIOpenAI
		default:
			return nil // AI off
		}
	case AIAnthropic:
		if !anthropicSet {
			return errors.New("EZE_AI_PROVIDER=anthropic needs EZE_ANTHROPIC_API_KEY")
		}
	case AIOpenAI:
		if !openAISet {
			return errors.New("EZE_AI_PROVIDER=openai needs EZE_OPENAI_API_KEY, or EZE_OPENAI_BASE_URL for a local server")
		}
	default:
		return fmt.Errorf("EZE_AI_PROVIDER must be %q or %q", AIAnthropic, AIOpenAI)
	}
	if c.AIProvider == AIOpenAI && c.OpenAIBaseURL == "" && c.OpenAIAPIKey == "" {
		return errors.New("api.openai.com needs EZE_OPENAI_API_KEY")
	}

	models := env("EZE_AI_MODELS", "")
	if models == "" {
		if c.AIProvider != AIAnthropic {
			return errors.New("set EZE_AI_MODELS to the models pages may use, e.g. the ones your OpenAI-compatible server offers")
		}
		models = "claude-opus-5-5,claude-sonnet-5-5,claude-haiku-5-5"
	}
	c.AIModels = splitList(models)
	c.AIDefaultModel = env("EZE_AI_DEFAULT_MODEL", c.AIModels[0])
	if !slices.Contains(c.AIModels, c.AIDefaultModel) {
		return fmt.Errorf("EZE_AI_DEFAULT_MODEL %q must be one of EZE_AI_MODELS", c.AIDefaultModel)
	}
	return nil
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

// publicEmailDomains are free email providers: allowing one opens eze to the whole internet.
var publicEmailDomains = map[string]bool{
	"gmail.com": true, "googlemail.com": true, "outlook.com": true, "hotmail.com": true, "live.com": true,
	"msn.com": true, "yahoo.com": true, "ymail.com": true, "icloud.com": true, "me.com": true, "mac.com": true,
	"aol.com": true, "proton.me": true, "protonmail.com": true, "pm.me": true, "gmx.com": true, "gmx.de": true,
	"gmx.net": true, "mail.com": true, "yandex.com": true, "yandex.ru": true, "zoho.com": true, "qq.com": true,
	"163.com": true, "126.com": true, "fastmail.com": true, "hey.com": true, "tutanota.com": true, "tuta.io": true,
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

// CheckStorage validates EZE_STORAGE: empty, or an s3:// or gs:// URL naming a bucket.
func CheckStorage(raw string) error {
	if raw == "" {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "s3" && u.Scheme != "gs") || u.Host == "" {
		return errors.New("EZE_STORAGE must be s3://bucket or gs://bucket, optionally with ?options")
	}
	return nil
}
