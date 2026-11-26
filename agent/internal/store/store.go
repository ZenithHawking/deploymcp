// Package store persists apps, releases, tokens and audit events in SQLite.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Release statuses.
const (
	StatusBuilding    = "building"
	StatusHealthy     = "healthy"
	StatusFailed      = "failed"       // container crashed / healthcheck timeout
	StatusBuildFailed = "build_failed" // docker build error
	StatusSuperseded  = "superseded"   // was healthy, replaced by a newer release
)

// Store wraps the SQLite database.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS apps (
	name              TEXT PRIMARY KEY,
	domain            TEXT NOT NULL DEFAULT '',
	port              INTEGER NOT NULL DEFAULT 0,
	healthcheck_path  TEXT NOT NULL DEFAULT '/',
	mode              TEXT NOT NULL DEFAULT '',
	current_release   TEXT NOT NULL DEFAULT '',
	current_instance  TEXT NOT NULL DEFAULT '',
	current_host_port INTEGER NOT NULL DEFAULT 0,
	created_at        TEXT NOT NULL,
	updated_at        TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS releases (
	id          TEXT NOT NULL,
	app         TEXT NOT NULL,
	status      TEXT NOT NULL,
	mode        TEXT NOT NULL DEFAULT '',
	image       TEXT NOT NULL DEFAULT '',
	port        INTEGER NOT NULL DEFAULT 0,
	exit_code   INTEGER,
	error       TEXT NOT NULL DEFAULT '',
	created_at  TEXT NOT NULL,
	PRIMARY KEY (app, id)
);
CREATE INDEX IF NOT EXISTS idx_releases_app ON releases(app, created_at);
CREATE TABLE IF NOT EXISTS tokens (
	id          INTEGER PRIMARY KEY AUTOINCREMENT,
	name        TEXT NOT NULL DEFAULT '',
	hash        TEXT NOT NULL UNIQUE,
	apps        TEXT NOT NULL,
	perm        TEXT NOT NULL,
	created_at  TEXT NOT NULL,
	last_used   TEXT NOT NULL DEFAULT '',
	revoked     INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS audit (
	id        INTEGER PRIMARY KEY AUTOINCREMENT,
	token_id  INTEGER NOT NULL,
	action    TEXT NOT NULL,
	app       TEXT NOT NULL DEFAULT '',
	detail    TEXT NOT NULL DEFAULT '',
	at        TEXT NOT NULL
);
`

// Open opens (and migrates) the database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------- tokens ----------

// Token is a stored API token (hash only).
type Token struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Apps      string `json:"apps"`
	Perm      string `json:"perm"`
	CreatedAt string `json:"created_at"`
	LastUsed  string `json:"last_used"`
	Revoked   bool   `json:"revoked"`
}

// CreateToken stores a token hash.
func (s *Store) CreateToken(name, hash, apps, perm string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO tokens(name, hash, apps, perm, created_at) VALUES(?,?,?,?,?)`, name, hash, apps, perm, now())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// TokenByHash returns a non-revoked token by hash and marks it used.
func (s *Store) TokenByHash(hash string) (*Token, error) {
	t := &Token{}
	var revoked int
	err := s.db.QueryRow(`SELECT id, name, apps, perm, created_at, last_used, revoked FROM tokens WHERE hash = ?`, hash).
		Scan(&t.ID, &t.Name, &t.Apps, &t.Perm, &t.CreatedAt, &t.LastUsed, &revoked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if revoked != 0 {
		return nil, ErrNotFound
	}
	_, _ = s.db.Exec(`UPDATE tokens SET last_used = ? WHERE id = ?`, now(), t.ID)
	return t, nil
}

// ListTokens lists all tokens.
func (s *Store) ListTokens() ([]Token, error) {
	rows, err := s.db.Query(`SELECT id, name, apps, perm, created_at, last_used, revoked FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Token
	for rows.Next() {
		var t Token
		var revoked int
		if err := rows.Scan(&t.ID, &t.Name, &t.Apps, &t.Perm, &t.CreatedAt, &t.LastUsed, &revoked); err != nil {
			return nil, err
		}
		t.Revoked = revoked != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken revokes a token by id.
func (s *Store) RevokeToken(id int64) error {
	res, err := s.db.Exec(`UPDATE tokens SET revoked = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// Audit records an action.
func (s *Store) Audit(tokenID int64, action, app, detail string) {
	_, _ = s.db.Exec(`INSERT INTO audit(token_id, action, app, detail, at) VALUES(?,?,?,?,?)`, tokenID, action, app, detail, now())
}

// ---------- apps ----------

// App is a deployed application.
type App struct {
	Name            string `json:"name"`
	Domain          string `json:"domain"`
	Port            int    `json:"port"`
	HealthcheckPath string `json:"healthcheck_path"`
	Mode            string `json:"mode"`
	CurrentRelease  string `json:"current_release"`
	CurrentInstance string `json:"-"`
	CurrentHostPort int    `json:"-"`
	CreatedAt       string `json:"created_at"`
	UpdatedAt       string `json:"updated_at"`
}

const appCols = `name, domain, port, healthcheck_path, mode, current_release, current_instance, current_host_port, created_at, updated_at`

func scanApp(sc interface{ Scan(...any) error }) (*App, error) {
	a := &App{}
	err := sc.Scan(&a.Name, &a.Domain, &a.Port, &a.HealthcheckPath, &a.Mode, &a.CurrentRelease, &a.CurrentInstance, &a.CurrentHostPort, &a.CreatedAt, &a.UpdatedAt)
	return a, err
}

// GetApp returns an app by name.
func (s *Store) GetApp(name string) (*App, error) {
	a, err := scanApp(s.db.QueryRow(`SELECT `+appCols+` FROM apps WHERE name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return a, err
}

// EnsureApp creates the app row if missing and returns it.
func (s *Store) EnsureApp(name string) (*App, error) {
	t := now()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO apps(name, created_at, updated_at) VALUES(?,?,?)`, name, t, t)
	if err != nil {
		return nil, err
	}
	return s.GetApp(name)
}

// SaveApp updates mutable app fields.
func (s *Store) SaveApp(a *App) error {
	a.UpdatedAt = now()
	_, err := s.db.Exec(`UPDATE apps SET domain=?, port=?, healthcheck_path=?, mode=?, current_release=?, current_instance=?, current_host_port=?, updated_at=? WHERE name=?`,
		a.Domain, a.Port, a.HealthcheckPath, a.Mode, a.CurrentRelease, a.CurrentInstance, a.CurrentHostPort, a.UpdatedAt, a.Name)
	return err
}

// ListApps lists all apps.
func (s *Store) ListApps() ([]*App, error) {
	rows, err := s.db.Query(`SELECT ` + appCols + ` FROM apps ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DomainOwner returns the app that currently owns domain, or "".
func (s *Store) DomainOwner(domain string) (string, error) {
	var name string
	err := s.db.QueryRow(`SELECT name FROM apps WHERE domain = ? AND domain != ''`, domain).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return name, err
}

// ---------- releases ----------

// Release is one deploy attempt.
type Release struct {
	ID        string `json:"id"`
	App       string `json:"app"`
	Status    string `json:"status"`
	Mode      string `json:"mode"`
	Image     string `json:"image,omitempty"`
	Port      int    `json:"port"`
	ExitCode  *int   `json:"exit_code,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at"`
}

const relCols = `id, app, status, mode, image, port, exit_code, error, created_at`

func scanRelease(sc interface{ Scan(...any) error }) (*Release, error) {
	r := &Release{}
	var exit sql.NullInt64
	if err := sc.Scan(&r.ID, &r.App, &r.Status, &r.Mode, &r.Image, &r.Port, &exit, &r.Error, &r.CreatedAt); err != nil {
		return nil, err
	}
	if exit.Valid {
		v := int(exit.Int64)
		r.ExitCode = &v
	}
	return r, nil
}

// CreateRelease inserts a release.
func (s *Store) CreateRelease(r *Release) error {
	r.CreatedAt = now()
	_, err := s.db.Exec(`INSERT INTO releases(`+relCols+`) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.ID, r.App, r.Status, r.Mode, r.Image, r.Port, r.ExitCode, r.Error, r.CreatedAt)
	return err
}

// UpdateRelease updates status fields.
func (s *Store) UpdateRelease(r *Release) error {
	_, err := s.db.Exec(`UPDATE releases SET status=?, mode=?, image=?, port=?, exit_code=?, error=? WHERE app=? AND id=?`,
		r.Status, r.Mode, r.Image, r.Port, r.ExitCode, r.Error, r.App, r.ID)
	return err
}

// GetRelease fetches one release.
func (s *Store) GetRelease(app, id string) (*Release, error) {
	r, err := scanRelease(s.db.QueryRow(`SELECT `+relCols+` FROM releases WHERE app=? AND id=?`, app, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// ReleaseExists reports whether id is taken for app.
func (s *Store) ReleaseExists(app, id string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(1) FROM releases WHERE app=? AND id=?`, app, id).Scan(&n)
	return n > 0
}

// ListReleases returns newest-first releases (limit <= 0 means all).
func (s *Store) ListReleases(app string, limit int) ([]*Release, error) {
	q := `SELECT ` + relCols + ` FROM releases WHERE app=? ORDER BY created_at DESC, id DESC`
	args := []any{app}
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit)
	}
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LatestRollbackTarget returns the newest release (other than exclude) that
// was ever healthy (healthy or superseded).
func (s *Store) LatestRollbackTarget(app, exclude string) (*Release, error) {
	r, err := scanRelease(s.db.QueryRow(`SELECT `+relCols+` FROM releases
		WHERE app=? AND id != ? AND status IN (?, ?)
		ORDER BY created_at DESC, id DESC LIMIT 1`, app, exclude, StatusHealthy, StatusSuperseded))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return r, err
}

// DeleteRelease removes a release row.
func (s *Store) DeleteRelease(app, id string) error {
	_, err := s.db.Exec(`DELETE FROM releases WHERE app=? AND id=?`, app, id)
	return err
}
