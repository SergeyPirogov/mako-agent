package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestCheckName(t *testing.T) {
	for cmd, want := range map[string]string{
		"go test ./...": "тести", "npm test": "тести", "pnpm vitest run": "тести", "make test": "тести",
		"go vet ./...": "лінт", "npm run lint": "лінт", "golangci-lint run": "лінт",
		"go build ./...": "збірка", "npm run build": "збірка",
		"./scripts/verify.sh": "./scripts/verify.sh",
	} {
		if got := checkName(cmd); got != want {
			t.Errorf("checkName(%q) = %q, want %q", cmd, got, want)
		}
	}
	line := checksLine([]core.CheckResult{{Name: "тести", OK: true}, {Name: "лінт"}, {Name: "збірка", TimedOut: true}})
	if line != "✓ тести, ✗ лінт, ⏱ збірка" {
		t.Fatal(line)
	}
}

// checksSetup: Mako's agent on grp/app#5 with its worktree, the repo's checks, and the origin behind the fake
// GitLab's address, so Mako can push the branch and open the MR there.
func checksSetup(t *testing.T, prep func(*config.Config), checks ...string) (d *Dispatcher, cfg *config.Config, repo *config.Repo, id, wt, origin string, idle func()) {
	d, cfg, id, wt, idle = reviewSetup(t, func(c *config.Config) {
		for _, r := range c.Trackers[0].Projects["grp/app"].Repos {
			r.Checks = checks
		}
		if prep != nil {
			prep(c)
		}
	})
	for _, r := range cfg.Trackers[0].Projects["grp/app"].Repos {
		repo = r
	}
	origin = strings.TrimSpace(gitOut(t, repo.Path, "config", "--get", "remote.origin.url"))
	host := strings.TrimPrefix(cfg.Trackers[0].BaseURL, "http://")
	host = host[:strings.Index(host, ":")]
	address := "git@" + host + ":grp/app.git"
	git(t, repo.Path, "remote", "set-url", "origin", address)
	git(t, repo.Path, "config", "url."+origin+".insteadOf", address)
	old := reviewText
	reviewText = func(ctx context.Context, worktree, base string, task core.Task) (string, string, error) {
		return "Fix the pay button", "- the button pays again", nil
	}
	t.Cleanup(func() { reviewText = old })
	return
}

func said(t *testing.T, wt string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(wt, "said.json"))
	return string(b)
}

func commit(t *testing.T, wt, name string) {
	t.Helper()
	os.WriteFile(filepath.Join(wt, name), []byte("package app\n"), 0o644)
	git(t, wt, "add", name)
	git(t, wt, "commit", "-q", "-m", "Change "+name)
}

func TestChecksFailThenPassOpenMR(t *testing.T) {
	// The tests fail until the agent adds pay_test.go.
	d, _, repo, id, wt, origin, idle := checksSetup(t, nil, "echo vet fine", "test -f pay_test.go || { echo FAIL: TestPay; exit 1; }")

	// Nothing committed yet: nothing to check.
	checkNow(t, d, id)
	if r, _ := d.Store.Get(id); r.Checks != nil {
		t.Fatalf("checked a branch with no commits: %+v", r.Checks)
	}

	commit(t, wt, "pay.go")
	checkNow(t, d, id)
	r, _ := d.Store.Get(id)
	c := r.Checks
	if c == nil || c.State != "failed" || c.Fails != 1 || c.Attention || len(c.Results) != 2 || !c.Results[0].OK || c.Results[1].OK {
		t.Fatalf("checks %+v", c)
	}
	if !strings.Contains(c.Results[1].Tail, "FAIL: TestPay") {
		t.Fatalf("tail %q", c.Results[1].Tail)
	}
	idle()
	if s := said(t, wt); !strings.Contains(s, "FAIL: TestPay") || !strings.Contains(s, "спроба 1 з 3") {
		t.Fatalf("the agent got %s", s)
	}
	// No MR, nothing pushed.
	if out := gitOut(t, repo.Path, "ls-remote", origin, "fix/5-pay"); strings.TrimSpace(out) != "" {
		t.Fatalf("pushed a branch whose checks failed: %s", out)
	}
	d.CheckHandoffs(context.Background())
	if r, _ := d.Store.Get(id); r.Status != core.HandedOff || r.PRURL != "" {
		t.Fatalf("an MR for failed checks: %+v", r)
	}

	// The same commit is not checked again.
	at := c.At
	checkNow(t, d, id)
	if r, _ := d.Store.Get(id); r.Checks.At != at {
		t.Fatal("checked the same commit twice")
	}

	// Fixed: the checks pass, Mako pushes the branch and opens the MR.
	commit(t, wt, "pay_test.go")
	checkNow(t, d, id)
	r, _ = d.Store.Get(id)
	if r.Checks.State != "passed" || r.Checks.Fails != 0 || r.Checks.Error != "" {
		t.Fatalf("checks %+v", r.Checks)
	}
	head := strings.TrimSpace(gitOut(t, wt, "rev-parse", "HEAD"))
	if out := gitOut(t, repo.Path, "ls-remote", origin, "fix/5-pay"); !strings.HasPrefix(out, head) {
		t.Fatalf("the branch was not pushed at %s: %q", head, out)
	}
	d.CheckHandoffs(context.Background())
	r, _ = d.Store.Get(id)
	if r.Status != core.Done || r.PRURL == "" {
		t.Fatalf("the MR was not found: %+v", r)
	}
	host, project, _ := d.host(context.Background(), d.tracker(r.Task.TrackerID), repo)
	title, description, err := host.ReadReview(context.Background(), r.PRURL)
	if err != nil || title != "Fix the pay button" || !strings.Contains(description, "- ✓ `echo vet fine`") || !strings.Contains(description, "the button pays again") {
		t.Fatalf("%s MR %q %q %v", project, title, description, err)
	}
}

func TestChecksTimeoutNeedsAttention(t *testing.T) {
	d, _, _, id, wt, _, idle := checksSetup(t, func(c *config.Config) { c.Terminal.CheckTimeoutSec, c.Terminal.CheckAttempts = 1, 1 }, "sleep 5")
	commit(t, wt, "pay.go")
	checkNow(t, d, id)
	r, _ := d.Store.Get(id)
	c := r.Checks
	if c == nil || c.State != "failed" || !c.Results[0].TimedOut || !c.Attention || c.Fails != 1 {
		t.Fatalf("checks %+v", c)
	}
	if c.Results[0].Seconds > 3 {
		t.Fatalf("the timeout did not stop it: %d s", c.Results[0].Seconds)
	}
	idle()
	// The attempts are used up: the agent is not asked, the task waits for you.
	if s := said(t, wt); strings.Contains(s, "перевірку") {
		t.Fatalf("the agent was asked after the last attempt: %s", s)
	}
}

func TestNoChecksAsBefore(t *testing.T) {
	d, _, _, id, wt, _, _ := checksSetup(t, nil)
	commit(t, wt, "pay.go")
	r, _ := d.Store.Get(id)
	if d.checksFor(r) != nil {
		t.Fatal("a repo without checks is gated")
	}
	checkNow(t, d, id)
	if r, _ := d.Store.Get(id); r.Checks != nil {
		t.Fatalf("checks ran: %+v", r.Checks)
	}
}

func TestChecksNotForMRsByMe(t *testing.T) {
	d, _, _, id, _, _, _ := checksSetup(t, func(c *config.Config) { c.Orca.MRBy = "me" }, "true")
	r, _ := d.Store.Get(id)
	if d.checksFor(r) != nil {
		t.Fatal("gated while you open the MRs yourself")
	}
}

func TestChecksPassedButSecretsStopPush(t *testing.T) {
	d, _, repo, id, wt, origin, idle := checksSetup(t, nil, "true")
	token := "ghp_" + "Zq3" + strings.Repeat("aB9xK", 6) + "Lm7"
	os.WriteFile(filepath.Join(wt, "conf.go"), []byte("package app\n\nconst tok = \""+token+"\"\n"), 0o644)
	git(t, wt, "add", "conf.go")
	git(t, wt, "commit", "-q", "-m", "Add conf")
	checkNow(t, d, id)
	r, _ := d.Store.Get(id)
	if r.Checks.State != "passed" || !strings.Contains(r.Checks.Error, "секрети") || len(r.Secrets) == 0 {
		t.Fatalf("checks %+v secrets %+v", r.Checks, r.Secrets)
	}
	if out := gitOut(t, repo.Path, "ls-remote", origin, "fix/5-pay"); strings.TrimSpace(out) != "" {
		t.Fatalf("Mako pushed a secret: %s", out)
	}
	idle()
	if s := said(t, wt); !strings.Contains(s, "секрети") {
		t.Fatalf("the agent was not told: %s", s)
	}
}
