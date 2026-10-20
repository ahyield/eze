package cli

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// parseFlags parses flags that may appear before or after positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return positional, nil
		}
		positional = append(positional, args[0])
		args = args[1:]
	}
}

func Login(args []string) error {
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: eze login <server-url>")
	}
	server := strings.TrimRight(pos[0], "/")
	if !strings.Contains(server, "://") {
		server = "https://" + server
	}
	if _, err := url.Parse(server); err != nil {
		return fmt.Errorf("invalid server url: %w", err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	stateBytes := make([]byte, 24)
	rand.Read(stateBytes)
	state := base64.RawURLEncoding.EncodeToString(stateBytes)

	tokens := make(chan string, 1)
	srv := &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/callback" || r.ParseForm() != nil {
			http.Error(w, "invalid login callback", http.StatusBadRequest)
			return
		}
		q := r.PostForm
		if subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state)) != 1 || q.Get("token") == "" {
			http.Error(w, "invalid login callback", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>eze</title><body style="font:16px system-ui;display:grid;place-items:center;height:90vh">Logged in. You can close this tab.</body>`)
		select {
		case tokens <- q.Get("token"):
		default:
		}
	})}
	go srv.Serve(ln)
	defer srv.Close()

	authURL := server + "/_q/cli/authorize?" + url.Values{"port": {strconv.Itoa(port)}, "state": {state}}.Encode()
	fmt.Println("Opening your browser to sign in. If it doesn't open, visit:")
	fmt.Println("  " + authURL)
	openBrowser(authURL)

	var token string
	select {
	case token = <-tokens:
	case <-time.After(5 * time.Minute):
		return errors.New("timed out waiting for the browser login")
	}

	cfg := &Config{Server: server, Token: token}
	c := &client{cfg: cfg, http: &http.Client{Timeout: 30 * time.Second}}
	var me user
	if err := c.do("GET", "/_q/api/me", nil, "", &me); err != nil {
		return fmt.Errorf("verifying new token: %w", err)
	}
	path, err := saveConfig(cfg)
	if err != nil {
		return err
	}
	fmt.Printf("Logged in to %s as %s (saved to %s)\n", server, me.Email, path)
	return nil
}

func openBrowser(u string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", u)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	_ = cmd.Start()
}

func Whoami(args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var me user
	if err := c.do("GET", "/_q/api/me", nil, "", &me); err != nil {
		return err
	}
	fmt.Printf("%s (%s) on %s\n", me.Email, me.Name, c.cfg.Server)
	return nil
}

func List(args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	var list []site
	if err := c.do("GET", "/_q/api/sites", nil, "", &list); err != nil {
		return err
	}
	if len(list) == 0 {
		fmt.Println("No sites yet.")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SITE\tLAST DEPLOYED BY\tUPDATED\tURL")
	for _, s := range list {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", s.Name, s.DeployedBy, s.UpdatedAt.Local().Format("2006-01-02 15:04"), s.URL)
	}
	return tw.Flush()
}

func Remove(args []string) error {
	fs := flag.NewFlagSet("rm", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "skip the confirmation prompt")
	pos, err := parseFlags(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errors.New("usage: eze rm <site> [--yes]")
	}
	name := pos[0]
	c, err := newClient()
	if err != nil {
		return err
	}
	if !*yes {
		fmt.Printf("This deletes %s, its files and its database for everyone. Type the site name to confirm: ", name)
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if strings.TrimSpace(line) != name {
			return errors.New("aborted")
		}
	}
	if err := c.do("DELETE", "/_q/api/sites/"+url.PathEscape(name), nil, "", nil); err != nil {
		return err
	}
	fmt.Println("Deleted", name)
	return nil
}

type tokenInfo struct {
	ID         string     `json:"id"`
	Label      string     `json:"label"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
	ExpiresAt  time.Time  `json:"expires_at"`
	Current    bool       `json:"current"`
}

// Tokens lists or revokes API tokens: `eze tokens`, `eze tokens revoke <id>`,
// `eze tokens revoke --user <email>`.
func Tokens(args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	if len(args) == 0 || args[0] == "ls" {
		var tokens []tokenInfo
		if err := c.do("GET", "/_q/api/tokens", nil, "", &tokens); err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tLABEL\tCREATED\tLAST USED\tEXPIRES")
		for _, t := range tokens {
			used := "never"
			if t.LastUsedAt != nil {
				used = t.LastUsedAt.Local().Format("2006-01-02")
			}
			id := t.ID
			if t.Current {
				id += " (this one)"
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", id, t.Label, t.CreatedAt.Local().Format("2006-01-02"), used, t.ExpiresAt.Local().Format("2006-01-02"))
		}
		return tw.Flush()
	}
	if args[0] != "revoke" {
		return errors.New("usage: eze tokens [ls] | eze tokens revoke <id> | eze tokens revoke --user <email>")
	}
	fs := flag.NewFlagSet("tokens revoke", flag.ContinueOnError)
	userFlag := fs.String("user", "", "revoke every token of this person, e.g. when they leave")
	pos, err := parseFlags(fs, args[1:])
	if err != nil {
		return err
	}
	switch {
	case *userFlag != "" && len(pos) == 0:
		var res struct {
			Revoked int `json:"revoked"`
		}
		if err := c.do("DELETE", "/_q/api/users/"+url.PathEscape(*userFlag)+"/tokens", nil, "", &res); err != nil {
			return err
		}
		fmt.Printf("Revoked %d token(s) of %s\n", res.Revoked, *userFlag)
	case *userFlag == "" && len(pos) == 1:
		if err := c.do("DELETE", "/_q/api/tokens/"+url.PathEscape(pos[0]), nil, "", nil); err != nil {
			return err
		}
		fmt.Println("Revoked", pos[0])
	default:
		return errors.New("usage: eze tokens revoke <id> | eze tokens revoke --user <email>")
	}
	return nil
}

// Logout revokes the CLI's token on the server and forgets it locally.
func Logout(args []string) error {
	c, err := newClient()
	if err != nil {
		return err
	}
	if err := c.do("DELETE", "/_q/api/tokens/current", nil, "", nil); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not revoke the token on the server:", err)
	}
	p, err := configPath()
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	fmt.Println("Logged out of", c.cfg.Server)
	return nil
}
