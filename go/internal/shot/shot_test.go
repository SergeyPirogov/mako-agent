package shot

import (
	"image"
	"image/png"
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

// A phone's screenshot is taken in Chrome's narrowest window and cut to the phone's width.
func TestCrop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 500, 900))); err != nil {
		t.Fatal(err)
	}
	f.Close()
	if err := crop(path, 430, 932); err != nil {
		t.Fatal(err)
	}
	f, err = os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 430 || b.Dy() != 900 {
		t.Fatalf("cropped to %v, want 430×900", b)
	}
}
