package relay

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadTokenKeepsOne(t *testing.T) {
	dir := t.TempDir()
	a, err := LoadToken(dir)
	if err != nil || len(a) < 40 {
		t.Fatalf("%q %v", a, err)
	}
	b, _ := LoadToken(dir)
	if a != b {
		t.Fatal("a new token on the second start: the Mac's address would change")
	}
	if fi, _ := os.Stat(filepath.Join(dir, "relay.json")); fi.Mode().Perm() != 0o600 {
		t.Fatalf("relay.json is %v", fi.Mode().Perm())
	}
	if id := IDFor(a); !validID(id) || id != IDFor(b) || id == IDFor(a+"x") {
		t.Fatalf("id %q", id)
	}
}

func TestAddr(t *testing.T) {
	for env, want := range map[string]string{"": DefaultAddr, "off": "", "https://relay.example.com/": "https://relay.example.com"} {
		t.Setenv("MAKO_RELAY", env)
		if got := Addr(); got != want {
			t.Errorf("MAKO_RELAY=%q: %q, want %q", env, got, want)
		}
	}
}
