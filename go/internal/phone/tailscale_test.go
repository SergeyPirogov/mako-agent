package phone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeTailscale puts a tailscale command on PATH that records its calls and answers like the real one.
func fakeTailscale(t *testing.T, serveStatus string) string {
	dir := t.TempDir()
	calls := filepath.Join(dir, "calls")
	script := `#!/bin/sh
printf '%s\n' "$*" >> ` + calls + `
case "$1 $2" in
  "status --json") echo '{"BackendState":"Running","Self":{"DNSName":"mac.tail1234.ts.net."}}' ;;
  "serve status") echo '` + serveStatus + `' ;;
esac
`
	os.WriteFile(filepath.Join(dir, "tailscale"), []byte(script), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

func TestTailscale(t *testing.T) {
	calls := fakeTailscale(t, `{"TCP":{"443":{"HTTPS":true}},"Web":{"mac.tail1234.ts.net:443":{"Handlers":{"/":{"Proxy":"http://127.0.0.1:7777"}}}},"AllowFunnel":{"mac.tail1234.ts.net:443":true}}`)
	st := TailscaleState(context.Background(), 7777)
	if !st.Installed || !st.Running || st.Host != "mac.tail1234.ts.net" || !st.Serving || !st.Public {
		t.Fatalf("%+v", st)
	}
	if err := Serve(context.Background(), 7777, true); err != nil {
		t.Fatal(err)
	}
	if err := Serve(context.Background(), 7777, false); err != nil {
		t.Fatal(err)
	}
	log, _ := os.ReadFile(calls)
	for _, want := range []string{"funnel --https=443 off", "funnel --bg --https=443 http://127.0.0.1:7777", "serve --bg --https=443 http://127.0.0.1:7777"} {
		if !strings.Contains(string(log), want) {
			t.Errorf("no %q in\n%s", want, log)
		}
	}
}
