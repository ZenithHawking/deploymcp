// Package cloudflare manages DNS records and a remotely-managed Cloudflare
// Tunnel so deploying to a new domain needs no manual Cloudflare work.
//
// Required API token permissions:
//
//	Zone    → Zone → Read
//	Zone    → DNS  → Edit
//	Account → Cloudflare Tunnel → Edit   (tunnel mode only)
package cloudflare

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ManagedComment marks DNS records created by DeployMCP. Records without it
// are never modified.
const ManagedComment = "managed by deploymcp"

// ErrConflict means a DNS record exists that DeployMCP does not own.
var ErrConflict = errors.New("dns record exists and is not managed by deploymcp")

// Client is a minimal Cloudflare API v4 client.
type Client struct {
	Token   string
	BaseURL string
	HTTP    *http.Client
}

// New returns a client; empty token means disabled.
func New(token string) *Client {
	return &Client{Token: strings.TrimSpace(token), BaseURL: "https://api.cloudflare.com/client/v4", HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Enabled reports whether a token is configured.
func (c *Client) Enabled() bool { return c != nil && c.Token != "" }

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare api: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("cloudflare api %s %s: HTTP %d", method, path, resp.StatusCode)
	}
	if !env.Success {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		return fmt.Errorf("cloudflare api %s: %s", strings.SplitN(path, "?", 2)[0], strings.Join(msgs, "; "))
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// Zone identifies a Cloudflare zone.
type Zone struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"-"`
}

// FindZone returns the zone that contains host (longest matching suffix).
func (c *Client) FindZone(ctx context.Context, host string) (*Zone, error) {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		name := strings.Join(labels[i:], ".")
		var zones []struct {
			ID      string `json:"id"`
			Name    string `json:"name"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		}
		if err := c.do(ctx, "GET", "/zones?name="+url.QueryEscape(name), nil, &zones); err != nil {
			return nil, err
		}
		if len(zones) > 0 {
			return &Zone{ID: zones[0].ID, Name: zones[0].Name, AccountID: zones[0].Account.ID}, nil
		}
	}
	return nil, fmt.Errorf("no Cloudflare zone for %q is accessible with this API token", host)
}

// Record is a DNS record.
type Record struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl"`
	Comment string `json:"comment"`
}

func (c *Client) records(ctx context.Context, zoneID, name string) ([]Record, error) {
	var rs []Record
	err := c.do(ctx, "GET", "/zones/"+zoneID+"/dns_records?per_page=100&name="+url.QueryEscape(name), nil, &rs)
	return rs, err
}

// CheckRecord returns ErrConflict if name has records DeployMCP does not own.
func (c *Client) CheckRecord(ctx context.Context, zoneID, name string) error {
	rs, err := c.records(ctx, zoneID, name)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.Comment != ManagedComment {
			return fmt.Errorf("%w: %s %s → %s", ErrConflict, r.Type, r.Name, r.Content)
		}
	}
	return nil
}

// EnsureRecord creates or updates a managed record. Unmanaged records with the
// same name are never touched (ErrConflict).
func (c *Client) EnsureRecord(ctx context.Context, zoneID string, want Record) error {
	want.Comment = ManagedComment
	if want.TTL == 0 {
		want.TTL = 1 // automatic
	}
	rs, err := c.records(ctx, zoneID, want.Name)
	if err != nil {
		return err
	}
	var mine *Record
	for i := range rs {
		if rs[i].Comment != ManagedComment {
			return fmt.Errorf("%w: %s %s → %s", ErrConflict, rs[i].Type, rs[i].Name, rs[i].Content)
		}
		mine = &rs[i]
	}
	if mine == nil {
		return c.do(ctx, "POST", "/zones/"+zoneID+"/dns_records", want, nil)
	}
	if mine.Type == want.Type && mine.Content == want.Content && mine.Proxied == want.Proxied {
		return nil
	}
	if mine.Type != want.Type {
		// Type changes (A ↔ CNAME) need delete + create.
		if err := c.do(ctx, "DELETE", "/zones/"+zoneID+"/dns_records/"+mine.ID, nil, nil); err != nil {
			return err
		}
		return c.do(ctx, "POST", "/zones/"+zoneID+"/dns_records", want, nil)
	}
	return c.do(ctx, "PUT", "/zones/"+zoneID+"/dns_records/"+mine.ID, want, nil)
}

// Tunnel is a remotely-managed Cloudflare Tunnel.
type Tunnel struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	AccountID string `json:"-"`
}

// Target is the CNAME target for routing a hostname through the tunnel.
func (t *Tunnel) Target() string { return t.ID + ".cfargotunnel.com" }

// EnsureTunnel finds or creates a remotely-managed tunnel named name.
func (c *Client) EnsureTunnel(ctx context.Context, accountID, name string) (*Tunnel, error) {
	var ts []Tunnel
	if err := c.do(ctx, "GET", "/accounts/"+accountID+"/cfd_tunnel?is_deleted=false&name="+url.QueryEscape(name), nil, &ts); err != nil {
		return nil, err
	}
	if len(ts) > 0 {
		ts[0].AccountID = accountID
		return &ts[0], nil
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	var t Tunnel
	body := map[string]any{"name": name, "config_src": "cloudflare", "tunnel_secret": base64.StdEncoding.EncodeToString(secret)}
	if err := c.do(ctx, "POST", "/accounts/"+accountID+"/cfd_tunnel", body, &t); err != nil {
		return nil, err
	}
	t.AccountID = accountID
	return &t, nil
}

// TunnelToken returns the connector token for `cloudflared tunnel run --token`.
func (c *Client) TunnelToken(ctx context.Context, t *Tunnel) (string, error) {
	var tok string
	err := c.do(ctx, "GET", "/accounts/"+t.AccountID+"/cfd_tunnel/"+t.ID+"/token", nil, &tok)
	return tok, err
}

// SetIngress replaces the tunnel's ingress: every hostname → service.
// Cloudflare pushes the new config to running connectors (no restart).
func (c *Client) SetIngress(ctx context.Context, t *Tunnel, hostnames []string, service string) error {
	var ingress []map[string]any
	for _, h := range hostnames {
		ingress = append(ingress, map[string]any{"hostname": h, "service": service})
	}
	ingress = append(ingress, map[string]any{"service": "http_status:404"})
	body := map[string]any{"config": map[string]any{"ingress": ingress}}
	return c.do(ctx, "PUT", "/accounts/"+t.AccountID+"/cfd_tunnel/"+t.ID+"/configurations", body, nil)
}
