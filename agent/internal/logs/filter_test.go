package logs

import (
	"strings"
	"testing"
)

func TestCompactCollapsesRepeatsAndAnsi(t *testing.T) {
	raw := "\x1b[32mstarting\x1b[0m\nping\nping\nping\n\nready\n"
	r := Filter(raw, Options{Lines: 50, Compact: true})
	got := strings.Join(r.Lines, "|")
	want := "starting|ping|  (previous line repeated 2x)|ready"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}

func TestCompactTrimsStack(t *testing.T) {
	raw := strings.Join([]string{
		"TypeError: x is not a function",
		"    at handler (/app/src/server.js:10:5)",
		"    at Layer.handle (/app/node_modules/express/lib/router/layer.js:95:5)",
		"    at next (/app/node_modules/express/lib/router/route.js:137:13)",
		"    at Module._compile (node:internal/modules/cjs/loader:1256:14)",
		"listening",
	}, "\n")
	r := Filter(raw, Options{Lines: 50, Compact: true})
	got := strings.Join(r.Lines, "\n")
	if strings.Contains(got, "node_modules") || strings.Contains(got, "node:internal") {
		t.Fatalf("noise frames kept:\n%s", got)
	}
	if !strings.Contains(got, "server.js:10:5") || !strings.Contains(got, "3 more frames hidden") {
		t.Fatalf("expected app frame + hidden marker:\n%s", got)
	}
}

func TestErrorLevelAndTail(t *testing.T) {
	raw := "ok 1\nok 2\nError: boom\n    at main (/app/a.js:1:1)\nok 3\nFATAL crash\n"
	r := Filter(raw, Options{Lines: 2, Level: "error", Compact: true})
	if len(r.Lines) != 2 || !r.Truncated {
		t.Fatalf("unexpected: %#v", r)
	}
	if r.Lines[1] != "FATAL crash" {
		t.Fatalf("tail wrong: %#v", r.Lines)
	}
}
