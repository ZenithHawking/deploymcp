package deployer

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Runner executes docker CLI commands. It is an interface so tests can fake it.
type Runner interface {
	// Run executes `docker args...` with dir as working directory. Combined
	// output is streamed to w (if non-nil) and also returned.
	Run(ctx context.Context, dir string, w io.Writer, args ...string) (string, error)
}

// CLIRunner shells out to the docker binary.
type CLIRunner struct{ Bin string }

// Run implements Runner.
func (c CLIRunner) Run(ctx context.Context, dir string, w io.Writer, args ...string) (string, error) {
	bin := c.Bin
	if bin == "" {
		bin = "docker"
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DOCKER_BUILDKIT=1", "BUILDKIT_PROGRESS=plain")
	var buf bytes.Buffer
	var out io.Writer = &buf
	if w != nil {
		out = io.MultiWriter(&buf, w)
	}
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err != nil {
		return buf.String(), fmt.Errorf("docker %s: %w", firstArgs(args), err)
	}
	return buf.String(), nil
}

func firstArgs(args []string) string {
	if len(args) > 2 {
		return strings.Join(args[:2], " ")
	}
	return strings.Join(args, " ")
}

// freePort asks the kernel for an unused TCP port on loopback.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port, nil
}

var composeFiles = []string{"compose.yaml", "compose.yml", "docker-compose.yaml", "docker-compose.yml"}

// detectMode returns ("compose", file) or ("docker", "Dockerfile").
func detectMode(dir string) (mode, file string, err error) {
	for _, f := range composeFiles {
		if fileExists(filepath.Join(dir, f)) {
			return ModeCompose, f, nil
		}
	}
	if fileExists(filepath.Join(dir, "Dockerfile")) {
		return ModeDocker, "Dockerfile", nil
	}
	return "", "", fmt.Errorf("no Dockerfile or docker-compose.yml found in project root; add a Dockerfile that starts the app and EXPOSEs its port")
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

var exposeRe = regexp.MustCompile(`(?im)^\s*EXPOSE\s+(\d+)`)

// detectDockerfilePort returns the first EXPOSE port in the Dockerfile, or 0.
func detectDockerfilePort(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, "Dockerfile"))
	if err != nil {
		return 0
	}
	m := exposeRe.FindSubmatch(b)
	if m == nil {
		return 0
	}
	p, _ := strconv.Atoi(string(m[1]))
	return p
}

// ---------- compose ----------

type composeService struct {
	Build  json.RawMessage   `json:"build"`
	Image  string            `json:"image"`
	Ports  []composePort     `json:"ports"`
	Expose []json.RawMessage `json:"expose"`
}

type composePort struct {
	Target int `json:"target"`
}

type composeConfig struct {
	Services map[string]composeService `json:"services"`
}

// loadComposeConfig runs `docker compose config --format json`.
func loadComposeConfig(ctx context.Context, r Runner, dir, file, project, envFile string) (*composeConfig, error) {
	out, err := r.Run(ctx, dir, nil, "compose", "-p", project, "--env-file", envFile, "-f", file, "config", "--format", "json")
	if err != nil {
		return nil, fmt.Errorf("invalid compose file: %s", strings.TrimSpace(lastLines(out, 10)))
	}
	// Some compose versions print warnings before the JSON.
	if i := strings.Index(out, "{"); i > 0 {
		out = out[i:]
	}
	var cfg composeConfig
	if err := json.Unmarshal([]byte(out), &cfg); err != nil {
		return nil, fmt.Errorf("parse compose config: %w", err)
	}
	if len(cfg.Services) == 0 {
		return nil, fmt.Errorf("compose file defines no services")
	}
	return &cfg, nil
}

func exposeInt(raw json.RawMessage) int {
	s := strings.Trim(string(raw), `"`)
	s = strings.SplitN(s, "/", 2)[0]
	s = strings.SplitN(s, "-", 2)[0]
	p, _ := strconv.Atoi(s)
	return p
}

// pickPrimary chooses the service that receives HTTP traffic and its port.
func pickPrimary(cfg *composeConfig, wantPort int) (svc string, port int) {
	names := make([]string, 0, len(cfg.Services))
	for n := range cfg.Services {
		names = append(names, n)
	}
	sort.Strings(names)
	portsOf := func(s composeService) []int {
		var ps []int
		for _, p := range s.Ports {
			if p.Target > 0 {
				ps = append(ps, p.Target)
			}
		}
		for _, e := range s.Expose {
			if p := exposeInt(e); p > 0 {
				ps = append(ps, p)
			}
		}
		return ps
	}
	// 1) a service exposing the requested port
	if wantPort > 0 {
		for _, n := range names {
			for _, p := range portsOf(cfg.Services[n]) {
				if p == wantPort {
					return n, wantPort
				}
			}
		}
	}
	// 2) conventional names, 3) first service with build, 4) first service with ports, 5) first
	choose := func(n string) (string, int) {
		if wantPort > 0 {
			return n, wantPort
		}
		if ps := portsOf(cfg.Services[n]); len(ps) > 0 {
			return n, ps[0]
		}
		return n, 3000
	}
	for _, pref := range []string{"web", "app", "api", "server", "frontend"} {
		if _, ok := cfg.Services[pref]; ok {
			return choose(pref)
		}
	}
	for _, n := range names {
		if len(cfg.Services[n].Build) > 0 && string(cfg.Services[n].Build) != "null" {
			return choose(n)
		}
	}
	for _, n := range names {
		if len(portsOf(cfg.Services[n])) > 0 {
			return choose(n)
		}
	}
	return choose(names[0])
}

// writeComposeOverride writes a JSON (valid YAML) override file that pins
// built images to the release tag, publishes the primary port on loopback
// and injects the app's .env.
func writeComposeOverride(path string, cfg *composeConfig, app, release, primary string, port, hostPort int, bindAddr, envFile string) error {
	services := map[string]any{}
	for name, s := range cfg.Services {
		o := map[string]any{
			"labels": map[string]string{"deploymcp.app": app, "deploymcp.release": release},
		}
		if len(s.Build) > 0 && string(s.Build) != "null" {
			o["image"] = fmt.Sprintf("deploymcp/%s-%s:%s", app, name, release)
		}
		if name == primary {
			o["ports"] = []string{fmt.Sprintf("%s:%d:%d", bindAddr, hostPort, port)}
			o["env_file"] = []string{envFile}
		}
		services[name] = o
	}
	b, err := json.MarshalIndent(map[string]any{"services": services}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0o644)
}

type composePS struct {
	Service  string `json:"Service"`
	State    string `json:"State"`
	ExitCode int    `json:"ExitCode"`
}

// composeStates lists container states for a compose project.
func composeStates(ctx context.Context, r Runner, project string) ([]composePS, error) {
	out, err := r.Run(ctx, "", nil, "compose", "-p", project, "ps", "-a", "--format", "json")
	if err != nil {
		return nil, err
	}
	out = strings.TrimSpace(out)
	var list []composePS
	if strings.HasPrefix(out, "[") {
		err = json.Unmarshal([]byte(out), &list)
		return list, err
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var p composePS
		if json.Unmarshal([]byte(line), &p) == nil {
			list = append(list, p)
		}
	}
	return list, nil
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}
