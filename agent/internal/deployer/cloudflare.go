package deployer

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"

	"github.com/ZenithHawking/deploymcp/agent/internal/cloudflare"
)

// CloudflareConfig enables automatic DNS (and tunnel routing) per domain.
type CloudflareConfig struct {
	Client     *cloudflare.Client
	Tunnel     bool   // route through a DeployMCP-owned Cloudflare Tunnel
	TunnelName string // e.g. deploymcp-myserver
	Service    string // what the tunnel forwards to (Caddy), e.g. http://127.0.0.1:8088
}

const connectorName = "deploymcp-cloudflared"

func (d *Deployer) cfEnabled() bool { return d.cfg.CF.Client.Enabled() }

// SetupCloudflare creates/attaches the tunnel, starts its connector and syncs
// all routes. Safe to call on every start.
func (d *Deployer) SetupCloudflare(ctx context.Context) error {
	if !d.cfEnabled() {
		return nil
	}
	cf := d.cfg.CF
	if cf.Tunnel {
		if d.cfg.AgentDomain == "" {
			return errors.New("cloudflare tunnel mode needs --domain for the agent")
		}
		zone, err := cf.Client.FindZone(ctx, d.cfg.AgentDomain)
		if err != nil {
			return err
		}
		t, err := cf.Client.EnsureTunnel(ctx, zone.AccountID, cf.TunnelName)
		if err != nil {
			return fmt.Errorf("create tunnel (token needs Account → Cloudflare Tunnel → Edit): %w", err)
		}
		tok, err := cf.Client.TunnelToken(ctx, t)
		if err != nil {
			return err
		}
		if err := d.ensureConnector(ctx, tok); err != nil {
			return err
		}
		d.cfMu.Lock()
		d.tunnel = t
		d.cfMu.Unlock()
	}
	return d.SyncCloudflare(ctx)
}

// ensureConnector keeps a cloudflared container running for our tunnel.
func (d *Deployer) ensureConnector(ctx context.Context, token string) error {
	out, err := d.docker.Run(ctx, "", nil, "inspect", "-f", "{{.State.Running}}", connectorName)
	if err == nil && strings.TrimSpace(out) == "true" {
		return nil
	}
	d.docker.Run(ctx, "", nil, "rm", "-f", connectorName)
	if out, err := d.docker.Run(ctx, "", nil, "run", "-d", "--name", connectorName,
		"--restart", "unless-stopped", "--network", "host",
		"--label", "deploymcp.role=tunnel",
		"cloudflare/cloudflared:latest", "tunnel", "--no-autoupdate", "run", "--token", token); err != nil {
		return fmt.Errorf("start cloudflared connector: %s", lastLines(out, 5))
	}
	return nil
}

// CheckDomain fails early if domain cannot be managed (no zone / foreign record).
func (d *Deployer) CheckDomain(ctx context.Context, domain string) error {
	if !d.cfEnabled() || domain == "" {
		return nil
	}
	zone, err := d.cfg.CF.Client.FindZone(ctx, domain)
	if err != nil {
		return err
	}
	if err := d.cfg.CF.Client.CheckRecord(ctx, zone.ID, domain); err != nil {
		if errors.Is(err, cloudflare.ErrConflict) {
			return fmt.Errorf("%v. DeployMCP never overwrites DNS records it did not create; use another subdomain", err)
		}
		return err
	}
	return nil
}

// SyncCloudflare ensures DNS records (and tunnel ingress) for the agent domain
// and every app domain.
func (d *Deployer) SyncCloudflare(ctx context.Context) error {
	if !d.cfEnabled() {
		return nil
	}
	d.cfMu.Lock()
	defer d.cfMu.Unlock()
	cf := d.cfg.CF
	apps, err := d.store.ListApps()
	if err != nil {
		return err
	}
	var hosts []string
	if d.cfg.AgentDomain != "" {
		hosts = append(hosts, d.cfg.AgentDomain)
	}
	for _, a := range apps {
		if a.Domain != "" && a.CurrentHostPort > 0 {
			hosts = append(hosts, a.Domain)
		}
	}

	var want func(host string) cloudflare.Record
	if cf.Tunnel {
		if d.tunnel == nil {
			return errors.New("cloudflare tunnel is not set up")
		}
		if err := cf.Client.SetIngress(ctx, d.tunnel, hosts, cf.Service); err != nil {
			return fmt.Errorf("update tunnel routes: %w", err)
		}
		target := d.tunnel.Target()
		want = func(h string) cloudflare.Record {
			return cloudflare.Record{Type: "CNAME", Name: h, Content: target, Proxied: true}
		}
	} else {
		ip := net.ParseIP(d.cfg.PublicHost)
		if ip == nil {
			return fmt.Errorf("--public-host must be this server's public IP for automatic DNS (got %q)", d.cfg.PublicHost)
		}
		typ := "A"
		if ip.To4() == nil {
			typ = "AAAA"
		}
		// DNS only: Caddy obtains the certificate itself.
		want = func(h string) cloudflare.Record {
			return cloudflare.Record{Type: typ, Name: h, Content: ip.String(), Proxied: false}
		}
	}

	var errs []error
	for _, h := range hosts {
		zone, err := cf.Client.FindZone(ctx, h)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := cf.Client.EnsureRecord(ctx, zone.ID, want(h)); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", h, err))
		}
	}
	return errors.Join(errs...)
}
