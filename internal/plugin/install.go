package plugin

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// Program is the executable every plugin archive must contain.
const Program = "plugin"

// maxUnpacked bounds what an archive may expand to.
const maxUnpacked = 2 << 30

// Dir is where a version of a plugin is installed under root.
func Dir(root, name, version string) string { return filepath.Join(root, name, version) }

// Install verifies archive against the manifest's entry for arch and unpacks
// it to Dir(root, name, version). It replaces nothing: an installed version
// is left as it is and reported. The archive is a gzip-compressed tar of
// regular files and directories only, with the program at its top.
func Install(root string, m Manifest, arch string, archive io.Reader) (string, error) {
	entry, err := m.Archive(arch)
	if err != nil {
		return "", err
	}
	target := Dir(root, m.Name, m.Version)
	if _, err := os.Stat(filepath.Join(target, Program)); err == nil {
		return target, nil
	}
	if err := os.MkdirAll(filepath.Join(root, m.Name), 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Join(root, m.Name), ".install-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(staging)
	// The digest covers exactly the bytes that are unpacked.
	digest := sha256.New()
	limited := &io.LimitedReader{R: archive, N: entry.Size + 1}
	if err := unpack(staging, io.TeeReader(limited, digest)); err != nil {
		return "", err
	}
	if _, err := io.Copy(digest, limited); err != nil {
		return "", errors.New("the plugin archive could not be read to its end")
	}
	read := entry.Size + 1 - limited.N
	if read != entry.Size || hex.EncodeToString(digest.Sum(nil)) != entry.SHA256 {
		return "", errors.New("the plugin archive does not match its signed manifest; nothing was installed")
	}
	info, err := os.Lstat(filepath.Join(staging, Program))
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("the plugin archive has no %q program at its top", Program)
	}
	if err := os.Chmod(filepath.Join(staging, Program), 0o755); err != nil {
		return "", err
	}
	if err := os.Chmod(staging, 0o755); err != nil {
		return "", err
	}
	if err := os.Rename(staging, target); err != nil {
		return "", err
	}
	return target, nil
}

func unpack(dir string, archive io.Reader) error {
	gz, err := gzip.NewReader(archive)
	if err != nil {
		return errors.New("the plugin archive is not a gzip-compressed tar")
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var total int64
	for {
		header, err := tr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return errors.New("the plugin archive is damaged")
		}
		name := filepath.Clean(header.Name)
		if name == "." {
			continue
		}
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, ".."+string(filepath.Separator)) {
			return fmt.Errorf("the plugin archive has an unsafe path %q", header.Name)
		}
		path := filepath.Join(dir, name)
		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if total += header.Size; header.Size < 0 || total > maxUnpacked {
				return errors.New("the plugin archive is too large when unpacked")
			}
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			mode := os.FileMode(0o644)
			if header.Mode&0o111 != 0 {
				mode = 0o755
			}
			file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return err
			}
			_, copyErr := io.CopyN(file, tr, header.Size)
			if err := file.Close(); copyErr != nil || err != nil {
				return errors.New("the plugin archive could not be unpacked")
			}
		default:
			// Links and devices have no place in a plugin.
			return fmt.Errorf("the plugin archive has an entry that is not a file or directory: %q", header.Name)
		}
	}
}
