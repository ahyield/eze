// Package cli implements the eze command-line client: login, init, deploy, ls, rm and whoami.
package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Config struct {
	Server string `json:"server"`
	Token  string `json:"token"`
}

func configPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "eze", "config.json"), nil
}

// loadConfig reads the saved login; EZE_SERVER and EZE_TOKEN override it (useful in CI).
func loadConfig() (*Config, error) {
	c := &Config{}
	if p, err := configPath(); err == nil {
		if b, err := os.ReadFile(p); err == nil {
			if err := json.Unmarshal(b, c); err != nil {
				return nil, fmt.Errorf("%s: %w", p, err)
			}
		}
	}
	if v := os.Getenv("EZE_SERVER"); v != "" {
		c.Server = v
	}
	if v := os.Getenv("EZE_TOKEN"); v != "" {
		c.Token = v
	}
	if c.Server == "" || c.Token == "" {
		return nil, errors.New("not logged in: run `eze login <server-url>` first")
	}
	c.Server = strings.TrimRight(c.Server, "/")
	return c, nil
}

func saveConfig(c *Config) (string, error) {
	p, err := configPath()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return "", err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	if err := os.WriteFile(p, append(b, '\n'), 0o600); err != nil {
		return "", err
	}
	// WriteFile keeps an existing file's mode; the token must stay private either way.
	return p, os.Chmod(p, 0o600)
}

type client struct {
	cfg  *Config
	http *http.Client
}

func newClient() (*client, error) {
	cfg, err := loadConfig()
	if err != nil {
		return nil, err
	}
	return &client{cfg: cfg, http: &http.Client{
		Timeout: 5 * time.Minute,
		// An auth proxy that doesn't recognise the token redirects to its login page; surface that instead of following it.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}}, nil
}

// do sends an authenticated request and decodes a JSON response into out (if non-nil).
func (c *client) do(method, path string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequest(method, c.cfg.Server+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.Token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	res, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, err := io.ReadAll(io.LimitReader(res.Body, 10<<20))
	if err != nil {
		return err
	}
	if res.StatusCode >= 300 {
		var apiErr struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(data, &apiErr) == nil && apiErr.Error != "" {
			return fmt.Errorf("%s (HTTP %d)", apiErr.Error, res.StatusCode)
		}
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusFound {
			return fmt.Errorf("not authorized (HTTP %d): run `eze login` again, or check that your auth proxy lets Bearer requests to /_q/api/ through", res.StatusCode)
		}
		return fmt.Errorf("HTTP %d: %s", res.StatusCode, strings.TrimSpace(string(data)))
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unexpected response from server: %w", err)
		}
	}
	return nil
}

type site struct {
	Name       string    `json:"name"`
	Owner      string    `json:"owner"`
	DeployedBy string    `json:"deployed_by"`
	Files      int       `json:"files"`
	Bytes      int64     `json:"bytes"`
	UpdatedAt  time.Time `json:"updated_at"`
	URL        string    `json:"url"`
}

type user struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}
