package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestReleaseChecksumsVerifyAfterFlattenedDownload(t *testing.T) {
	for _, tool := range []string{"make", "sha256sum"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s unavailable", tool)
		}
	}
	root := t.TempDir()
	makefile, err := os.ReadFile("Makefile")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "Makefile"), makefile, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "VERSION"), []byte("test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, "dist"), 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"docker-zero-linux-amd64", "docker-zero-linux-arm64"} {
		if err := os.WriteFile(filepath.Join(root, "dist", name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("make", "-o", "build-all", "checksums")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("checksums: %s %v", output, err)
	}
	flat := t.TempDir()
	for _, name := range []string{"docker-zero-linux-amd64", "docker-zero-linux-arm64", "SHA256SUMS"} {
		data, err := os.ReadFile(filepath.Join(root, "dist", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(flat, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	verify := func() ([]byte, error) {
		cmd := exec.Command("sha256sum", "--check", "SHA256SUMS")
		cmd.Dir = flat
		return cmd.CombinedOutput()
	}
	if output, err := verify(); err != nil {
		t.Fatalf("downloaded artifact checksum paths are broken: %s %v", output, err)
	}
	if err := os.WriteFile(filepath.Join(flat, "docker-zero-linux-amd64"), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := verify(); err == nil {
		t.Fatal("checksum accepted a tampered binary")
	}
}
