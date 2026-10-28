package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Vendor releases ship archives (xray as .zip, sing-box as .tar.gz) while builds
// from the panel are raw binaries. Rather than making the caller know which is
// which, sniff the file and pick the binary out of it.
//
// A nil error with an empty path means "this already is a plain executable".
func extractCoreBinary(src, destDir, core string) (string, error) {
	f, err := os.Open(src)
	if err != nil {
		return "", err
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return "", err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return "", err
	}

	out := filepath.Join(destDir, "."+core+"-unpacked")
	_ = os.Remove(out)

	switch {
	case magic[0] == 0x1f && magic[1] == 0x8b: // gzip
		gz, err := gzip.NewReader(f)
		if err != nil {
			return "", err
		}
		defer gz.Close()
		tr := tar.NewReader(gz)
		for {
			hdr, err := tr.Next()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return "", err
			}
			if hdr.Typeflag != tar.TypeReg || !isCoreName(hdr.Name, core) {
				continue
			}
			if err := writeExec(out, tr); err != nil {
				return "", err
			}
			return out, nil
		}
		return "", fmt.Errorf("%s: no %q binary in archive", filepath.Base(src), core)

	case magic[0] == 0x50 && magic[1] == 0x4b: // zip
		zr, err := zip.NewReader(f, fileSize(src))
		if err != nil {
			return "", err
		}
		for _, zf := range zr.File {
			if zf.FileInfo().IsDir() || !isCoreName(zf.Name, core) {
				continue
			}
			rc, err := zf.Open()
			if err != nil {
				return "", err
			}
			defer rc.Close()
			if err := writeExec(out, rc); err != nil {
				return "", err
			}
			return out, nil
		}
		return "", fmt.Errorf("%s: no %q binary in archive", filepath.Base(src), core)
	}
	return "", nil
}

// isCoreName matches "sing-box", "xray", or a path ending in one of those, and
// skips anything that is obviously not the server binary.
func isCoreName(name, core string) bool {
	base := filepath.Base(name)
	switch core {
	case "singbox":
		return base == "sing-box"
	case "xray":
		return base == "xray"
	}
	return false
}

func writeExec(path string, r io.Reader) error {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Chmod(path, 0o755)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}
