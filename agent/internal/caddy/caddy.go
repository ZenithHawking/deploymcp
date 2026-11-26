// Package caddy pushes reverse-proxy routes to Caddy's Admin API.
//
// The agent owns the whole Caddy config: on every change it rebuilds the JSON
// config from the app table and POSTs it to /load. Caddy obtains TLS
// certificates automatically for every host in a route matcher.
package caddy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"time"
)

// Route maps a domain to a local upstream (host:port).
type Route struct {
	Domain   string
	Upstream string
}

// Client talks to the Caddy Admin API.
type Client struct {
	AdminURL string // e.g. http://localhost:2019 ; empty disables Caddy
	Email    string // optional ACME email
	// HTTPOnly serves plain HTTP on :80 with automatic HTTPS disabled. Used
	// behind Cloudflare Tunnel, where Cloudflare terminates TLS.
	HTTPOnly bool
	HTTP     *http.Client
}

// New returns a client. adminURL "" or "off" disables it.
func New(adminURL, email string) *Client {
	if adminURL == "off" {
		adminURL = ""
	}
	return &Client{AdminURL: adminURL, Email: email, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Enabled reports whether Caddy integration is on.
func (c *Client) Enabled() bool { return c != nil && c.AdminURL != "" }

// BuildConfig renders a full Caddy JSON config for routes.
func (c *Client) BuildConfig(routes []Route) map[string]any {
	sort.Slice(routes, func(i, j int) bool { return routes[i].Domain < routes[j].Domain })
	var rs []any
	var hosts []string
	for _, r := range routes {
		if r.Domain == "" || r.Upstream == "" {
			continue
		}
		hosts = append(hosts, r.Domain)
		rs = append(rs, map[string]any{
			"match": []any{map[string]any{"host": []string{r.Domain}}},
			"handle": []any{map[string]any{
				"handler":   "reverse_proxy",
				"upstreams": []any{map[string]any{"dial": r.Upstream}},
			}},
			"terminal": true,
		})
	}
	if rs == nil {
		rs = []any{}
	}
	server := map[string]any{
		"listen": []string{":443", ":80"},
		"routes": rs,
	}
	if c.HTTPOnly {
		server["listen"] = []string{":80"}
		server["automatic_https"] = map[string]any{"disable": true}
	}
	cfg := map[string]any{
		"admin": map[string]any{"listen": hostOf(c.AdminURL)},
		"apps": map[string]any{
			"http": map[string]any{
				"servers": map[string]any{"deploymcp": server},
			},
		},
	}
	if c.Email != "" && len(hosts) > 0 && !c.HTTPOnly {
		cfg["apps"].(map[string]any)["tls"] = map[string]any{
			"automation": map[string]any{
				"policies": []any{map[string]any{
					"subjects": hosts,
					"issuers":  []any{map[string]any{"module": "acme", "email": c.Email}},
				}},
			},
		}
	}
	return cfg
}

// Sync replaces Caddy's config with routes.
func (c *Client) Sync(ctx context.Context, routes []Route) error {
	if !c.Enabled() {
		return nil
	}
	body, err := json.Marshal(c.BuildConfig(routes))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AdminURL+"/load", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("caddy admin api unreachable at %s: %w", c.AdminURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("caddy /load failed (%d): %s", resp.StatusCode, bytes.TrimSpace(msg))
	}
	return nil
}

func hostOf(u string) string {
	for _, p := range []string{"http://", "https://"} {
		if len(u) > len(p) && u[:len(p)] == p {
			return u[len(p):]
		}
	}
	return u
}
