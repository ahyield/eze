// Command eze is both the server and the CLI for eze, a zero-config internal hosting platform.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ahyield/eze/internal/auth"
	"github.com/ahyield/eze/internal/cli"
	"github.com/ahyield/eze/internal/config"
	"github.com/ahyield/eze/internal/server"
	"github.com/ahyield/eze/internal/sites"
	"github.com/ahyield/eze/internal/store"
)

const usage = `eze: ship a folder of HTML to a private URL.

Usage:
  eze login <server-url>        sign the CLI in through your browser
  eze init [dir] [--site name]  scaffold a site with agent instructions
  eze deploy [dir] [--site name]  upload a folder; replaces the site atomically
  eze ls                        list all sites
  eze rm <site> [--yes]         delete a site, its files and its data
  eze whoami                    show the signed-in user
  eze tokens [revoke <id>]      list or revoke your API tokens
  eze tokens revoke --user <email>  revoke all of someone's tokens (offboarding)
  eze logout                    revoke this computer's token
  eze mcp                       serve eze's tools to an MCP client (Claude Code, Cursor...)
  eze serve                     run the server (configured with EZE_* env vars)
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmds := map[string]func([]string) error{
		"login":  cli.Login,
		"init":   cli.Init,
		"deploy": cli.Deploy,
		"ls":     cli.List,
		"rm":     cli.Remove,
		"whoami": cli.Whoami,
		"tokens": cli.Tokens,
		"logout": cli.Logout,
		"mcp":    cli.MCP,
		"serve":  func([]string) error { return serve() },
	}
	cmd, ok := cmds[os.Args[1]]
	if !ok {
		if os.Args[1] != "help" && os.Args[1] != "-h" && os.Args[1] != "--help" {
			fmt.Fprintf(os.Stderr, "unknown command %q\n\n", os.Args[1])
		}
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func serve() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}
	st, err := store.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	defer st.Close()
	ss, err := sites.NewStore(cfg.DataDir, cfg.MaxSiteBytes, cfg.MaxSiteFiles)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var authn auth.Authenticator
	switch cfg.AuthMode {
	case config.AuthHeader:
		authn = &auth.Header{EmailHeader: cfg.EmailHeader, NameHeader: cfg.NameHeader, TrustedProxies: cfg.TrustedProxies}
	case config.AuthOIDC:
		authn, err = auth.NewOIDC(ctx, auth.OIDCConfig{
			Issuer:               cfg.OIDCIssuer,
			ClientID:             cfg.OIDCClientID,
			ClientSecret:         cfg.OIDCClientSecret,
			AllowedDomains:       cfg.AllowedEmailDomains,
			AllowUnverifiedEmail: cfg.AllowUnverifiedEmail,
			Secret:               cfg.SessionSecret,
			Domain:               cfg.Domain,
			CookieDomain:         cfg.CookieDomain(),
			Scheme:               cfg.Scheme,
		})
		if err != nil {
			return fmt.Errorf("oidc: %w", err)
		}
	case config.AuthDev:
		slog.Warn("EZE_AUTH=dev: every request is treated as " + cfg.DevEmail + ". Never use this outside your own machine.")
		authn = &auth.Dev{Email: cfg.DevEmail}
	}

	handler, err := server.New(cfg, authn, st, ss)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	slog.Info("eze listening", "addr", cfg.Addr, "domain", cfg.Domain, "auth", cfg.AuthMode)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
