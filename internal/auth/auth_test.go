package auth

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestHeaderAuth(t *testing.T) {
	h := &Header{
		EmailHeader:    "X-Forwarded-Email",
		NameHeader:     "X-Forwarded-User",
		TrustedProxies: []netip.Prefix{netip.MustParsePrefix("127.0.0.1/32"), netip.MustParsePrefix("10.0.0.0/8")},
	}
	req := func(remote, email, name string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if email != "" {
			r.Header.Set("X-Forwarded-Email", email)
		}
		if name != "" {
			r.Header.Set("X-Forwarded-User", name)
		}
		return r
	}

	u := h.Authenticate(req("127.0.0.1:5555", "Ana@Example.com", "Ana Lima"))
	if u == nil || u.Email != "ana@example.com" || u.Name != "Ana Lima" || u.Via != "header" {
		t.Errorf("trusted proxy: %+v", u)
	}
	if u := h.Authenticate(req("10.1.2.3:80", "bo@example.com", "")); u == nil || u.Name != "bo" {
		t.Errorf("name fallback: %+v", u)
	}
	if u := h.Authenticate(req("10.1.2.3:80", "bo@example.com", "bo@example.com")); u == nil || u.Name != "bo" {
		t.Errorf("email as name should fall back to local part: %+v", u)
	}
	if u := h.Authenticate(req("[::ffff:127.0.0.1]:5555", "ana@example.com", "")); u == nil {
		t.Error("IPv4-mapped IPv6 loopback should be trusted")
	}
	if u := h.Authenticate(req("203.0.113.9:5555", "ana@example.com", "")); u != nil {
		t.Errorf("untrusted peer was believed: %+v", u)
	}
	if u := h.Authenticate(req("127.0.0.1:5555", "", "")); u != nil {
		t.Errorf("missing header authenticated: %+v", u)
	}
	if u := h.Authenticate(req("127.0.0.1:5555", "not-an-email", "")); u != nil {
		t.Errorf("non-email accepted: %+v", u)
	}
	if u := h.Authenticate(req("garbage", "ana@example.com", "")); u != nil {
		t.Errorf("unparseable remote addr trusted: %+v", u)
	}
}

func TestBearerToken(t *testing.T) {
	token, hash := NewToken()
	if !strings.HasPrefix(token, "eze_") || hash != HashToken(token) || len(hash) != 64 {
		t.Fatalf("token=%q hash=%q", token, hash)
	}
	if other, _ := NewToken(); other == token {
		t.Fatal("tokens not random")
	}
	for header, want := range map[string]bool{
		"Bearer " + token: true,
		"bearer " + token: false,
		token:             false,
		"Bearer abc":      false, // not an eze token, e.g. a JWT meant for a proxy
		"":                false,
	} {
		r := httptest.NewRequest("GET", "/", nil)
		if header != "" {
			r.Header.Set("Authorization", header)
		}
		got, ok := BearerToken(r)
		if ok != want || (ok && got != token) {
			t.Errorf("BearerToken(%q) = %q, %v", header, got, ok)
		}
	}
}

func newTestOIDC() *OIDC {
	return &OIDC{cfg: OIDCConfig{
		Secret:       []byte("0123456789abcdef0123456789abcdef"),
		Domain:       "eze.test",
		CookieDomain: "eze.test",
		Scheme:       "https",
	}}
}

func TestSealUnseal(t *testing.T) {
	o := newTestOIDC()
	in := session{Email: "ana@example.com", Name: "Ana", Exp: 42}
	sealed := o.seal(in)

	var out session
	if err := o.unseal(sealed, &out); err != nil || out != in {
		t.Fatalf("roundtrip = %+v, %v", out, err)
	}

	payload, sig, _ := strings.Cut(sealed, ".")
	flipped := []byte(sig)
	if flipped[0] == 'A' {
		flipped[0] = 'B'
	} else {
		flipped[0] = 'A'
	}
	for name, value := range map[string]string{
		"tampered payload": payload + "x." + sig,
		"tampered sig":     payload + "." + string(flipped),
		"no sig":           payload,
		"empty":            "",
	} {
		if err := o.unseal(value, &out); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	other := newTestOIDC()
	other.cfg.Secret = []byte("ffffffffffffffffffffffffffffffff")
	if err := other.unseal(sealed, &out); err == nil {
		t.Error("cookie accepted under a different secret")
	}
}

func TestOIDCAuthenticate(t *testing.T) {
	o := newTestOIDC()
	withCookie := func(s session) *http.Request {
		r := httptest.NewRequest("GET", "https://blog.eze.test/", nil)
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: o.seal(s)})
		return r
	}
	if u := o.Authenticate(withCookie(session{Email: "ana@example.com", Name: "Ana", Exp: time.Now().Add(time.Hour).Unix()})); u == nil || u.Email != "ana@example.com" || u.Via != "oidc" {
		t.Errorf("valid session: %+v", u)
	}
	if u := o.Authenticate(withCookie(session{Email: "ana@example.com", Exp: time.Now().Add(-time.Second).Unix()})); u != nil {
		t.Errorf("expired session accepted: %+v", u)
	}
	if u := o.Authenticate(httptest.NewRequest("GET", "/", nil)); u != nil {
		t.Errorf("no cookie accepted: %+v", u)
	}
}

func TestOIDCChallenge(t *testing.T) {
	o := newTestOIDC()
	w := httptest.NewRecorder()
	o.Challenge(w, httptest.NewRequest("GET", "https://blog.eze.test/a?b=1", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("code = %d", w.Code)
	}
	loc, _ := url.Parse(w.Header().Get("Location"))
	if loc.Host != "eze.test" || loc.Path != LoginPath || loc.Query().Get("rd") != "https://blog.eze.test/a?b=1" {
		t.Errorf("location = %s", loc)
	}

	w = httptest.NewRecorder()
	o.Challenge(w, httptest.NewRequest("POST", "https://blog.eze.test/", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("POST challenge = %d, want 401", w.Code)
	}
}

func TestSafeRedirect(t *testing.T) {
	o := newTestOIDC()
	home := "https://eze.test/"
	for in, want := range map[string]string{
		"https://eze.test/x":          "https://eze.test/x",
		"https://blog.eze.test/a?b=1": "https://blog.eze.test/a?b=1",
		"https://evil.com/":           home,
		"https://eze.test.evil.com/":  home,
		"https://evileze.test/":       home,
		"http://blog.eze.test/":       home, // scheme downgrade
		"//evil.com/":                 home,
		"javascript:alert(1)":         home,
		"https://user@blog.eze.test/": home,
		"":                            home,
	} {
		if got := o.safeRedirect(in); got != want {
			t.Errorf("safeRedirect(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDev(t *testing.T) {
	d := &Dev{Email: "dev@localhost"}
	if u := d.Authenticate(httptest.NewRequest("GET", "/", nil)); u.Email != "dev@localhost" || u.Name != "dev" {
		t.Errorf("dev user = %+v", u)
	}
}

func TestEmailVerified(t *testing.T) {
	for claim, want := range map[any]bool{true: true, false: false, "true": true, "false": false, "": false, 1.0: false} {
		if got := emailVerified(claim); got != want {
			t.Errorf("emailVerified(%#v) = %v", claim, got)
		}
	}
	if emailVerified(nil) {
		t.Error("a missing claim must not count as verified")
	}
}
