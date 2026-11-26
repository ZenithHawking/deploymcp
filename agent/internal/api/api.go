// Package api exposes the agent's HTTPS REST API with scoped-token auth.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ZenithHawking/deploymcp/agent/internal/auth"
	"github.com/ZenithHawking/deploymcp/agent/internal/deployer"
	"github.com/ZenithHawking/deploymcp/agent/internal/logs"
	"github.com/ZenithHawking/deploymcp/agent/internal/store"
)

// MaxBundleBytes caps the upload size (compressed).
const MaxBundleBytes = 100 << 20

// DeployTimeout bounds one deploy (build + healthcheck).
const DeployTimeout = 25 * time.Minute

// Server holds dependencies for HTTP handlers.
type Server struct {
	Store    *store.Store
	Deployer *deployer.Deployer
	Version  string
}

type ctxKey struct{}

type principal struct {
	tokenID int64
	scope   auth.Scope
}

// Handler returns the routed HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.health)
	mux.Handle("GET /v1/apps", s.authed(s.listApps))
	mux.Handle("POST /v1/apps/{name}/deploy", s.authed(s.appScoped(true, s.deploy)))
	mux.Handle("GET /v1/apps/{name}/logs", s.authed(s.appScoped(false, s.logs)))
	mux.Handle("POST /v1/apps/{name}/rollback", s.authed(s.appScoped(true, s.rollback)))
	mux.Handle("GET /v1/apps/{name}/env", s.authed(s.appScoped(false, s.envKeys)))
	mux.Handle("PUT /v1/apps/{name}/env", s.authed(s.appScoped(true, s.envUpdate)))
	return logRequests(mux)
}

func logRequests(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		h.ServeHTTP(sw, r)
		log.Printf("%s %s %d %s", r.Method, r.URL.Path, sw.status, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(c int) { w.status = c; w.ResponseWriter.WriteHeader(c) }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var ue *deployer.UserError
	if errors.As(err, &ue) {
		writeJSON(w, ue.Status, map[string]string{"error": ue.Msg})
		return
	}
	log.Printf("internal error: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
}

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok", "version": s.Version})
}

// authed validates the bearer token and stores the principal in the context.
func (s *Server) authed(next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := r.Header.Get("Authorization")
		tok, ok := strings.CutPrefix(h, "Bearer ")
		tok = strings.TrimSpace(tok)
		if !ok || !auth.WellFormed(tok) {
			writeJSON(w, 401, map[string]string{"error": "missing or malformed bearer token"})
			return
		}
		t, err := s.Store.TokenByHash(auth.Hash(tok))
		if err != nil {
			// Constant message; do not reveal whether the token existed.
			writeJSON(w, 401, map[string]string{"error": "invalid or revoked token"})
			return
		}
		scope, err := auth.ParseScope(t.Apps, t.Perm)
		if err != nil {
			writeJSON(w, 401, map[string]string{"error": "token has an invalid scope"})
			return
		}
		ctx := context.WithValue(r.Context(), ctxKey{}, &principal{tokenID: t.ID, scope: scope})
		next(w, r.WithContext(ctx))
	})
}

func who(r *http.Request) *principal { return r.Context().Value(ctxKey{}).(*principal) }

// appScoped enforces app name validity, app scope, and write permission.
func (s *Server) appScoped(write bool, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if !auth.ValidAppName(name) {
			writeJSON(w, 400, map[string]string{"error": "invalid app name: use lowercase letters, digits and dashes (max 50)"})
			return
		}
		p := who(r)
		if !p.scope.AllowsApp(name) {
			writeJSON(w, 403, map[string]string{"error": fmt.Sprintf("token is not allowed to access app %q", name)})
			return
		}
		if write && !p.scope.AllowsWrite() {
			writeJSON(w, 403, map[string]string{"error": "token is read-only"})
			return
		}
		next(w, r)
	}
}

func (s *Server) listApps(w http.ResponseWriter, r *http.Request) {
	p := who(r)
	apps, err := s.Deployer.ListApps(r.Context(), p.scope.AllowsApp)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"apps": apps, "token_scope": p.scope.String()})
}

func (s *Server) deploy(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	r.Body = http.MaxBytesReader(w, r.Body, MaxBundleBytes+(1<<20))
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeJSON(w, 413, map[string]string{"error": fmt.Sprintf("bundle exceeds %d MB", MaxBundleBytes>>20)})
			return
		}
		writeJSON(w, 400, map[string]string{"error": "expected multipart/form-data with a 'bundle' file: " + err.Error()})
		return
	}
	defer r.MultipartForm.RemoveAll()
	f, _, err := r.FormFile("bundle")
	if err != nil {
		writeJSON(w, 400, map[string]string{"error": "missing 'bundle' file field"})
		return
	}
	defer f.Close()
	opts := deployer.DeployOptions{
		Domain:          r.FormValue("domain"),
		HealthcheckPath: r.FormValue("healthcheck_path"),
	}
	if ps := strings.TrimSpace(r.FormValue("port")); ps != "" {
		if opts.Port, err = strconv.Atoi(ps); err != nil {
			writeJSON(w, 400, map[string]string{"error": "port must be a number"})
			return
		}
	}
	// Do not abort a half-finished build just because the client disconnected.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), DeployTimeout)
	defer cancel()
	res, err := s.Deployer.Deploy(ctx, name, f, opts)
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Store.Audit(who(r).tokenID, "deploy", name, fmt.Sprintf("%s %s", res.ReleaseID, res.Status))
	status := 200
	if !res.OK {
		status = 422
	}
	writeJSON(w, status, res)
}

func (s *Server) logs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o := logs.Options{Lines: 100, Level: q.Get("level"), Compact: q.Get("compact") != "false"}
	if v, err := strconv.Atoi(q.Get("lines")); err == nil && v > 0 {
		o.Lines = v
	}
	typ := q.Get("type")
	if typ == "" {
		typ = "runtime"
	}
	if typ != "runtime" && typ != "build" {
		writeJSON(w, 400, map[string]string{"error": "type must be runtime or build"})
		return
	}
	res, rel, err := s.Deployer.Logs(r.Context(), r.PathValue("name"), typ, q.Get("release_id"), o)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"app": r.PathValue("name"), "type": typ, "release_id": rel, "lines": res.Lines, "total_lines": res.Total, "truncated": res.Truncated})
}

func (s *Server) rollback(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ReleaseID string `json:"release_id"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, 400, map[string]string{"error": "invalid JSON body"})
			return
		}
	}
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), DeployTimeout)
	defer cancel()
	res, err := s.Deployer.Rollback(ctx, name, strings.TrimSpace(body.ReleaseID))
	if err != nil {
		writeErr(w, err)
		return
	}
	s.Store.Audit(who(r).tokenID, "rollback", name, fmt.Sprintf("%s %s", res.ReleaseID, res.Status))
	status := 200
	if !res.OK {
		status = 422
	}
	writeJSON(w, status, res)
}

func (s *Server) envKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Deployer.EnvKeys(r.PathValue("name"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"app": r.PathValue("name"), "keys": keys, "note": "values are never returned"})
}

func (s *Server) envUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Set    map[string]string `json:"set"`
		Delete []string          `json:"delete"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON body: expected {\"set\":{...},\"delete\":[...]}"})
		return
	}
	if len(body.Set) == 0 && len(body.Delete) == 0 {
		writeJSON(w, 400, map[string]string{"error": "nothing to change: provide set and/or delete"})
		return
	}
	name := r.PathValue("name")
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), DeployTimeout)
	defer cancel()
	res, err := s.Deployer.UpdateEnv(ctx, name, body.Set, body.Delete)
	if err != nil {
		writeErr(w, err)
		return
	}
	setKeys := make([]string, 0, len(body.Set))
	for k := range body.Set {
		setKeys = append(setKeys, k)
	}
	s.Store.Audit(who(r).tokenID, "env_update", name, fmt.Sprintf("set=%v delete=%v", setKeys, body.Delete))
	status := 200
	if res.Applied != nil && !res.Applied.OK {
		status = 422
	}
	writeJSON(w, status, res)
}
