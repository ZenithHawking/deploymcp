// Package deployer extracts bundles, builds Docker images / compose projects,
// healthchecks new releases and switches traffic or rolls back automatically.
package deployer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ZenithHawking/deploymcp/agent/internal/caddy"
	"github.com/ZenithHawking/deploymcp/agent/internal/logs"
	"github.com/ZenithHawking/deploymcp/agent/internal/store"
)

// Deployment modes.
const (
	ModeDocker  = "docker"
	ModeCompose = "compose"
)

// UserError is a client-side problem (maps to HTTP 4xx).
type UserError struct {
	Status int
	Msg    string
}

func (e *UserError) Error() string { return e.Msg }

func userErr(status int, format string, a ...any) error {
	return &UserError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// Config configures a Deployer.
type Config struct {
	DataDir        string
	PublicHost     string // host/IP used for URLs of apps without a domain
	HealthAttempts int
	HealthInterval time.Duration
	KeepReleases   int
	AgentDomain    string // domain routed to the agent itself (optional)
	AgentUpstream  string // e.g. 127.0.0.1:7070
}

// Deployer orchestrates deploys.
type Deployer struct {
	cfg     Config
	store   *store.Store
	caddy   *caddy.Client
	docker  Runner
	http    *http.Client
	locks   sync.Map
	caddyMu sync.Mutex
}

// New creates a Deployer.
func New(cfg Config, st *store.Store, cd *caddy.Client, r Runner) *Deployer {
	if cfg.HealthAttempts <= 0 {
		cfg.HealthAttempts = 15
	}
	if cfg.HealthInterval <= 0 {
		cfg.HealthInterval = 2 * time.Second
	}
	if cfg.KeepReleases <= 0 {
		cfg.KeepReleases = 10
	}
	if cfg.PublicHost == "" {
		cfg.PublicHost = "SERVER_IP"
	}
	return &Deployer{cfg: cfg, store: st, caddy: cd, docker: r, http: &http.Client{
		Timeout: 3 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}}
}

// Result is returned to the API / MCP client.
type Result struct {
	OK            bool     `json:"ok"`
	App           string   `json:"app"`
	ReleaseID     string   `json:"release_id,omitempty"`
	Status        string   `json:"status"`
	Mode          string   `json:"mode,omitempty"`
	URL           string   `json:"url,omitempty"`
	Port          int      `json:"port,omitempty"`
	Message       string   `json:"message"`
	ExitCode      *int     `json:"exit_code,omitempty"`
	Logs          []string `json:"logs,omitempty"`
	ActiveRelease string   `json:"active_release,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

// DeployOptions are per-deploy settings.
type DeployOptions struct {
	Domain          string
	Port            int
	HealthcheckPath string
}

func (d *Deployer) lock(app string) func() {
	m, _ := d.locks.LoadOrStore(app, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ---------- paths ----------

func (d *Deployer) appDir(app string) string { return filepath.Join(d.cfg.DataDir, "apps", app) }
func (d *Deployer) releaseDir(app, rel string) string {
	return filepath.Join(d.appDir(app), "releases", rel)
}
func (d *Deployer) buildLogPath(app, rel string) string {
	return filepath.Join(d.appDir(app), "logs", rel+".build.log")
}
func (d *Deployer) envPath(app string) string { return filepath.Join(d.appDir(app), ".env") }

func composeProject(app string) string { return "dmcp-" + app }

var domainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,63}$`)

func (d *Deployer) newReleaseID(app string) string {
	base := "rel_" + time.Now().UTC().Format("20060102_150405")
	id := base
	for i := 2; d.store.ReleaseExists(app, id); i++ {
		id = fmt.Sprintf("%s_%d", base, i)
	}
	return id
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (d *Deployer) appURL(a *store.App) string {
	if a.Domain != "" && d.caddy.Enabled() {
		return "https://" + a.Domain
	}
	if a.CurrentHostPort > 0 {
		return fmt.Sprintf("http://%s:%d", d.cfg.PublicHost, a.CurrentHostPort)
	}
	return ""
}

func (d *Deployer) ensureEnvFile(app string) error {
	p := d.envPath(app)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_RDONLY, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}

// ---------- deploy ----------

// Deploy extracts bundle as a new release, builds it, healthchecks it and
// either switches traffic or keeps the previous release live.
func (d *Deployer) Deploy(ctx context.Context, app string, bundle io.Reader, opts DeployOptions) (*Result, error) {
	unlock := d.lock(app)
	defer unlock()

	a, err := d.store.EnsureApp(app)
	if err != nil {
		return nil, err
	}

	domain := strings.ToLower(strings.TrimSpace(opts.Domain))
	if domain == "" {
		domain = a.Domain
	}
	if domain != "" {
		if !domainRe.MatchString(domain) {
			return nil, userErr(400, "invalid domain %q", domain)
		}
		if domain == d.cfg.AgentDomain {
			return nil, userErr(409, "domain %q is reserved for the DeployMCP agent itself", domain)
		}
		owner, err := d.store.DomainOwner(domain)
		if err != nil {
			return nil, err
		}
		if owner != "" && owner != app {
			return nil, userErr(409, "domain %q is already used by app %q", domain, owner)
		}
	}
	hc := strings.TrimSpace(opts.HealthcheckPath)
	if hc == "" {
		hc = a.HealthcheckPath
	}
	if hc == "" {
		hc = "/"
	}
	if !strings.HasPrefix(hc, "/") {
		return nil, userErr(400, "healthcheck_path must start with /")
	}
	if opts.Port < 0 || opts.Port > 65535 {
		return nil, userErr(400, "invalid port %d", opts.Port)
	}

	rel := d.newReleaseID(app)
	dir := d.releaseDir(app, rel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	skipped, err := ExtractTarGz(bundle, dir)
	if err != nil {
		os.RemoveAll(dir)
		if errors.Is(err, ErrUnsafePath) {
			return nil, userErr(400, "%v", err)
		}
		return nil, userErr(400, "extract bundle: %v", err)
	}
	var warnings []string
	if len(skipped) > 0 {
		warnings = append(warnings, fmt.Sprintf("skipped %d symlink/special entries (e.g. %s)", len(skipped), skipped[0]))
	}
	if err := d.ensureEnvFile(app); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(d.buildLogPath(app, rel)), 0o755); err != nil {
		return nil, err
	}
	logf, err := os.Create(d.buildLogPath(app, rel))
	if err != nil {
		return nil, err
	}
	defer logf.Close()

	r := &store.Release{ID: rel, App: app, Status: store.StatusBuilding, Port: opts.Port}
	mode, file, derr := detectMode(dir)
	r.Mode = mode
	if err := d.store.CreateRelease(r); err != nil {
		return nil, err
	}
	fail := func(status, msg string, out string) (*Result, error) {
		r.Status, r.Error = status, msg
		_ = d.store.UpdateRelease(r)
		fmt.Fprintf(logf, "\n[deploymcp] %s\n", msg)
		res := d.failResult(a, r, msg, out)
		res.Warnings = append(warnings, res.Warnings...)
		return res, nil
	}
	if derr != nil {
		return fail(store.StatusBuildFailed, derr.Error(), "")
	}

	a.Domain = domain
	a.HealthcheckPath = hc

	switch mode {
	case ModeDocker:
		if r.Port == 0 {
			r.Port = detectDockerfilePort(dir)
		}
		if r.Port == 0 {
			r.Port = 3000
			warnings = append(warnings, "no port given and no EXPOSE in Dockerfile; assuming 3000")
		}
		image := fmt.Sprintf("deploymcp/%s:%s", app, rel)
		fmt.Fprintf(logf, "[deploymcp] docker build -t %s .\n", image)
		out, err := d.docker.Run(ctx, dir, logf, "build", "-t", image, "--label", "deploymcp.app="+app, ".")
		if err != nil {
			return fail(store.StatusBuildFailed, "docker build failed", out)
		}
		r.Image = image
		_ = d.store.UpdateRelease(r)
		res := d.activateDocker(ctx, a, r, nil)
		res.Warnings = append(warnings, res.Warnings...)
		return res, nil

	case ModeCompose:
		cp, err := d.prepareCompose(ctx, a, r, dir, file)
		if err != nil {
			return fail(store.StatusBuildFailed, err.Error(), "")
		}
		fmt.Fprintf(logf, "[deploymcp] docker compose build (service %q, port %d)\n", cp.primary, r.Port)
		out, err := d.docker.Run(ctx, dir, logf, cp.args("build")...)
		if err != nil {
			return fail(store.StatusBuildFailed, "docker compose build failed", out)
		}
		r.Image = "compose:" + cp.primary
		_ = d.store.UpdateRelease(r)
		res := d.activateCompose(ctx, a, r, cp, nil)
		res.Warnings = append(warnings, res.Warnings...)
		return res, nil
	}
	return fail(store.StatusBuildFailed, "unknown mode", "")
}

func (d *Deployer) failResult(a *store.App, r *store.Release, msg, out string) *Result {
	res := &Result{
		OK: false, App: a.Name, ReleaseID: r.ID, Status: r.Status, Mode: r.Mode,
		Port: r.Port, Message: msg, ExitCode: r.ExitCode, ActiveRelease: a.CurrentRelease,
	}
	if out != "" {
		f := logs.Filter(out, logs.Options{Lines: 50, Compact: true})
		res.Logs = f.Lines
	}
	if a.CurrentRelease != "" {
		res.Message += fmt.Sprintf(". Traffic stays on previous healthy release %s (%s)", a.CurrentRelease, d.appURL(a))
	} else {
		res.Message += ". No previous release is running"
	}
	return res
}

// ---------- docker mode ----------

// activateDocker starts a fresh container for release r, healthchecks it and
// switches traffic. On failure the new container is removed and the previous
// instance keeps serving. onFail runs before returning a failure.
func (d *Deployer) activateDocker(ctx context.Context, a *store.App, r *store.Release, onFail func()) *Result {
	hp, err := freePort()
	if err != nil {
		return d.markFailed(a, r, "no free port: "+err.Error(), "", onFail)
	}
	bind := "127.0.0.1"
	if a.Domain == "" || !d.caddy.Enabled() {
		bind = "0.0.0.0"
	}
	name := fmt.Sprintf("dmcp-%s-%s-%s", a.Name, strings.TrimPrefix(r.ID, "rel_"), randHex(2))
	args := []string{"run", "-d", "--name", name, "--restart", "unless-stopped",
		"-p", fmt.Sprintf("%s:%d:%d", bind, hp, r.Port),
		"--env-file", d.envPath(a.Name),
		"--label", "deploymcp.app=" + a.Name, "--label", "deploymcp.release=" + r.ID,
		"--log-opt", "max-size=10m", "--log-opt", "max-file=3",
		r.Image}
	if out, err := d.docker.Run(ctx, "", nil, args...); err != nil {
		d.docker.Run(context.Background(), "", nil, "rm", "-f", name)
		return d.markFailed(a, r, "container failed to start", out, onFail)
	}

	ok, exit, reason := d.waitHealthy(ctx, hp, a.HealthcheckPath, func() (bool, *int) {
		return d.dockerCrashed(ctx, name)
	})
	if !ok {
		out, _ := d.docker.Run(context.Background(), "", nil, "logs", "--tail", "50", name)
		d.docker.Run(context.Background(), "", nil, "rm", "-f", name)
		r.ExitCode = exit
		return d.markFailed(a, r, reason, out, onFail)
	}
	return d.promote(ctx, a, r, name, hp, ModeDocker)
}

func (d *Deployer) dockerCrashed(ctx context.Context, name string) (bool, *int) {
	out, err := d.docker.Run(ctx, "", nil, "inspect", "-f", "{{.State.Status}} {{.State.ExitCode}} {{.RestartCount}}", name)
	if err != nil {
		return true, nil
	}
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) < 3 {
		return false, nil
	}
	code, _ := strconv.Atoi(f[1])
	restarts, _ := strconv.Atoi(f[2])
	if f[0] == "exited" || f[0] == "dead" || f[0] == "restarting" || restarts > 0 {
		return true, &code
	}
	return false, nil
}

// ---------- compose mode ----------

type composePlan struct {
	project, dir, file, override, envFile, primary string
	hostPort                                       int
}

func (c *composePlan) args(cmd ...string) []string {
	base := []string{"compose", "-p", c.project}
	if c.envFile != "" {
		base = append(base, "--env-file", c.envFile)
	}
	base = append(base, "-f", c.file, "-f", c.override)
	return append(base, cmd...)
}

// existingPlan builds a plan for an already-prepared release directory.
func (d *Deployer) existingPlan(app, project, rel string) (*composePlan, error) {
	dir := d.releaseDir(app, rel)
	_, file, err := detectMode(dir)
	if err != nil {
		return nil, err
	}
	return &composePlan{project: project, dir: dir, file: file,
		override: filepath.Join(dir, ".deploymcp.override.json"), envFile: d.envPath(app)}, nil
}

// prepareCompose resolves the primary service/port and writes the override.
// Compose mode runs in-place under a stable project name per app so named
// volumes (databases) persist across releases.
func (d *Deployer) prepareCompose(ctx context.Context, a *store.App, r *store.Release, dir, file string) (*composePlan, error) {
	project := composeProject(a.Name)
	cfg, err := loadComposeConfig(ctx, d.docker, dir, file, project, d.envPath(a.Name))
	if err != nil {
		return nil, err
	}
	primary, port := pickPrimary(cfg, r.Port)
	r.Port = port
	hp := 0
	if a.Mode == ModeCompose && a.CurrentHostPort > 0 {
		hp = a.CurrentHostPort
	} else if hp, err = freePort(); err != nil {
		return nil, err
	}
	bind := "127.0.0.1"
	if a.Domain == "" || !d.caddy.Enabled() {
		bind = "0.0.0.0"
	}
	override := filepath.Join(dir, ".deploymcp.override.json")
	if err := writeComposeOverride(override, cfg, a.Name, r.ID, primary, port, hp, bind, d.envPath(a.Name)); err != nil {
		return nil, err
	}
	return &composePlan{project: project, dir: dir, file: file, override: override, envFile: d.envPath(a.Name), primary: primary, hostPort: hp}, nil
}

func (d *Deployer) activateCompose(ctx context.Context, a *store.App, r *store.Release, cp *composePlan, onFail func()) *Result {
	prevMode, prevRel := a.Mode, a.CurrentRelease

	restorePrev := func() {
		if onFail != nil {
			onFail()
			onFail = nil
		}
		bg := context.Background()
		if prevMode == ModeCompose && prevRel != "" {
			if pp, err := d.existingPlan(a.Name, cp.project, prevRel); err == nil {
				d.docker.Run(bg, pp.dir, nil, pp.args("up", "-d", "--remove-orphans")...)
				return
			}
		}
		// previous release was docker-mode (still running) or none: tear down.
		d.docker.Run(bg, cp.dir, nil, cp.args("down", "--remove-orphans")...)
	}

	if out, err := d.docker.Run(ctx, cp.dir, nil, cp.args("up", "-d", "--remove-orphans")...); err != nil {
		logsOut, _ := d.docker.Run(context.Background(), cp.dir, nil, cp.args("logs", "--tail", "50", "--no-color")...)
		restorePrev()
		return d.markFailed(a, r, "docker compose up failed", out+"\n"+logsOut, nil)
	}
	ok, exit, reason := d.waitHealthy(ctx, cp.hostPort, a.HealthcheckPath, func() (bool, *int) {
		states, err := composeStates(ctx, d.docker, cp.project)
		if err != nil {
			return false, nil
		}
		for _, s := range states {
			if s.State == "restarting" || ((s.State == "exited" || s.State == "dead") && s.ExitCode != 0) {
				c := s.ExitCode
				return true, &c
			}
		}
		return false, nil
	})
	if !ok {
		out, _ := d.docker.Run(context.Background(), cp.dir, nil, cp.args("logs", "--tail", "50", "--no-color")...)
		restorePrev()
		r.ExitCode = exit
		return d.markFailed(a, r, reason, out, nil)
	}
	return d.promote(ctx, a, r, cp.project, cp.hostPort, ModeCompose)
}

// ---------- shared ----------

func (d *Deployer) markFailed(a *store.App, r *store.Release, msg, out string, onFail func()) *Result {
	if onFail != nil {
		onFail()
	}
	if r.Status == store.StatusBuilding {
		r.Status = store.StatusFailed
		r.Error = msg
		_ = d.store.UpdateRelease(r)
	}
	// a was mutated in memory (domain/hc) but not persisted; reload for result.
	if fresh, err := d.store.GetApp(a.Name); err == nil {
		*a = *fresh
	}
	res := d.failResult(a, r, msg, out)
	res.Status = store.StatusFailed
	res.Message = "Release failed: " + res.Message + ". Read logs, fix the code and deploy again"
	return res
}

// waitHealthy polls http://127.0.0.1:hp<path> until it answers < 500.
func (d *Deployer) waitHealthy(ctx context.Context, hp int, path string, crashed func() (bool, *int)) (bool, *int, string) {
	url := fmt.Sprintf("http://127.0.0.1:%d%s", hp, path)
	var lastErr string
	for i := 0; i < d.cfg.HealthAttempts; i++ {
		select {
		case <-ctx.Done():
			return false, nil, "deploy cancelled"
		case <-time.After(d.cfg.HealthInterval):
		}
		if c, exit := crashed(); c {
			msg := "container crashed during healthcheck"
			if exit != nil {
				msg += fmt.Sprintf(" (exit code %d)", *exit)
			}
			return false, exit, msg
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := d.http.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode < 500 {
			return true, nil, ""
		}
		lastErr = fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	secs := int(d.cfg.HealthInterval.Seconds()) * d.cfg.HealthAttempts
	return false, nil, fmt.Sprintf("healthcheck %s did not pass within %ds (last: %s). Check the app listens on 0.0.0.0 and the configured port", path, secs, lastErr)
}

// promote switches traffic to the new instance and retires the old one.
func (d *Deployer) promote(ctx context.Context, a *store.App, r *store.Release, instance string, hp int, mode string) *Result {
	prevInstance, prevMode, prevRel := a.CurrentInstance, a.Mode, a.CurrentRelease
	a.Mode = mode
	a.Port = r.Port
	a.CurrentRelease = r.ID
	a.CurrentInstance = instance
	a.CurrentHostPort = hp
	res := &Result{OK: true, App: a.Name, ReleaseID: r.ID, Mode: mode, Port: r.Port, Status: store.StatusHealthy}
	if err := d.store.SaveApp(a); err != nil {
		res.Warnings = append(res.Warnings, "save app state: "+err.Error())
	}
	r.Status = store.StatusHealthy
	r.Error = ""
	r.ExitCode = nil
	_ = d.store.UpdateRelease(r)

	caddyOK := true
	if err := d.SyncCaddy(ctx); err != nil {
		caddyOK = false
		res.Warnings = append(res.Warnings, "routing update failed, previous instance kept running: "+err.Error())
	}
	if caddyOK && prevInstance != "" && prevInstance != instance {
		bg := context.Background()
		if prevMode == ModeCompose {
			if pp, err := d.existingPlan(a.Name, prevInstance, prevRel); err == nil {
				d.docker.Run(bg, pp.dir, nil, pp.args("down", "--remove-orphans")...)
			} else {
				d.docker.Run(bg, "", nil, "compose", "-p", prevInstance, "down", "--remove-orphans")
			}
		} else {
			d.docker.Run(bg, "", nil, "rm", "-f", prevInstance)
		}
	}
	if prevRel != "" && prevRel != r.ID {
		if pr, err := d.store.GetRelease(a.Name, prevRel); err == nil && pr.Status == store.StatusHealthy {
			pr.Status = store.StatusSuperseded
			_ = d.store.UpdateRelease(pr)
		}
	}
	res.URL = d.appURL(a)
	res.ActiveRelease = r.ID
	res.Message = fmt.Sprintf("Release %s is healthy and live at %s", r.ID, res.URL)
	if a.Domain == "" {
		res.Message += " (no domain set: pass `domain` to get HTTPS)"
	}
	d.prune(a)
	return res
}

// SyncCaddy pushes the full routing table to Caddy.
func (d *Deployer) SyncCaddy(ctx context.Context) error {
	if !d.caddy.Enabled() {
		return nil
	}
	d.caddyMu.Lock()
	defer d.caddyMu.Unlock()
	apps, err := d.store.ListApps()
	if err != nil {
		return err
	}
	var routes []caddy.Route
	if d.cfg.AgentDomain != "" && d.cfg.AgentUpstream != "" {
		routes = append(routes, caddy.Route{Domain: d.cfg.AgentDomain, Upstream: d.cfg.AgentUpstream})
	}
	for _, a := range apps {
		if a.Domain != "" && a.CurrentHostPort > 0 {
			routes = append(routes, caddy.Route{Domain: a.Domain, Upstream: fmt.Sprintf("127.0.0.1:%d", a.CurrentHostPort)})
		}
	}
	return d.caddy.Sync(ctx, routes)
}

// prune removes old releases beyond KeepReleases (never the current one).
func (d *Deployer) prune(a *store.App) {
	rels, err := d.store.ListReleases(a.Name, 0)
	if err != nil || len(rels) <= d.cfg.KeepReleases {
		return
	}
	bg := context.Background()
	for _, r := range rels[d.cfg.KeepReleases:] {
		if r.ID == a.CurrentRelease {
			continue
		}
		os.RemoveAll(d.releaseDir(a.Name, r.ID))
		os.Remove(d.buildLogPath(a.Name, r.ID))
		if r.Mode == ModeDocker && r.Image != "" {
			d.docker.Run(bg, "", nil, "rmi", r.Image)
		} else if r.Mode == ModeCompose {
			if out, err := d.docker.Run(bg, "", nil, "images", "-q", "--filter", fmt.Sprintf("reference=deploymcp/%s-*:%s", a.Name, r.ID)); err == nil {
				for _, id := range strings.Fields(out) {
					d.docker.Run(bg, "", nil, "rmi", id)
				}
			}
		}
		_ = d.store.DeleteRelease(a.Name, r.ID)
	}
}

// ---------- rollback ----------

// Rollback re-activates releaseID (or the newest previously-healthy release).
func (d *Deployer) Rollback(ctx context.Context, app, releaseID string) (*Result, error) {
	unlock := d.lock(app)
	defer unlock()
	a, err := d.store.GetApp(app)
	if errors.Is(err, store.ErrNotFound) {
		return nil, userErr(404, "app %q not found", app)
	}
	if err != nil {
		return nil, err
	}
	var target *store.Release
	if releaseID == "" {
		target, err = d.store.LatestRollbackTarget(app, a.CurrentRelease)
		if errors.Is(err, store.ErrNotFound) {
			return nil, userErr(404, "no previous healthy release to roll back to")
		}
	} else {
		target, err = d.store.GetRelease(app, releaseID)
		if errors.Is(err, store.ErrNotFound) {
			return nil, userErr(404, "release %q not found", releaseID)
		}
	}
	if err != nil {
		return nil, err
	}
	if target.ID == a.CurrentRelease {
		return nil, userErr(409, "release %s is already live", target.ID)
	}
	if target.Status == store.StatusBuildFailed || target.Status == store.StatusBuilding || target.Image == "" {
		return nil, userErr(409, "release %s has no runnable image (status %s)", target.ID, target.Status)
	}
	// Re-run healthcheck on the target; its stored status changes only on success.
	orig := *target
	target.Status = store.StatusBuilding
	var res *Result
	switch target.Mode {
	case ModeDocker:
		res = d.activateDocker(ctx, a, target, nil)
	case ModeCompose:
		dir := d.releaseDir(app, target.ID)
		_, file, derr := detectMode(dir)
		if derr != nil {
			return nil, userErr(409, "release files for %s are gone", target.ID)
		}
		cp, perr := d.prepareCompose(ctx, a, target, dir, file)
		if perr != nil {
			return nil, perr
		}
		res = d.activateCompose(ctx, a, target, cp, nil)
	default:
		return nil, userErr(409, "release %s has unknown mode", target.ID)
	}
	if !res.OK {
		// Keep the release's historical status (it was healthy once).
		orig.ExitCode = target.ExitCode
		_ = d.store.UpdateRelease(&orig)
		res.Status = "rollback_failed"
	} else {
		res.Message = fmt.Sprintf("Rolled back to %s; live at %s", target.ID, res.URL)
	}
	return res, nil
}

// ---------- env ----------

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func readEnv(path string) (map[string]string, []byte, error) {
	b, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	m := map[string]string{}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if ok {
			m[strings.TrimSpace(k)] = v
		}
	}
	return m, b, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// EnvKeys lists env variable names (never values).
func (d *Deployer) EnvKeys(app string) ([]string, error) {
	if _, err := d.store.GetApp(app); errors.Is(err, store.ErrNotFound) {
		return nil, userErr(404, "app %q not found", app)
	}
	m, _, err := readEnv(d.envPath(app))
	if err != nil {
		return nil, err
	}
	return sortedKeys(m), nil
}

// EnvResult is returned by UpdateEnv.
type EnvResult struct {
	Keys    []string `json:"keys"`
	Applied *Result  `json:"applied,omitempty"`
	Message string   `json:"message"`
}

// UpdateEnv sets/deletes variables and recreates the running app.
// If the app fails with the new env, the old file is restored.
func (d *Deployer) UpdateEnv(ctx context.Context, app string, set map[string]string, del []string) (*EnvResult, error) {
	for k, v := range set {
		if !envKeyRe.MatchString(k) {
			return nil, userErr(400, "invalid env key %q", k)
		}
		if strings.ContainsAny(v, "\n\r\x00") {
			return nil, userErr(400, "value for %q must be a single line", k)
		}
	}
	unlock := d.lock(app)
	defer unlock()
	a, err := d.store.EnsureApp(app)
	if err != nil {
		return nil, err
	}
	if err := d.ensureEnvFile(app); err != nil {
		return nil, err
	}
	path := d.envPath(app)
	m, backup, err := readEnv(path)
	if err != nil {
		return nil, err
	}
	for k, v := range set {
		m[k] = v
	}
	for _, k := range del {
		delete(m, k)
	}
	var sb strings.Builder
	for _, k := range sortedKeys(m) {
		sb.WriteString(k + "=" + m[k] + "\n")
	}
	if err := writeFileAtomic(path, []byte(sb.String()), 0o600); err != nil {
		return nil, err
	}
	out := &EnvResult{Keys: sortedKeys(m)}
	if a.CurrentRelease == "" {
		out.Message = "Saved. Will be applied on next deploy"
		return out, nil
	}
	r, err := d.store.GetRelease(app, a.CurrentRelease)
	if err != nil {
		return nil, err
	}
	restore := func() { _ = writeFileAtomic(path, backup, 0o600) }
	var res *Result
	switch a.Mode {
	case ModeDocker:
		res = d.activateDocker(ctx, a, r, restore)
	case ModeCompose:
		dir := d.releaseDir(app, r.ID)
		_, file, derr := detectMode(dir)
		if derr != nil {
			restore()
			return nil, derr
		}
		cp, perr := d.prepareCompose(ctx, a, r, dir, file)
		if perr != nil {
			restore()
			return nil, perr
		}
		// Same release is "previous" too, so on failure it is re-upped after restore().
		res = d.activateCompose(ctx, a, r, cp, restore)
	}
	out.Applied = res
	if res != nil && res.OK {
		out.Message = "Saved and applied; app recreated and healthy"
	} else {
		out.Keys = func() []string { m, _, _ := readEnv(path); return sortedKeys(m) }()
		out.Message = "App failed to start with the new env; changes reverted, previous instance still live"
	}
	return out, nil
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------- logs ----------

// Logs returns filtered build or runtime logs.
func (d *Deployer) Logs(ctx context.Context, app, typ, releaseID string, o logs.Options) (*logs.Result, string, error) {
	a, err := d.store.GetApp(app)
	if errors.Is(err, store.ErrNotFound) {
		return nil, "", userErr(404, "app %q not found", app)
	}
	if err != nil {
		return nil, "", err
	}
	if typ == "build" {
		rel := releaseID
		if rel == "" {
			rels, err := d.store.ListReleases(app, 1)
			if err != nil {
				return nil, "", err
			}
			if len(rels) == 0 {
				return nil, "", userErr(404, "no releases yet")
			}
			rel = rels[0].ID
		}
		b, err := os.ReadFile(d.buildLogPath(app, rel))
		if err != nil {
			return nil, rel, userErr(404, "no build log for release %s", rel)
		}
		res := logs.Filter(string(b), o)
		return &res, rel, nil
	}
	if a.CurrentInstance == "" {
		return nil, "", userErr(404, "app %q has no running release; use type=build to see the last build", app)
	}
	tail := o.Lines * 3
	if strings.EqualFold(o.Level, "error") {
		tail = o.Lines * 10
	}
	if tail > 5000 {
		tail = 5000
	}
	var out string
	if a.Mode == ModeCompose {
		out, err = d.docker.Run(ctx, "", nil, "compose", "-p", a.CurrentInstance, "logs", "--tail", strconv.Itoa(tail), "--no-color")
	} else {
		out, err = d.docker.Run(ctx, "", nil, "logs", "--tail", strconv.Itoa(tail), a.CurrentInstance)
	}
	if err != nil && out == "" {
		return nil, "", err
	}
	res := logs.Filter(out, o)
	return &res, a.CurrentRelease, nil
}

// ---------- list ----------

// AppInfo is an app plus live status and recent releases.
type AppInfo struct {
	*store.App
	URL      string           `json:"url,omitempty"`
	State    string           `json:"state"`
	Releases []*store.Release `json:"recent_releases"`
}

// ListApps returns info for apps accepted by allow.
func (d *Deployer) ListApps(ctx context.Context, allow func(string) bool) ([]AppInfo, error) {
	apps, err := d.store.ListApps()
	if err != nil {
		return nil, err
	}
	out := []AppInfo{}
	for _, a := range apps {
		if !allow(a.Name) {
			continue
		}
		info := AppInfo{App: a, URL: d.appURL(a), State: d.state(ctx, a)}
		info.Releases, _ = d.store.ListReleases(a.Name, 5)
		if info.Releases == nil {
			info.Releases = []*store.Release{}
		}
		out = append(out, info)
	}
	return out, nil
}

func (d *Deployer) state(ctx context.Context, a *store.App) string {
	if a.CurrentInstance == "" {
		return "not_running"
	}
	if a.Mode == ModeCompose {
		states, err := composeStates(ctx, d.docker, a.CurrentInstance)
		if err != nil || len(states) == 0 {
			return "unknown"
		}
		running := 0
		for _, s := range states {
			if s.State == "running" {
				running++
			}
		}
		if running == len(states) {
			return "running"
		}
		return fmt.Sprintf("degraded (%d/%d running)", running, len(states))
	}
	out, err := d.docker.Run(ctx, "", nil, "inspect", "-f", "{{.State.Status}}", a.CurrentInstance)
	if err != nil {
		return "missing"
	}
	return strings.TrimSpace(out)
}
