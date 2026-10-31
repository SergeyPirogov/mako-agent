package phone

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"syscall"
	"time"
)

// Tailscale: how the phone reaches this Mac with HTTPS, wherever both are.
type Tailscale struct {
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Host      string `json:"host,omitempty"` // mac.tailnet.ts.net
	Serving   bool   `json:"serving"`        // `tailscale serve` passes https://Host to Mako
	Public    bool   `json:"public"`         // through Funnel: from any network, no Tailscale on the phone
	Error     string `json:"error,omitempty"`
}

func tailscaleCLI() string {
	if p, err := exec.LookPath("tailscale"); err == nil {
		return p
	}
	if runtime.GOOS == "darwin" {
		for _, p := range []string{"/Applications/Tailscale.app/Contents/MacOS/Tailscale", "/opt/homebrew/bin/tailscale", "/usr/local/bin/tailscale"} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
	}
	return ""
}

func ts(ctx context.Context, args ...string) ([]byte, error) {
	cli := tailscaleCLI()
	if cli == "" {
		return nil, errors.New("Tailscale не встановлено")
	}
	ctx, cancel := context.WithTimeout(ctx, tsTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, cli, args...)
	// `tailscale funnel` waits until Funnel is allowed for this Mac, and /usr/local/bin/tailscale is a shell
	// script that runs the app's binary as its child. Killing only the script left that child holding the
	// output pipe, so the call never returned and «Зачекайте…» stayed forever: kill the whole group instead.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if ctx.Err() != nil {
			msg = strings.TrimSpace(msg + "\nTailscale не відповів за " + tsTimeout.String())
		}
		return out, fmt.Errorf("tailscale %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// tsTimeout: how long one tailscale command may take; tests shorten it.
var tsTimeout = 20 * time.Second

// TailscaleState: whether Tailscale runs here, the Mac's name in the tailnet, and whether Mako is served on it.
func TailscaleState(ctx context.Context, port int) Tailscale {
	var t Tailscale
	if tailscaleCLI() == "" {
		return t
	}
	t.Installed = true
	out, err := ts(ctx, "status", "--json")
	if err != nil {
		t.Error = err.Error()
		return t
	}
	var st struct {
		BackendState string `json:"BackendState"`
		Self         struct {
			DNSName string `json:"DNSName"`
		} `json:"Self"`
	}
	if json.Unmarshal(out, &st) != nil || st.BackendState != "Running" {
		t.Error = "Tailscale не підключено: увійдіть у застосунку Tailscale"
		return t
	}
	t.Running = true
	t.Host = strings.TrimSuffix(st.Self.DNSName, ".")
	if out, err := ts(ctx, "serve", "status", "--json"); err == nil {
		t.Serving = strings.Contains(string(out), fmt.Sprintf("127.0.0.1:%d", port)) || strings.Contains(string(out), fmt.Sprintf("localhost:%d", port))
		var st struct {
			AllowFunnel map[string]bool `json:"AllowFunnel"`
		}
		if json.Unmarshal(out, &st) == nil {
			for _, on := range st.AllowFunnel {
				t.Public = t.Public || on
			}
		}
	}
	return t
}

// Serve publishes Mako at https://<mac>.<tailnet>.ts.net → http://127.0.0.1:<port>. Private: only devices of
// your tailnet reach it. Public (Funnel): any network reaches it, the phone needs no Tailscale. Either way the
// board shows nothing without a paired phone's key.
func Serve(ctx context.Context, port int, public bool) error {
	Unserve(ctx) // switching between the two: one at a time
	mode := "serve"
	if public {
		mode = "funnel"
	}
	_, err := ts(ctx, mode, "--bg", "--https=443", fmt.Sprintf("http://127.0.0.1:%d", port))
	switch {
	case err == nil:
		return nil
	case public && strings.Contains(strings.ToLower(err.Error()), "funnel"):
		return fmt.Errorf("%v\nFunnel треба дозволити для цього Mac у Tailscale: відкрийте посилання вище або login.tailscale.com → Access controls (nodeAttrs \"funnel\")", err)
	case strings.Contains(err.Error(), "HTTPS"):
		return fmt.Errorf("%v\nУвімкніть HTTPS у Tailscale: login.tailscale.com → DNS → HTTPS Certificates", err)
	}
	return err
}

// Unserve stops publishing Mako, in the tailnet and on the internet.
func Unserve(ctx context.Context) error {
	ts(ctx, "funnel", "--https=443", "off")
	_, err := ts(ctx, "serve", "--https=443", "off")
	return err
}
