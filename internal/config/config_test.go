package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

// setEnv clears every EZE_ variable the loader reads, then applies vars.
func setEnv(t *testing.T, vars map[string]string) {
	t.Helper()
	for _, k := range []string{
		"EZE_ADDR", "EZE_DOMAIN", "EZE_SCHEME", "EZE_DATA_DIR", "EZE_AUTH", "EZE_AUTH_EMAIL_HEADER",
		"EZE_AUTH_NAME_HEADER", "EZE_TRUSTED_PROXIES", "EZE_OIDC_ISSUER", "EZE_OIDC_CLIENT_ID",
		"EZE_OIDC_CLIENT_SECRET", "EZE_OIDC_ALLOWED_DOMAINS", "EZE_SESSION_SECRET", "EZE_DEV_EMAIL",
		"EZE_OVERWRITE", "EZE_MAX_DEPLOY_BYTES", "EZE_MAX_SITE_BYTES", "EZE_MAX_SITE_FILES",
		"EZE_MAX_DOC_BYTES", "EZE_RATE_LIMIT", "EZE_RATE_BURST", "EZE_MAX_UPLOAD_BYTES", "EZE_UPLOAD_QUOTA_BYTES",
		"EZE_ANTHROPIC_API_KEY", "EZE_ANTHROPIC_BASE_URL", "EZE_AI_MODELS", "EZE_AI_DEFAULT_MODEL", "EZE_AI_DAILY_TOKENS",
		"EZE_AI_MAX_OUTPUT_TOKENS", "EZE_OIDC_ALLOW_PUBLIC_DOMAINS", "EZE_OIDC_ALLOW_UNVERIFIED_EMAIL",
	} {
		t.Setenv(k, "")
	}
	for k, v := range vars {
		t.Setenv(k, v)
	}
}

var secret = base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantErr string
	}{
		{"no domain", map[string]string{"EZE_AUTH": "header"}, "EZE_DOMAIN"},
		{"no auth", map[string]string{"EZE_DOMAIN": "eze.page"}, "EZE_AUTH is required"},
		{"unknown auth", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "none"}, "unknown"},
		{"header ok", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "header"}, ""},
		{"bad proxy cidr", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "header", "EZE_TRUSTED_PROXIES": "10.0.0.1"}, "EZE_TRUSTED_PROXIES"},
		{"oidc missing client", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "oidc"}, "EZE_OIDC_ISSUER"},
		{"oidc missing domains", map[string]string{
			"EZE_DOMAIN": "eze.page", "EZE_AUTH": "oidc", "EZE_OIDC_ISSUER": "https://idp", "EZE_OIDC_CLIENT_ID": "id",
			"EZE_OIDC_CLIENT_SECRET": "s", "EZE_SESSION_SECRET": secret,
		}, "EZE_OIDC_ALLOWED_DOMAINS"},
		{"oidc short secret", map[string]string{
			"EZE_DOMAIN": "eze.page", "EZE_AUTH": "oidc", "EZE_OIDC_ISSUER": "https://idp", "EZE_OIDC_CLIENT_ID": "id",
			"EZE_OIDC_CLIENT_SECRET": "s", "EZE_OIDC_ALLOWED_DOMAINS": "example.com", "EZE_SESSION_SECRET": "c2hvcnQ=",
		}, "EZE_SESSION_SECRET"},
		{"oidc ok", map[string]string{
			"EZE_DOMAIN": "eze.page", "EZE_AUTH": "oidc", "EZE_OIDC_ISSUER": "https://idp", "EZE_OIDC_CLIENT_ID": "id",
			"EZE_OIDC_CLIENT_SECRET": "s", "EZE_OIDC_ALLOWED_DOMAINS": "Example.com, corp.io", "EZE_SESSION_SECRET": secret,
		}, ""},
		{"oidc public email domain", map[string]string{
			"EZE_DOMAIN": "eze.page", "EZE_AUTH": "oidc", "EZE_OIDC_ISSUER": "https://idp", "EZE_OIDC_CLIENT_ID": "id",
			"EZE_OIDC_CLIENT_SECRET": "s", "EZE_OIDC_ALLOWED_DOMAINS": "example.com,Gmail.com", "EZE_SESSION_SECRET": secret,
		}, "anyone on the internet"},
		{"oidc public email domain, explicitly allowed", map[string]string{
			"EZE_DOMAIN": "eze.page", "EZE_AUTH": "oidc", "EZE_OIDC_ISSUER": "https://idp", "EZE_OIDC_CLIENT_ID": "id",
			"EZE_OIDC_CLIENT_SECRET": "s", "EZE_OIDC_ALLOWED_DOMAINS": "gmail.com", "EZE_SESSION_SECRET": secret,
			"EZE_OIDC_ALLOW_PUBLIC_DOMAINS": "true",
		}, ""},
		{"dev on all interfaces", map[string]string{"EZE_DOMAIN": "localhost:8080", "EZE_AUTH": "dev"}, "loopback"},
		{"dev on public ip", map[string]string{"EZE_DOMAIN": "localhost:8080", "EZE_AUTH": "dev", "EZE_ADDR": "0.0.0.0:8080"}, "loopback"},
		{"dev on loopback", map[string]string{"EZE_DOMAIN": "localhost:8080", "EZE_AUTH": "dev", "EZE_ADDR": "127.0.0.1:8080"}, ""},
		{"dev on localhost", map[string]string{"EZE_DOMAIN": "localhost:8080", "EZE_AUTH": "dev", "EZE_ADDR": "localhost:8080"}, ""},
		{"bad scheme", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "header", "EZE_SCHEME": "ftp"}, "EZE_SCHEME"},
		{"bad overwrite", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "header", "EZE_OVERWRITE": "nobody"}, "EZE_OVERWRITE"},
		{"default model not allowed", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "header", "EZE_AI_MODELS": "claude-haiku-5-5"}, "EZE_AI_DEFAULT_MODEL"},
		{"custom models", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "header", "EZE_AI_MODELS": "claude-haiku-5-5", "EZE_AI_DEFAULT_MODEL": "claude-haiku-5-5"}, ""},
		{"bad int", map[string]string{"EZE_DOMAIN": "eze.page", "EZE_AUTH": "header", "EZE_MAX_DOC_BYTES": "-1"}, "EZE_MAX_DOC_BYTES"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setEnv(t, tt.env)
			_, err := Load()
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("got %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoadNormalises(t *testing.T) {
	setEnv(t, map[string]string{
		"EZE_DOMAIN": "EZE.page", "EZE_AUTH": "oidc", "EZE_OIDC_ISSUER": "https://idp", "EZE_OIDC_CLIENT_ID": "id",
		"EZE_OIDC_CLIENT_SECRET": "s", "EZE_OIDC_ALLOWED_DOMAINS": "Example.com, corp.io ,", "EZE_SESSION_SECRET": secret,
	})
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.Domain != "eze.page" {
		t.Errorf("domain = %q", c.Domain)
	}
	if strings.Join(c.AllowedEmailDomains, ",") != "example.com,corp.io" {
		t.Errorf("allowed domains = %v", c.AllowedEmailDomains)
	}
	if c.SiteURL("blog") != "https://blog.eze.page/" || c.ApexURL("/x") != "https://eze.page/x" {
		t.Errorf("urls: %s %s", c.SiteURL("blog"), c.ApexURL("/x"))
	}
}

func TestCookieDomain(t *testing.T) {
	for domain, want := range map[string]string{"eze.page": "eze.page", "localhost:8080": "localhost"} {
		if got := (&Config{Domain: domain}).CookieDomain(); got != want {
			t.Errorf("CookieDomain(%q) = %q, want %q", domain, got, want)
		}
	}
}
