package auth

import (
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// Header trusts identity headers set by an upstream auth proxy such as oauth2-proxy,
// Pomerium, Cloudflare Access or Tailscale Serve. Headers are only honoured when the
// TCP peer is one of the trusted proxy addresses, so a client that reaches eze
// directly cannot spoof them.
type Header struct {
	EmailHeader    string
	NameHeader     string
	TrustedProxies []netip.Prefix
}

func (h *Header) Authenticate(r *http.Request) *User {
	if !h.trusted(r.RemoteAddr) {
		return nil
	}
	email := strings.TrimSpace(r.Header.Get(h.EmailHeader))
	if email == "" || !strings.Contains(email, "@") {
		return nil
	}
	name := strings.TrimSpace(r.Header.Get(h.NameHeader))
	if name == "" || name == email {
		name = nameFromEmail(email)
	}
	return &User{Email: strings.ToLower(email), Name: name, Via: "header"}
}

func (h *Header) Challenge(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "Not authenticated. eze expects an auth proxy in front of it to set the "+h.EmailHeader+" header.", http.StatusUnauthorized)
}

func (h *Header) Mount(*http.ServeMux) {}

func (h *Header) trusted(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		host = remoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return false
	}
	ip = ip.Unmap()
	for _, p := range h.TrustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// Dev authenticates every request as a fixed user. Config only allows it on loopback addresses.
type Dev struct{ Email string }

func (d *Dev) Authenticate(*http.Request) *User {
	return &User{Email: d.Email, Name: nameFromEmail(d.Email), Via: "dev"}
}

func (d *Dev) Challenge(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}

func (d *Dev) Mount(*http.ServeMux) {}
