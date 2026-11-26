package cloudflare

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// fakeCF is an in-memory Cloudflare API with one zone.
type fakeCF struct {
	mu      sync.Mutex
	records []Record
	ingress []any
	tunnels []Tunnel
	nextID  int
}

func (f *fakeCF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ok := func(v any) {
		b, _ := json.Marshal(v)
		json.NewEncoder(w).Encode(map[string]any{"success": true, "errors": []any{}, "result": json.RawMessage(b)})
	}
	p := r.URL.Path
	switch {
	case p == "/zones":
		if r.URL.Query().Get("name") == "example.com" {
			ok([]any{map[string]any{"id": "z1", "name": "example.com", "account": map[string]any{"id": "acc"}}})
		} else {
			ok([]any{})
		}
	case p == "/zones/z1/dns_records" && r.Method == "GET":
		var out []Record
		for _, rec := range f.records {
			if rec.Name == r.URL.Query().Get("name") {
				out = append(out, rec)
			}
		}
		ok(out)
	case p == "/zones/z1/dns_records" && r.Method == "POST":
		var rec Record
		json.NewDecoder(r.Body).Decode(&rec)
		f.nextID++
		rec.ID = string(rune('a' + f.nextID))
		f.records = append(f.records, rec)
		ok(rec)
	case strings.HasPrefix(p, "/zones/z1/dns_records/") && r.Method == "PUT":
		var rec Record
		json.NewDecoder(r.Body).Decode(&rec)
		id := strings.TrimPrefix(p, "/zones/z1/dns_records/")
		for i := range f.records {
			if f.records[i].ID == id {
				rec.ID = id
				f.records[i] = rec
			}
		}
		ok(rec)
	case p == "/accounts/acc/cfd_tunnel" && r.Method == "GET":
		ok(f.tunnels)
	case p == "/accounts/acc/cfd_tunnel" && r.Method == "POST":
		t := Tunnel{ID: "tun1", Name: "x"}
		f.tunnels = append(f.tunnels, t)
		ok(t)
	case p == "/accounts/acc/cfd_tunnel/tun1/configurations":
		var body struct {
			Config struct {
				Ingress []any `json:"ingress"`
			} `json:"config"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		f.ingress = body.Config.Ingress
		ok(map[string]any{})
	default:
		w.WriteHeader(404)
		json.NewEncoder(w).Encode(map[string]any{"success": false, "errors": []any{map[string]any{"code": 7000, "message": "no route " + p}}})
	}
}

func newTest(t *testing.T) (*Client, *fakeCF) {
	f := &fakeCF{}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	c := New("tok")
	c.BaseURL = srv.URL
	return c, f
}

func TestFindZone(t *testing.T) {
	c, _ := newTest(t)
	z, err := c.FindZone(context.Background(), "app.sub.example.com")
	if err != nil || z.ID != "z1" || z.AccountID != "acc" {
		t.Fatalf("zone: %+v %v", z, err)
	}
	if _, err := c.FindZone(context.Background(), "foo.other.org"); err == nil {
		t.Fatal("expected no zone")
	}
}

func TestEnsureRecordNeverTouchesUnmanaged(t *testing.T) {
	c, f := newTest(t)
	f.records = []Record{{ID: "u1", Type: "A", Name: "www.example.com", Content: "1.2.3.4"}}
	err := c.EnsureRecord(context.Background(), "z1", Record{Type: "CNAME", Name: "www.example.com", Content: "x.cfargotunnel.com", Proxied: true})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if f.records[0].Content != "1.2.3.4" {
		t.Fatal("unmanaged record modified")
	}
	if err := c.CheckRecord(context.Background(), "z1", "www.example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("check: %v", err)
	}
}

func TestEnsureRecordCreateThenUpdate(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	if err := c.EnsureRecord(ctx, "z1", Record{Type: "A", Name: "app.example.com", Content: "1.1.1.1"}); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 || f.records[0].Comment != ManagedComment {
		t.Fatalf("create: %+v", f.records)
	}
	if err := c.EnsureRecord(ctx, "z1", Record{Type: "A", Name: "app.example.com", Content: "2.2.2.2"}); err != nil {
		t.Fatal(err)
	}
	if len(f.records) != 1 || f.records[0].Content != "2.2.2.2" {
		t.Fatalf("update: %+v", f.records)
	}
}

func TestTunnelIngress(t *testing.T) {
	c, f := newTest(t)
	ctx := context.Background()
	tun, err := c.EnsureTunnel(ctx, "acc", "deploymcp-test")
	if err != nil || tun.ID != "tun1" {
		t.Fatalf("tunnel: %+v %v", tun, err)
	}
	if tun.Target() != "tun1.cfargotunnel.com" {
		t.Fatal(tun.Target())
	}
	if err := c.SetIngress(ctx, tun, []string{"a.example.com", "b.example.com"}, "http://127.0.0.1:8088"); err != nil {
		t.Fatal(err)
	}
	if len(f.ingress) != 3 {
		t.Fatalf("ingress: %+v", f.ingress)
	}
	again, _ := c.EnsureTunnel(ctx, "acc", "deploymcp-test")
	if again.ID != "tun1" || len(f.tunnels) != 1 {
		t.Fatal("tunnel should be reused")
	}
}
