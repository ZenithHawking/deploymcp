package caddy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBuildConfigModes(t *testing.T) {
	routes := []Route{{Domain: "app.example.com", Upstream: "127.0.0.1:4000"}}
	c := New("http://localhost:2019", "me@example.com")
	b, _ := json.Marshal(c.BuildConfig(routes))
	s := string(b)
	if !strings.Contains(s, `":443"`) || !strings.Contains(s, `"acme"`) || !strings.Contains(s, "127.0.0.1:4000") {
		t.Fatalf("https config wrong: %s", s)
	}
	c.HTTPOnly = true
	b, _ = json.Marshal(c.BuildConfig(routes))
	s = string(b)
	if strings.Contains(s, `":443"`) || strings.Contains(s, `"acme"`) || !strings.Contains(s, `"automatic_https":{"disable":true}`) {
		t.Fatalf("tunnel config wrong: %s", s)
	}
	c.HTTPListen = "127.0.0.1:8088"
	b, _ = json.Marshal(c.BuildConfig(routes))
	if !strings.Contains(string(b), `"listen":["127.0.0.1:8088"]`) {
		t.Fatalf("custom listen missing: %s", b)
	}
	if !strings.Contains(s, `"listen":"localhost:2019"`) {
		t.Fatalf("admin listen wrong: %s", s)
	}
}
