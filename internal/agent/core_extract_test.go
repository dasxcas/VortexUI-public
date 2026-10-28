package agent

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

func writeTarGz(t *testing.T, path, member, body string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: member, Mode: 0o755, Size: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func writeZip(t *testing.T, path, member, body string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(member)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(body)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestExtractCoreBinaryFromTarGz covers the shape of an upstream sing-box
// release: the binary sits inside a versioned directory, not at the root.
func TestExtractCoreBinaryFromTarGz(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sing-box-1.14.4-linux-amd64.tar.gz")
	writeTarGz(t, src, "sing-box-1.14.4-linux-amd64/sing-box", "#!/bin/sh\necho fake\n")

	out, err := extractCoreBinary(src, dir, "singbox")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if out == "" {
		t.Fatal("no binary extracted")
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "#!/bin/sh\necho fake\n" {
		t.Fatalf("unexpected body %q", body)
	}
	if info, _ := os.Stat(out); info.Mode().Perm()&0o111 == 0 {
		t.Fatal("extracted binary is not executable")
	}
}

// TestExtractCoreBinaryFromZip covers Xray's release shape: a flat zip.
func TestExtractCoreBinaryFromZip(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "Xray-linux-64.zip")
	writeZip(t, src, "xray", "ELF-fake")

	out, err := extractCoreBinary(src, dir, "xray")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	body, _ := os.ReadFile(out)
	if string(body) != "ELF-fake" {
		t.Fatalf("body = %q", body)
	}
}

// TestExtractCoreBinaryLeavesRawBinariesAlone proves a panel-built artifact is
// passed through untouched — that is the metered path.
func TestExtractCoreBinaryLeavesRawBinariesAlone(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "sing-box-1.14.2-metred-linux-amd64")
	if err := os.WriteFile(src, []byte("\x7fELF not really"), 0o755); err != nil {
		t.Fatal(err)
	}
	out, err := extractCoreBinary(src, dir, "singbox")
	if err != nil {
		t.Fatalf("extract: %v", err)
	}
	if out != "" {
		t.Fatalf("raw binary should not be extracted, got %q", out)
	}
}

// TestExtractCoreBinaryRejectsUnrelatedArchive makes sure a wrong asset cannot
// be installed by accident.
func TestExtractCoreBinaryRejectsUnrelatedArchive(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "xray.zip")
	writeZip(t, src, "README.md", "docs")
	if _, err := extractCoreBinary(src, dir, "xray"); err == nil {
		t.Fatal("expected an error when the archive has no core binary")
	}
}
