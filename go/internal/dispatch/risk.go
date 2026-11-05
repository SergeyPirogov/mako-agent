package dispatch

import (
	"context"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
)

// The change's risk (core.RiskOf) and the repo's policy by it (config.RiskPolicy):
//   - low: with autoMergeLow Mako starts the reviewer agent itself and merges the MR once its CI is green and the
//     reviewer found nothing to fix; otherwise as medium;
//   - medium (and a level not known yet): as before, the agent advises, you merge;
//   - high: the reviewer agent starts by itself and the merge waits for «Check it yourself» (gateHigh, on by default);
//   - block: the change touches a path the repo forbids, and does not merge until it does not.
// The reviewer and the check are there for Mako's own agents only, so for an agent in Orca high is advice.

// Kinds of files the signals tell apart, by name.
var (
	codeExt = map[string]bool{
		".go": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".tsx": true, ".jsx": true, ".vue": true,
		".svelte": true, ".py": true, ".rb": true, ".java": true, ".kt": true, ".kts": true, ".scala": true,
		".swift": true, ".m": true, ".mm": true, ".rs": true, ".c": true, ".cc": true, ".cpp": true, ".h": true,
		".hpp": true, ".cs": true, ".php": true, ".ex": true, ".exs": true, ".erl": true, ".dart": true, ".lua": true,
		".sh": true,
	}
	testDirs = map[string]bool{"test": true, "tests": true, "__tests__": true, "spec": true, "specs": true, "testdata": true, "e2e": true}
	depFiles = map[string]bool{
		"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true, "npm-shrinkwrap.json": true,
		"yarn.lock": true, "pnpm-lock.yaml": true, "bun.lockb": true, "Cargo.toml": true, "Cargo.lock": true,
		"Gemfile": true, "Gemfile.lock": true, "pyproject.toml": true, "poetry.lock": true, "Pipfile": true,
		"Pipfile.lock": true, "uv.lock": true, "composer.json": true, "composer.lock": true, "pom.xml": true,
		"build.gradle": true, "build.gradle.kts": true, "gradle.lockfile": true, "Podfile.lock": true,
		"Package.resolved": true, "mix.lock": true, "pubspec.lock": true,
	}
	requirements = regexp.MustCompile(`^requirements.*\.txt$`)
	// revertWord: a commit whose subject starts as one that undid or fixed in a hurry what was merged ("Revert «…»",
	// "hotfix: …", "[hotfix] …"); a mention in the middle ("Document the hotfix process") is not one.
	revertWord = regexp.MustCompile(`(?i)^[\s\[(]*(revert|rollback|hotfix)\b`)
)

// isTestFile: a test, by its name or its folder.
func isTestFile(p string) bool {
	name := path.Base(p)
	if strings.HasSuffix(name, "_test.go") || strings.HasPrefix(name, "test_") || strings.Contains(name, ".test.") ||
		strings.Contains(name, ".spec.") || strings.HasSuffix(strings.TrimSuffix(name, path.Ext(name)), "_spec") ||
		strings.HasSuffix(strings.TrimSuffix(name, path.Ext(name)), "Test") || strings.HasSuffix(strings.TrimSuffix(name, path.Ext(name)), "Tests") {
		return true
	}
	return slices.ContainsFunc(strings.Split(path.Dir(p), "/"), func(d string) bool { return testDirs[d] })
}

func isCodeFile(p string) bool { return codeExt[strings.ToLower(path.Ext(p))] && !isTestFile(p) }

func isDepFile(p string) bool {
	name := path.Base(p)
	return depFiles[name] || requirements.MatchString(name)
}

// The history the signal reads: so many commits of the base branch at most, a level by how many undid or hotfixed
// the same files.
const (
	historyDepth  = 300
	historyFiles  = 100
	historyMedium = 2
	historyHigh   = 5
	signalFiles   = 10 // the files a signal names at most
)

// riskSignals: what the change says of its risk by itself, without a model.
func riskSignals(ctx context.Context, wt, base string, files []gitx.ChangedFile, repo *config.Repo) []core.RiskSignal {
	var paths []config.RiskPath
	policy := config.RiskPolicy{MaxLines: config.DefaultRiskMaxLines, MaxFiles: config.DefaultRiskMaxFiles}
	if repo != nil {
		paths, policy = repo.RiskPaths, repo.Risk
		policy.MaxLines = orDefault(policy.MaxLines, config.DefaultRiskMaxLines)
		policy.MaxFiles = orDefault(policy.MaxFiles, config.DefaultRiskMaxFiles)
	}
	var out []core.RiskSignal
	for _, rp := range paths {
		var hit []string
		for _, f := range files {
			if core.MatchPath(rp.Pattern, f.Path) {
				hit = append(hit, f.Path)
			}
		}
		if len(hit) > 0 {
			text := "Touches " + rp.Pattern
			if rp.Level == core.RiskBlock {
				text = "Touches a protected path: " + rp.Pattern
			}
			out = append(out, core.RiskSignal{Kind: core.SignalPath, Level: rp.Level, Text: text, Files: clip(hit)})
		}
	}
	lines := 0
	for _, f := range files {
		lines += f.Additions + f.Deletions
	}
	if level := sizeLevel(lines, len(files), policy); level != "" {
		out = append(out, core.RiskSignal{Kind: core.SignalSize, Level: level,
			Text: fmt.Sprintf("Big change: %d lines in %d files (more than %d lines or %d files is medium)", lines, len(files), policy.MaxLines, policy.MaxFiles)})
	}
	var code, removed, deps []string
	tests := false
	for _, f := range files {
		switch {
		case isTestFile(f.Path):
			tests = true
		case f.Status != "D" && isCodeFile(f.Path):
			code = append(code, f.Path)
		}
		if (f.Status == "D" || f.Status == "R") && !isTestFile(f.Path) {
			removed = append(removed, f.Path)
		}
		if isDepFile(f.Path) {
			deps = append(deps, f.Path)
		}
	}
	if len(code) > 0 && !tests {
		out = append(out, core.RiskSignal{Kind: core.SignalNoTests, Level: core.RiskMedium, Text: "Code changed without a test", Files: clip(code)})
	}
	if len(removed) > 0 {
		out = append(out, core.RiskSignal{Kind: core.SignalRemoved, Level: core.RiskMedium,
			Text: "Removes or renames " + fileCount(len(removed)), Files: clip(removed)})
	}
	if len(deps) > 0 {
		out = append(out, core.RiskSignal{Kind: core.SignalDeps, Level: core.RiskMedium, Text: "Changes dependencies", Files: clip(deps)})
	}
	if s := historySignal(ctx, wt, base, files); s != nil {
		out = append(out, *s)
	}
	return out
}

func orDefault(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

// fileCount: "1 file", "3 files".
func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return strconv.Itoa(n) + " files"
}

// sizeLevel: medium above the thresholds, high above three times them, "" within them.
func sizeLevel(lines, files int, p config.RiskPolicy) string {
	switch {
	case lines > 3*p.MaxLines || files > 3*p.MaxFiles:
		return core.RiskHigh
	case lines > p.MaxLines || files > p.MaxFiles:
		return core.RiskMedium
	}
	return ""
}

func clip(files []string) []string {
	if len(files) > signalFiles {
		return files[:signalFiles]
	}
	return files
}

// historySignal: the files of the change were in reverts or hotfixes of the base branch before: medium from
// historyMedium such commits, high from historyHigh; nil when fewer, or when git cannot tell.
func historySignal(ctx context.Context, wt, base string, files []gitx.ChangedFile) *core.RiskSignal {
	if base == "" || len(files) == 0 {
		return nil
	}
	var paths []string
	for _, f := range files {
		if f.Status != "A" && f.Status != "?" { // a new file has no history
			paths = append(paths, f.Path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	if len(paths) > historyFiles {
		paths = paths[:historyFiles]
	}
	var out string
	for _, ref := range []string{"origin/" + base, base} {
		args := append([]string{"-c", "core.quotePath=false", "log", "-n", strconv.Itoa(historyDepth), "--no-merges", "--format=%x00%s", "--name-only", ref, "--"}, paths...)
		var err error
		if out, err = gitx.Run(ctx, wt, args...); err == nil {
			break
		}
		out = ""
	}
	n := 0
	seen := map[string]bool{}
	var hit []string
	for _, c := range strings.Split(out, "\x00") {
		subject, names, _ := strings.Cut(c, "\n")
		if !revertWord.MatchString(subject) {
			continue
		}
		n++
		for _, f := range strings.Split(names, "\n") {
			if f = strings.TrimSpace(f); f != "" && !seen[f] && slices.Contains(paths, f) {
				seen[f] = true
				hit = append(hit, f)
			}
		}
	}
	level := ""
	switch {
	case n >= historyHigh:
		level = core.RiskHigh
	case n >= historyMedium:
		level = core.RiskMedium
	default:
		return nil
	}
	return &core.RiskSignal{Kind: core.SignalHistory, Level: level,
		Text: fmt.Sprintf("The files were in %d reverts or hotfixes before", n), Files: clip(hit)}
}

// lookRisk reads the risk of the task's branch as it is now and keeps it on the task.
func (d *Dispatcher) lookRisk(ctx context.Context, id string) {
	wt, base, err := d.workOf(id)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	head, err := gitx.Run(ctx, wt, "rev-parse", "HEAD")
	if err != nil || head == "" {
		return
	}
	cs, err := gitx.Changes(ctx, wt, base)
	if err != nil {
		return
	}
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	look := &core.RiskLook{At: now(), Commit: head, Signals: riskSignals(ctx, wt, base, cs.Files, d.repoOf(r))}
	before := r.Risk.Level()
	d.Store.UpdateIfExists(id, func(r *core.Record) { r.Risk = look })
	if after := look.Level(); after != before && after != "" {
		d.Log(id, "change risk from the diff: "+after)
	}
}

// freshRisk: the record with the risk of the branch as it is now: read again when the branch has commits after the
// last look (or none yet), so a merge is judged by what is in the branch, not by what the round saw before.
func (d *Dispatcher) freshRisk(ctx context.Context, r core.Record) core.Record {
	if !riskOpen(r) {
		return r
	}
	head, err := d.headOf(r.ID)
	if err != nil || r.Risk != nil && r.Risk.Commit == head {
		return r
	}
	d.lookRisk(ctx, r.ID)
	if fresh, ok := d.Store.Get(r.ID); ok {
		return fresh
	}
	return r
}

// riskOpen: the task's MR is open and its branch is there to read: its risk is worth keeping up to date.
func riskOpen(r core.Record) bool {
	return r.ArchivedAt == 0 && r.Status == core.Done && r.PRURL != "" && r.ReviewState == "" && r.Worktree != "" && exists(r.Worktree)
}

// CheckRisk reads the risk of each open MR's branch again once it has new commits.
func (d *Dispatcher) CheckRisk(ctx context.Context) {
	for _, r := range d.Store.All() {
		if !riskOpen(r) {
			continue
		}
		c, cancel := context.WithTimeout(ctx, 10*time.Second)
		head, err := gitx.Run(c, r.Worktree, "rev-parse", "HEAD")
		cancel()
		if err != nil || head == "" || r.Risk != nil && r.Risk.Commit == head {
			continue
		}
		d.lookRisk(ctx, r.ID)
	}
}

// riskPolicy: the policy of the task's repo (the defaults when the repo is not known).
func (d *Dispatcher) riskPolicy(r core.Record) config.RiskPolicy {
	if repo := d.repoOf(r); repo != nil {
		return repo.Risk
	}
	return config.RiskPolicy{GateHigh: true, MaxLines: config.DefaultRiskMaxLines, MaxFiles: config.DefaultRiskMaxFiles}
}

// riskGate: the task's risk is high and its repo holds such a merge for «Check it yourself».
func (d *Dispatcher) riskGate(r core.Record) bool {
	level, _ := core.RiskOf(r)
	return level == core.RiskHigh && d.riskPolicy(r).GateHigh
}

// riskBlocks: the signals that keep the MR from merging (a protected path the branch touches), at its HEAD as Mako
// last read it.
func riskBlocks(r core.Record) []core.RiskSignal { return r.Risk.Blocked() }

// riskBlockText: why the merge is blocked by the change's risk, "" when it is not.
func riskBlockText(r core.Record) string {
	var files []string
	for _, s := range riskBlocks(r) {
		files = append(files, s.Files...)
	}
	if len(files) == 0 {
		return ""
	}
	return "the change touches protected paths: " + strings.Join(clip(files), ", ")
}

// RiskView is the task's risk as the board shows it.
type RiskView struct {
	Level   string            `json:"level"`
	Signals []core.RiskSignal `json:"signals,omitempty"`
	// Policy: what the level does here: "auto" (merges by itself), "advice" (the merge is yours), "gate" (waits for
	// «Check it yourself»), "block" (does not merge).
	Policy string `json:"policy"`
	// Stale: the branch got commits after Mako read its diff.
	Stale bool `json:"stale,omitempty"`
}

// RiskView: the task's risk for the board; nil while nothing is known of it. It reads no network, and git only
// through the board's caches.
func (d *Dispatcher) RiskView(r core.Record) *RiskView {
	level, signals := core.RiskOf(r)
	if level == "" {
		return nil
	}
	v := &RiskView{Level: level, Signals: signals, Policy: "advice"}
	p := d.riskPolicy(r)
	switch {
	case level == core.RiskBlock:
		v.Policy = "block"
	case level == core.RiskHigh && p.GateHigh && onTerminal(r):
		v.Policy = "gate"
	case level == core.RiskLow && p.AutoMergeLow && onTerminal(r):
		v.Policy = "auto"
	}
	if r.Risk != nil && reviewable(r) {
		if head := d.selfHead(r); head != "" && head != r.Risk.Commit {
			v.Stale = true
		}
	}
	return v
}

// pruneRisk forgets what AutoMerge and AutoRiskReview kept of the tasks whose MR is not open any more (merged,
// closed, archived, gone), so the maps do not grow with every task.
func (d *Dispatcher) pruneRisk(open map[string]bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for id := range d.autoMerged {
		if !open[id] {
			delete(d.autoMerged, id)
		}
	}
	for id := range d.autoReviewFailed {
		if !open[id] {
			delete(d.autoReviewFailed, id)
		}
	}
}

// autoReviewRounds: the reviewer rounds Mako starts by itself on a task at most; the rest are yours.
const autoReviewRounds = 3

// AutoRiskReview starts the reviewer agent by itself where the policy wants it: on a high risk (with gateHigh), and
// on a low one that may merge by itself. Once per HEAD: a round you skipped, or one already read, is not repeated.
// When it cannot start, the next round tries again, and the journal says why once per HEAD.
func (d *Dispatcher) AutoRiskReview() {
	if d.LimitHold() != nil || d.AuthHold() != nil {
		return
	}
	open := map[string]bool{}
	defer func() { d.pruneRisk(open) }()
	for _, r := range d.Store.All() {
		if !riskOpen(r) {
			continue
		}
		open[r.ID] = true
		if !reviewable(r) || len(r.AgentReviews) >= autoReviewRounds {
			continue
		}
		level, _ := core.RiskOf(r)
		p := d.riskPolicy(r)
		if !(level == core.RiskHigh && p.GateHigh || level == core.RiskLow && p.AutoMergeLow) || Assessing(r) {
			continue
		}
		if d.ReviewStep(r) != "ready" {
			continue
		}
		head := d.selfHead(r)
		if head == "" || slices.ContainsFunc(r.AgentReviews, func(a core.AgentReview) bool { return a.Commit == head }) {
			continue
		}
		if err := d.ReviewNow(r.ID); err != nil {
			d.mu.Lock()
			logged := d.autoReviewFailed[r.ID] == head
			d.autoReviewFailed[r.ID] = head
			d.mu.Unlock()
			if !logged {
				d.Log(r.ID, "reviewer agent not started by itself: "+err.Error())
			}
			continue
		}
		d.Log(r.ID, "reviewer agent started by itself: "+level+" risk")
	}
}

// autoMergeHold: why the task does not merge by itself now, "" when it may: a low risk read at the branch's HEAD,
// the agent's assessment done, the reviewer clean at that HEAD, the MR ready with its CI green.
func (d *Dispatcher) autoMergeHold(r core.Record) string {
	if !riskOpen(r) || !onTerminal(r) {
		return "not an open MR of Mako's agent"
	}
	if !d.riskPolicy(r).AutoMergeLow {
		return "auto-merge is off"
	}
	if level, _ := core.RiskOf(r); level != core.RiskLow {
		return "the risk is not low"
	}
	if r.Assessment == nil || r.Assessment.State != "done" {
		return "not assessed"
	}
	head := d.selfHead(r)
	if head == "" || r.Risk == nil || r.Risk.Commit != head {
		return "the diff is not read at the branch's HEAD"
	}
	if n := len(r.AgentReviews); n == 0 || r.AgentReviews[n-1].Commit != head || r.AgentReviews[n-1].State != "clean" {
		return "the reviewer agent has not passed the branch"
	}
	if !d.MergeReady(r) {
		return "the MR is not ready"
	}
	d.mu.Lock()
	l, ok := d.reviews[r.ID]
	d.mu.Unlock()
	if !ok || l.check == nil || l.check.CI != "success" {
		return "CI is not green"
	}
	return ""
}

// AutoMerge merges the low-risk MRs of repos with autoMergeLow, as you would from the board. A merge the host
// refused is not tried again at the same HEAD; a refusal that may pass by itself (CI running again after a push, a
// look that failed) is tried again in the next round.
func (d *Dispatcher) AutoMerge(ctx context.Context) {
	open := map[string]bool{}
	defer func() { d.pruneRisk(open) }()
	for _, r := range d.Store.All() {
		if riskOpen(r) {
			open[r.ID] = true
		}
		if d.autoMergeHold(r) != "" {
			continue
		}
		head := d.selfHead(r)
		d.mu.Lock()
		refused := d.autoMerged[r.ID] == head
		d.mu.Unlock()
		if refused {
			continue
		}
		err := d.merge(ctx, r, "auto")
		var wait notReady
		switch {
		case errors.As(err, &wait):
			continue
		case err != nil:
			d.mu.Lock()
			d.autoMerged[r.ID] = head
			d.mu.Unlock()
			d.Fault(r.ID, "MR not merged by itself", err)
			continue
		}
		d.Log(r.ID, "MR merged by itself: low risk, CI green, reviewer clean")
	}
}
