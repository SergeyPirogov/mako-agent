package writer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

func TestHeadlessArgs(t *testing.T) {
	work := headlessArgs(HeadlessOpts{Route: core.Route{Model: "sonnet", Effort: "high"}, Budget: 2.5})
	joined := strings.Join(work, " ")
	for _, want := range []string{"-p ", "--permission-mode acceptEdits", "--max-budget-usd 2.50", "--model sonnet", "--effort high",
		"--disallowedTools AskUserQuestion Bash(git push:*) Bash(gh:*) Bash(glab:*) --allowedTools Bash Edit Write NotebookEdit",
		"--no-session-persistence"} {
		if !strings.Contains(joined, want) {
			t.Errorf("no %q in %s", want, joined)
		}
	}
	plan := headlessArgs(HeadlessOpts{Plan: true})
	if !slices.Contains(plan, "plan") || !slices.Contains(plan, "ExitPlanMode") || slices.Contains(plan, "--allowedTools") || slices.Contains(plan, "--max-budget-usd") || slices.Contains(plan, "--model") {
		t.Errorf("plan args %v", plan)
	}
}

// The agent's answer and spend come from its result line; a failure still tells its spend.
func TestHeadless(t *testing.T) {
	dir := t.TempDir()
	got := filepath.Join(t.TempDir(), "got")
	fakeClaude(t, `cat > `+got+`; echo "[$MAKO_EVAL]" >> `+got+`
echo '{"type":"result","result":"done: added the button","total_cost_usd":0.75,"usage":{"input_tokens":10,"output_tokens":5}}'
[ -f fail ] && exit 1
exit 0`)
	var spent float64
	ctx := WithSpend(context.Background(), func(s Spend) { spent += s.USD })
	text, err := Headless(ctx, dir, "do the task", HeadlessOpts{Timeout: 10 * time.Second})
	if err != nil || text != "done: added the button" {
		t.Fatalf("%q %v", text, err)
	}
	if b, _ := os.ReadFile(got); string(b) != "do the task[1]\n" {
		t.Errorf("prompt and env %q", b)
	}
	os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644)
	if _, err := Headless(ctx, dir, "again", HeadlessOpts{Timeout: 10 * time.Second}); err == nil {
		t.Fatal("a failed claude must be an error")
	}
	if spent != 1.5 {
		t.Errorf("spent %v: the failed call's spend must count too", spent)
	}
}

func TestHeadlessCancelAndTimeout(t *testing.T) {
	fakeClaude(t, "exec sleep 30")
	if _, err := Headless(context.Background(), t.TempDir(), "x", HeadlessOpts{Timeout: 200 * time.Millisecond}); err == nil || !strings.Contains(err.Error(), "did not finish") {
		t.Errorf("timeout: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	if _, err := Headless(ctx, t.TempDir(), "x", HeadlessOpts{Timeout: time.Minute}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancel: %v", err)
	}
}
