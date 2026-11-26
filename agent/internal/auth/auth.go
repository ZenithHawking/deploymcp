// Package auth generates and validates scoped API tokens.
//
// Token format: dmcp_live_<64 hex chars>. Only the SHA-256 hash is stored.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

const Prefix = "dmcp_live_"

// Permission levels. Write implies read.
const (
	PermRead  = "read"
	PermWrite = "write"
)

var appNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,48}[a-z0-9])?$`)

// ValidAppName reports whether name is a valid app identifier
// (lowercase letters, digits, dashes; 1-50 chars; no leading/trailing dash).
func ValidAppName(name string) bool { return appNameRe.MatchString(name) }

// Generate returns a new plaintext token and its storage hash.
func Generate() (token, hash string, err error) {
	b := make([]byte, 32)
	if _, err = rand.Read(b); err != nil {
		return "", "", err
	}
	token = Prefix + hex.EncodeToString(b)
	return token, Hash(token), nil
}

// Hash returns the hex SHA-256 of a token.
func Hash(token string) string {
	s := sha256.Sum256([]byte(token))
	return hex.EncodeToString(s[:])
}

// WellFormed checks the token shape without touching storage.
func WellFormed(token string) bool {
	if !strings.HasPrefix(token, Prefix) {
		return false
	}
	h := strings.TrimPrefix(token, Prefix)
	if len(h) != 64 {
		return false
	}
	_, err := hex.DecodeString(h)
	return err == nil
}

// Scope describes what a token may touch.
type Scope struct {
	Apps []string // app names, or ["*"]
	Perm string   // read | write
}

// ParseScope parses "app:<name>,app:<name>|*" style app lists and a permission.
// apps accepts: "*", "foo", "app:foo", "foo,bar".
func ParseScope(apps, perm string) (Scope, error) {
	perm = strings.ToLower(strings.TrimSpace(perm))
	if perm != PermRead && perm != PermWrite {
		return Scope{}, fmt.Errorf("perm must be %q or %q", PermRead, PermWrite)
	}
	var list []string
	for _, a := range strings.Split(apps, ",") {
		a = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(a), "app:"))
		if a == "" {
			continue
		}
		if a == "*" {
			list = []string{"*"}
			break
		}
		if !ValidAppName(a) {
			return Scope{}, fmt.Errorf("invalid app name %q", a)
		}
		list = append(list, a)
	}
	if len(list) == 0 {
		return Scope{}, errors.New("at least one app (or *) is required")
	}
	return Scope{Apps: list, Perm: perm}, nil
}

// AppsString serializes app list for storage.
func (s Scope) AppsString() string { return strings.Join(s.Apps, ",") }

// AllowsApp reports whether the scope covers app.
func (s Scope) AllowsApp(app string) bool {
	for _, a := range s.Apps {
		if a == "*" || a == app {
			return true
		}
	}
	return false
}

// AllowsWrite reports whether the scope has write permission.
func (s Scope) AllowsWrite() bool { return s.Perm == PermWrite }

// String renders the scope like "app:foo,app:bar write".
func (s Scope) String() string {
	parts := make([]string, len(s.Apps))
	for i, a := range s.Apps {
		if a == "*" {
			parts[i] = "*"
		} else {
			parts[i] = "app:" + a
		}
	}
	return strings.Join(parts, ",") + " " + s.Perm
}
