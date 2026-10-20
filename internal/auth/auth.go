// Package auth identifies who is making a request. Every request to eze must be authenticated:
// by an API token, by headers from a trusted auth proxy, or by an OIDC session cookie.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"strings"
)

type User struct {
	Email string `json:"email"`
	Name  string `json:"name"`
	// Via records how the request was authenticated: "token", "header", "oidc" or "dev".
	Via string `json:"-"`
}

// Authenticator identifies browser requests.
type Authenticator interface {
	// Authenticate returns the user for r, or nil if the request carries no valid identity.
	Authenticate(r *http.Request) *User
	// Challenge responds to an unauthenticated browser request, e.g. by redirecting to a login page.
	Challenge(w http.ResponseWriter, r *http.Request)
	// Mount registers login routes on the apex host's mux. Routes mounted here are reachable without auth.
	Mount(mux *http.ServeMux)
}

type ctxKey struct{}

func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

func UserFrom(ctx context.Context) *User {
	u, _ := ctx.Value(ctxKey{}).(*User)
	return u
}

// nameFromEmail derives a display name when the identity source only provides an email.
func nameFromEmail(email string) string {
	local, _, _ := strings.Cut(email, "@")
	return local
}

const tokenPrefix = "eze_"

// NewToken returns a random API token and the hash to store for it.
func NewToken() (token, hash string) {
	b := make([]byte, 32)
	rand.Read(b)
	token = tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return token, HashToken(token)
}

func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// BearerToken extracts an eze API token from the Authorization header.
func BearerToken(r *http.Request) (string, bool) {
	h := r.Header.Get("Authorization")
	token, ok := strings.CutPrefix(h, "Bearer ")
	if !ok || !strings.HasPrefix(token, tokenPrefix) {
		return "", false
	}
	return token, true
}
