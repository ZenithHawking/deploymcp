package auth

import "testing"

func TestGenerateAndHash(t *testing.T) {
	tok, h, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	if !WellFormed(tok) {
		t.Fatalf("token not well formed: %s", tok)
	}
	if Hash(tok) != h {
		t.Fatal("hash mismatch")
	}
	if WellFormed("dmcp_live_xyz") || WellFormed("bad") {
		t.Fatal("malformed token accepted")
	}
}

func TestScope(t *testing.T) {
	s, err := ParseScope("app:web, api", "write")
	if err != nil {
		t.Fatal(err)
	}
	if !s.AllowsApp("web") || !s.AllowsApp("api") || s.AllowsApp("other") {
		t.Fatal("app scope wrong")
	}
	if !s.AllowsWrite() {
		t.Fatal("write expected")
	}
	all, _ := ParseScope("*", "read")
	if !all.AllowsApp("anything") || all.AllowsWrite() {
		t.Fatal("wildcard read scope wrong")
	}
	if _, err := ParseScope("../etc", "read"); err == nil {
		t.Fatal("invalid app name accepted")
	}
	if _, err := ParseScope("web", "admin"); err == nil {
		t.Fatal("invalid perm accepted")
	}
}

func TestValidAppName(t *testing.T) {
	for _, ok := range []string{"a", "my-app", "app1", "x9-y"} {
		if !ValidAppName(ok) {
			t.Errorf("%q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-a", "a-", "A", "a_b", "a/b", "..", "a.b"} {
		if ValidAppName(bad) {
			t.Errorf("%q should be invalid", bad)
		}
	}
}
