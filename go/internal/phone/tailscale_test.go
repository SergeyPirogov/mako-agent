package phone

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
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

// Like /usr/local/bin/tailscale on a Mac: a script that runs the real binary as its child, and `funnel` that
// waits for Funnel to be allowed. Serve must give up with the link instead of hanging.
func TestServeGivesUpWhenFunnelWaits(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1 $2" in
  "funnel --bg") echo 'Funnel is not enabled on your tailnet. To enable, visit: https://login.tailscale.com/f/funnel?node=x'; sleep 30 ;;
esac
`
	os.WriteFile(filepath.Join(dir, "tailscale-app"), []byte(script), 0o755)
	os.WriteFile(filepath.Join(dir, "tailscale"), []byte("#!/bin/sh\n"+filepath.Join(dir, "tailscale-app")+` "$@"`+"\n"), 0o755)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	old := tsTimeout
	tsTimeout = 500 * time.Millisecond
	defer func() { tsTimeout = old }()

	start := time.Now()
	err := Serve(context.Background(), 7777, true)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Serve took %v", took)
	}
	if err == nil || !strings.Contains(err.Error(), "https://login.tailscale.com/f/funnel?node=x") || !strings.Contains(err.Error(), "Funnel must be allowed") {
		t.Fatalf("error: %v", err)
	}
}
