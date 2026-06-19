package main

import (
	"archive/tar"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"
)

// writeTarGz builds a gzipped tar from a set of name->content entries, rooted at
// "./" the same way the backup script's `tar -C /data .` does.
func writeTarGz(t *testing.T, path string, files map[string]string) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestExtractBackupReplacesContents(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "minio-20260101-000000.tar.gz")
	target := filepath.Join(dir, "target")

	// Pre-existing junk that must be wiped by the restore.
	if err := os.MkdirAll(filepath.Join(target, "stale"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(target, "stale", "old.txt"), []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeTarGz(t, archive, map[string]string{
		"./.minio.sys/format.json":          `{"version":"1"}`,
		"./orbit-uploads/uploads/a/part.1":  "hello-bytes",
		"./orbit-uploads/uploads/b/xl.meta": "meta",
	})

	if err := extractBackup(archive, target); err != nil {
		t.Fatalf("extractBackup: %v", err)
	}

	// Restored files present with exact content.
	got, err := os.ReadFile(filepath.Join(target, "orbit-uploads/uploads/a/part.1"))
	if err != nil || string(got) != "hello-bytes" {
		t.Fatalf("restored content = %q, err=%v", got, err)
	}
	if _, err := os.Stat(filepath.Join(target, ".minio.sys/format.json")); err != nil {
		t.Fatalf("expected .minio.sys restored: %v", err)
	}
	// Pre-existing junk gone.
	if _, err := os.Stat(filepath.Join(target, "stale")); !os.IsNotExist(err) {
		t.Fatalf("stale data should have been wiped, err=%v", err)
	}
}

func TestExtractBackupRejectsZipSlip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "minio-20260101-000000.tar.gz")
	target := filepath.Join(dir, "target")
	writeTarGz(t, archive, map[string]string{"../escape.txt": "pwned"})

	if err := extractBackup(archive, target); err == nil {
		t.Fatal("expected extractBackup to reject a path-escaping entry")
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); !os.IsNotExist(err) {
		t.Fatal("zip-slip wrote outside the target directory")
	}
}

func TestBackupStampRegex(t *testing.T) {
	ok := []string{"20260618-020511", "20260101-000000"}
	bad := []string{"20260618-020511/../x", "../../etc", "20260618", "evil", "2026-06-18-000000", "20260618-02051"}
	for _, s := range ok {
		if !backupStamp.MatchString(s) {
			t.Errorf("expected %q to be valid", s)
		}
	}
	for _, s := range bad {
		if backupStamp.MatchString(s) {
			t.Errorf("expected %q to be rejected", s)
		}
	}
}
