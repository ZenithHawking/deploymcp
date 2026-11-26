package api

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ZenithHawking/deploymcp/agent/internal/auth"
	"github.com/ZenithHawking/deploymcp/agent/internal/caddy"
	"github.com/ZenithHawking/deploymcp/agent/internal/deployer"
	"github.com/ZenithHawking/deploymcp/agent/internal/store"
)

// fakeDocker simulates the docker CLI. When healthy is false, containers
// "crash" right after start.
type fakeDocker struct {
	mu       sync.Mutex
	healthy  bool
	servers  map[string]*http.Server
	commands []string
}

func (f *fakeDocker) Run(_ context.Context, _ string, w io.Writer, args ...string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commands = append(f.commands, strings.Join(args, " "))
	switch args[0] {
	case "build":
		if w != nil {
			io.WriteString(w, "#1 building\n#2 DONE 0.1s\n")
		}
		return "ok", nil
	case "run":
		var name, pub string
		for i, a := range args {
			if a == "--name" {
				name = args[i+1]
			}
			if a == "-p" {
				pub = args[i+1]
			}
		}
		if f.healthy {
			parts := strings.Split(pub, ":")
			ln, err := net.Listen("tcp", "127.0.0.1:"+parts[1])
			if err != nil {
				return err.Error(), err
			}
			srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "hello") })}
			go srv.Serve(ln)
			f.servers[name] = srv
		}
		return name + "\n", nil
	case "inspect":
		if f.healthy {
			return "running 0 0\n", nil
		}
		return "exited 1 0\n", nil
	case "logs":
		return "starting\nError: Cannot find module 'express'\n    at Object.<anonymous> (/app/index.js:1:1)\n", nil
	case "rm":
		name := args[len(args)-1]
		if s, ok := f.servers[name]; ok {
			s.Close()
			delete(f.servers, name)
		}
		return "", nil
	}
	return "", nil
}

type env struct {
	srv    *httptest.Server
	st     *store.Store
	docker *fakeDocker
	admin  string
}

func setup(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	fd := &fakeDocker{healthy: true, servers: map[string]*http.Server{}}
	dep := deployer.New(deployer.Config{DataDir: dir, HealthAttempts: 20, HealthInterval: 20 * time.Millisecond}, st, caddy.New("off", ""), fd)
	ts := httptest.NewServer((&Server{Store: st, Deployer: dep, Version: "test"}).Handler())
	t.Cleanup(ts.Close)
	t.Cleanup(func() {
		for _, s := range fd.servers {
			s.Close()
		}
	})
	return &env{srv: ts, st: st, docker: fd, admin: mkToken(t, st, "*", "write")}
}

func mkToken(t *testing.T, st *store.Store, apps, perm string) string {
	tok, hash, _ := auth.Generate()
	sc, err := auth.ParseScope(apps, perm)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateToken("t", hash, sc.AppsString(), sc.Perm); err != nil {
		t.Fatal(err)
	}
	return tok
}

func (e *env) do(t *testing.T, method, path, token, ctype string, body io.Reader) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(method, e.srv.URL+path, body)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func bundle(t *testing.T) []byte {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{"Dockerfile": "FROM node:20\nEXPOSE 8080\n", "index.js": "console.log(1)"}
	for n, c := range files {
		tw.WriteHeader(&tar.Header{Name: n, Mode: 0o644, Size: int64(len(c)), Typeflag: tar.TypeReg})
		tw.Write([]byte(c))
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func (e *env) deploy(t *testing.T, app, token string, extra map[string]string) (int, map[string]any) {
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("bundle", "bundle.tar.gz")
	fw.Write(bundle(t))
	for k, v := range extra {
		mw.WriteField(k, v)
	}
	mw.Close()
	return e.do(t, "POST", "/v1/apps/"+app+"/deploy", token, mw.FormDataContentType(), &body)
}

func TestHealthAndAuth(t *testing.T) {
	e := setup(t)
	if code, m := e.do(t, "GET", "/health", "", "", nil); code != 200 || m["status"] != "ok" {
		t.Fatalf("health: %d %v", code, m)
	}
	if code, _ := e.do(t, "GET", "/v1/apps", "", "", nil); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := e.do(t, "GET", "/v1/apps", "dmcp_live_"+strings.Repeat("0", 64), "", nil); code != 401 {
		t.Fatalf("unknown token: %d", code)
	}
	if code, _ := e.do(t, "GET", "/v1/apps", e.admin, "", nil); code != 200 {
		t.Fatalf("admin list: %d", code)
	}
}

func TestScopeEnforcement(t *testing.T) {
	e := setup(t)
	webOnly := mkToken(t, e.st, "web", "write")
	readOnly := mkToken(t, e.st, "*", "read")
	if code, m := e.deploy(t, "api", webOnly, nil); code != 403 {
		t.Fatalf("out of scope deploy: %d %v", code, m)
	}
	if code, m := e.deploy(t, "web", readOnly, nil); code != 403 {
		t.Fatalf("read-only deploy: %d %v", code, m)
	}
	if code, _ := e.deploy(t, "Bad_Name", e.admin, nil); code != 400 {
		t.Fatalf("bad name: %d", code)
	}
}

func TestDeployHealthyThenCrashKeepsOld(t *testing.T) {
	e := setup(t)
	code, m := e.deploy(t, "web", e.admin, nil)
	if code != 200 || m["ok"] != true || m["status"] != "healthy" {
		t.Fatalf("deploy1: %d %v", code, m)
	}
	if m["port"].(float64) != 8080 {
		t.Fatalf("EXPOSE port not detected: %v", m["port"])
	}
	rel1 := m["release_id"].(string)

	time.Sleep(1100 * time.Millisecond) // distinct release id
	e.docker.healthy = false
	code, m = e.deploy(t, "web", e.admin, nil)
	if code != 422 || m["ok"] != false || m["status"] != "failed" {
		t.Fatalf("deploy2 should fail: %d %v", code, m)
	}
	if m["active_release"] != rel1 {
		t.Fatalf("traffic should stay on %s, got %v", rel1, m["active_release"])
	}
	logs, _ := json.Marshal(m["logs"])
	if !strings.Contains(string(logs), "Cannot find module") {
		t.Fatalf("crash logs missing: %s", logs)
	}

	// list shows rel1 still current
	_, lm := e.do(t, "GET", "/v1/apps", e.admin, "", nil)
	apps := lm["apps"].([]any)
	if apps[0].(map[string]any)["current_release"] != rel1 {
		t.Fatalf("current release changed: %v", apps[0])
	}

	// build logs
	code, bl := e.do(t, "GET", "/v1/apps/web/logs?type=build", e.admin, "", nil)
	if code != 200 {
		t.Fatalf("build logs: %d %v", code, bl)
	}
}

func TestEnvNeverReturnsValuesAndRollback(t *testing.T) {
	e := setup(t)
	code, m := e.deploy(t, "web", e.admin, nil)
	if code != 200 {
		t.Fatalf("deploy: %d %v", code, m)
	}
	rel1 := m["release_id"].(string)
	body := strings.NewReader(`{"set":{"DATABASE_URL":"postgres://secret","STRIPE_KEY":"sk_live_x"}}`)
	code, m = e.do(t, "PUT", "/v1/apps/web/env", e.admin, "application/json", body)
	if code != 200 {
		t.Fatalf("env put: %d %v", code, m)
	}
	code, m = e.do(t, "GET", "/v1/apps/web/env", e.admin, "", nil)
	raw, _ := json.Marshal(m)
	if code != 200 || strings.Contains(string(raw), "secret") || strings.Contains(string(raw), "sk_live") {
		t.Fatalf("env values leaked or failed: %d %s", code, raw)
	}
	if !strings.Contains(string(raw), "DATABASE_URL") {
		t.Fatalf("keys missing: %s", raw)
	}

	time.Sleep(1100 * time.Millisecond)
	code, m = e.deploy(t, "web", e.admin, nil)
	if code != 200 {
		t.Fatalf("deploy2: %d %v", code, m)
	}
	code, m = e.do(t, "POST", "/v1/apps/web/rollback", e.admin, "application/json", strings.NewReader(`{}`))
	if code != 200 || m["release_id"] != rel1 {
		t.Fatalf("rollback: %d %v", code, m)
	}
	code, _ = e.do(t, "POST", "/v1/apps/web/rollback", e.admin, "application/json", strings.NewReader(`{"release_id":"`+rel1+`"}`))
	if code != 409 {
		t.Fatalf("rollback to live release should 409, got %d", code)
	}
}

func TestRejectedDeployLeavesNoApp(t *testing.T) {
	e := setup(t)
	if code, _ := e.deploy(t, "web", e.admin, map[string]string{"domain": "not a domain"}); code != 400 {
		t.Fatalf("expected 400, got %d", code)
	}
	if _, err := e.st.GetApp("web"); err == nil {
		t.Fatal("rejected deploy created an app row")
	}
}
