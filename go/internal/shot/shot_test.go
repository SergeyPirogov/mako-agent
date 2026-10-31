package shot

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The copy has the settings and the data to show, not the log, the phones' keys or old screenshots.
func TestCopyHome(t *testing.T) {
	home, dst := t.TempDir(), filepath.Join(t.TempDir(), "copy")
	write(t, filepath.Join(home, "mako.config.json"), `{"trackers":[]}`)
	write(t, filepath.Join(home, ".env"), "GITHUB_TOKEN=x")
	write(t, filepath.Join(home, ".mako", "state.json"), `{}`)
	write(t, filepath.Join(home, ".mako", "logs", "a.log"), "log")
	write(t, filepath.Join(home, ".mako", "mako.log"), "big")
	write(t, filepath.Join(home, ".mako", "phones.json"), "keys")
	write(t, filepath.Join(home, ".mako", "screenshots", "t", "a.png"), "png")
	if err := CopyHome(home, dst); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"mako.config.json", ".env", ".mako/state.json", ".mako/logs/a.log"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err != nil {
			t.Errorf("not copied: %s", p)
		}
	}
	for _, p := range []string{".mako/mako.log", ".mako/phones.json", ".mako/screenshots"} {
		if _, err := os.Stat(filepath.Join(dst, p)); err == nil {
			t.Errorf("copied: %s", p)
		}
	}
	// A home without data yet still gives a state folder.
	empty := filepath.Join(t.TempDir(), "copy")
	if err := CopyHome(t.TempDir(), empty); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(empty, ".mako")); err != nil {
		t.Fatal(err)
	}
}

func TestModule(t *testing.T) {
	repo := t.TempDir()
	if Module(repo) != "" {
		t.Fatal("found Mako in an empty folder")
	}
	write(t, filepath.Join(repo, "go", "cmd", "mako", "main.go"), "package main")
	if got := Module(repo); got != filepath.Join(repo, "go") {
		t.Fatalf("module %q", got)
	}
}
