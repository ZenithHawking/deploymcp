package deployer

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type entry struct {
	name, body, link string
	typ              byte
}

func makeTarGz(t *testing.T, entries []entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		h := &tar.Header{Name: e.name, Mode: 0o644, Typeflag: e.typ, Linkname: e.link}
		if e.typ == tar.TypeReg {
			h.Size = int64(len(e.body))
		}
		if e.typ == tar.TypeDir {
			h.Mode = 0o755
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return &buf
}

func TestExtractOK(t *testing.T) {
	dir := t.TempDir()
	buf := makeTarGz(t, []entry{
		{name: "src/", typ: tar.TypeDir},
		{name: "src/index.js", body: "hi", typ: tar.TypeReg},
		{name: "Dockerfile", body: "FROM x", typ: tar.TypeReg},
		{name: "link", link: "/etc/passwd", typ: tar.TypeSymlink},
	})
	skipped, err := ExtractTarGz(buf, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(skipped) != 1 || skipped[0] != "link" {
		t.Fatalf("symlink should be skipped, got %v", skipped)
	}
	b, err := os.ReadFile(filepath.Join(dir, "src", "index.js"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("bad content %q %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "link")); !os.IsNotExist(err) {
		t.Fatal("symlink was created")
	}
}

func TestExtractRejectsTraversal(t *testing.T) {
	for _, name := range []string{"../evil", "a/../../evil", "/etc/passwd", "C:/x", `..\evil`} {
		dir := t.TempDir()
		buf := makeTarGz(t, []entry{{name: name, body: "x", typ: tar.TypeReg}})
		_, err := ExtractTarGz(buf, dir)
		if !errors.Is(err, ErrUnsafePath) {
			t.Errorf("%q: expected ErrUnsafePath, got %v", name, err)
		}
	}
}

func TestDetectExposePort(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte("FROM node\n# EXPOSE 1\nexpose 8080/tcp 9090\n"), 0o644)
	if p := detectDockerfilePort(dir); p != 8080 {
		t.Fatalf("got %d", p)
	}
}
