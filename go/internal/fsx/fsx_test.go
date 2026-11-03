package fsx

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// WriteFile replaces the content and leaves no temporary file behind.
func TestWriteFileReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(path); string(b) != "new" {
		t.Fatalf("content = %q", b)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "*.tmp")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

// A file that was 0644 becomes 0600: the rights apply to a file that is already there, not only to a new one.
func TestWriteFileTightensRights(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(path, []byte("A=1\n"), 0o644)
	if err := WriteFile(path, []byte("A=2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("mode = %o, want 600", perm)
	}
}

// A write that fails returns the error and does not touch the old file.
func TestWriteFileErrorKeepsOld(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	os.WriteFile(path, []byte("old"), 0o600)
	os.Chmod(dir, 0o500)
	defer os.Chmod(dir, 0o700)
	if err := WriteFile(path, []byte("new"), 0o600); err == nil {
		t.Fatal("no error in a read-only folder")
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatalf("content = %q", b)
	}
	if err := WriteFile(filepath.Join(dir, "missing", "x"), nil, 0o600); err == nil {
		t.Fatal("no error for a missing folder")
	}
}

func TestRotate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mako.log")
	if err := Rotate(path, 10); err != nil {
		t.Fatalf("a missing log: %v", err)
	}
	os.WriteFile(path, []byte("small"), 0o600)
	Rotate(path, 10)
	if _, err := os.Stat(path + ".1"); err == nil {
		t.Fatal("a small log was rotated")
	}
	os.WriteFile(path+".1", []byte("older"), 0o600)
	os.WriteFile(path, []byte("much longer than ten bytes"), 0o600)
	if err := Rotate(path, 10); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the big log is still there")
	}
	if b, _ := os.ReadFile(path + ".1"); string(b) != "much longer than ten bytes" {
		t.Fatalf("mako.log.1 = %q", b)
	}
}
