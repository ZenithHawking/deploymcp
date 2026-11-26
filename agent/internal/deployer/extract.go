package deployer

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// MaxExtractedBytes caps the total uncompressed size of a bundle.
const MaxExtractedBytes int64 = 1 << 30 // 1 GiB

// MaxFiles caps the number of entries in a bundle.
const MaxFiles = 200_000

// ErrUnsafePath is returned when an archive entry would escape the target dir.
var ErrUnsafePath = errors.New("unsafe path in archive")

// safeJoin resolves name inside dest, rejecting absolute paths and traversal.
func safeJoin(dest, name string) (string, error) {
	name = strings.ReplaceAll(name, "\\", "/")
	if name == "" || strings.HasPrefix(name, "/") || (len(name) > 1 && name[1] == ':') {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == ".." {
			return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
		}
	}
	clean := path.Clean(name)
	if clean == "." {
		return dest, nil
	}
	full := filepath.Join(dest, filepath.FromSlash(clean))
	rel, err := filepath.Rel(dest, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q", ErrUnsafePath, name)
	}
	return full, nil
}

// ExtractTarGz safely extracts a gzip'd tarball into dest (which must exist).
//
// Only regular files and directories are written. Symlinks, hardlinks and
// device nodes are skipped (their names are returned) so nothing can point
// outside dest. File modes are limited to 0755/0644.
func ExtractTarGz(r io.Reader, dest string) (skipped []string, err error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("bundle is not a valid .tar.gz: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	files := 0
	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return skipped, fmt.Errorf("read bundle: %w", err)
		}
		files++
		if files > MaxFiles {
			return skipped, fmt.Errorf("bundle has more than %d entries", MaxFiles)
		}
		target, err := safeJoin(dest, h.Name)
		if err != nil {
			return skipped, err
		}
		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o755); err != nil {
				return skipped, err
			}
		case tar.TypeReg:
			total += h.Size
			if total > MaxExtractedBytes {
				return skipped, fmt.Errorf("bundle exceeds %d bytes uncompressed", MaxExtractedBytes)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return skipped, err
			}
			mode := os.FileMode(0o644)
			if h.FileInfo().Mode()&0o111 != 0 {
				mode = 0o755
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return skipped, err
			}
			n, err := io.Copy(f, io.LimitReader(tr, h.Size+1))
			cerr := f.Close()
			if err != nil {
				return skipped, err
			}
			if cerr != nil {
				return skipped, cerr
			}
			if n != h.Size {
				return skipped, fmt.Errorf("size mismatch for %q", h.Name)
			}
		case tar.TypeXGlobalHeader, tar.TypeXHeader:
			// metadata only
		default:
			skipped = append(skipped, h.Name)
		}
	}
	return skipped, nil
}
