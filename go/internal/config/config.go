// Package config reads the same mako.config.json and .env as the TypeScript Mako, keeping what the
// dispatcher uses: trackers and their projects, Orca, limits and notifications.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/fsx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

const (
	DefaultBranchPattern = "{type}/{id}-{title}"
	DefaultProjectsDir   = "~/mako/projects"
)

type Config struct {
	// AgentLanguage: what the agents write in (chat, questions, commits, merge requests, review reports, tracker
	// comments), i18n.EN (default) or i18n.UK. Mako's own interface is always English.
	AgentLanguage string
	// UnknownLanguage: the config's "agentLanguage" Mako does not speak (English is used instead); "" when fine.
	UnknownLanguage string
	Path            string
	Home            string // the folder of mako.config.json and .env
	StateDir        string
	PollIntervalSec int
	WipLimit        int
	// WipAuto: WipLimit is AutoWip, worked out from this Mac's cores and memory, not the number in the config.
	WipAuto bool
	// LimitPausePct: at this share of a Claude usage limit (5 hours or the week) used, the queue waits for it
	// to reset; 0: never.
	LimitPausePct int
	// ResumeAfterLimit: an agent the Claude limit cut goes on by itself once the limit has reset (on unless the
	// settings say off; a chat can still turn it off for itself).
	ResumeAfterLimit bool
	// KeepAwake: the Mac does not fall asleep while an agent works (on unless the settings say off).
	KeepAwake        bool
	ArchiveAfterDays int
	ProjectsDir      string
	BranchPattern    string
	Notifications    Notifications
	Orca             Orca
	Comments         Comments
	Trackers         []*Tracker
	Env              map[string]string

	// Executor: who gets the tasks, "orca" or "terminal" (Mako's own terminal). The end of the prompt (Orca's
	// Instructions, MRBy, SummaryComment) is the same for both.
	Executor string
	Terminal Terminal
	// Reviewer: a second agent reads the task and the branch's diff when you ask for it on the board.
	Reviewer Reviewer
	// SelfCheck: when «Check it yourself» holds the merge until its steps are ticked (or the check is skipped):
	// "advised" (when the assessment advises a check), "always", "off".
	SelfCheck string
	// Estimate: a new task gets the agent's estimate (clarity, size, risk, advice) as it appears.
	Estimate bool
	// EstimateModel: the estimate's size picks the task's routes (else every task is medium).
	EstimateModel bool
	// CodeModel: the model of «Запитати про код», one for every repository.
	CodeModel string
	// Routes: the model and the effort of each phase by the task's size, the defaults with the config's changes;
	// RouteMode: "cost", "balanced" or "quality", a level down, as it is or a level up (a repo's own wins).
	Routes    core.RouteTable
	RouteMode string
	// Prices: USD per million tokens by model family, for the stats' «the same on one model»; the defaults
	// with the config's changes (routes.prices).
	Prices core.Prices
}

// DefaultCodeModel: the model of «Запитати про код» when none is picked.
const DefaultCodeModel = "haiku"

// modelName: what a model's name may be (opus, claude-opus-5-5, claude-sonnet-5-5[1m]): it goes to claude's
// --model, so never an option of its own or several words.
var modelName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._\[\]-]{0,99}$`)

// CheckModel: an error unless name can be a model's name.
func CheckModel(name string) error {
	if !modelName.MatchString(name) {
		return fmt.Errorf("model %q: only letters, digits and . _ - [ ], not starting with a hyphen", name)
	}
	return nil
}

// Reviewer is the agent that reviews another agent's branch with a clean context and writes its remarks; it
// changes no code. Only Mako's own agents get it: you hand the remarks to their chat from the board.
type Reviewer struct {
	Model string // "" — the agents' model
	// Comments: where its remarks and the agent's answers on them go as comments: "issue", "mr", "both";
	// "" — nowhere, they stay on the board. «mr» with no MR yet goes to the issue.
	Comments string
	// LessonTasks: a kind of remark the reviewer made on this many of a repo's tasks makes Mako offer a rule
	// for its CLAUDE.md; LessonDays: how far back the tasks (the archived ones too) are looked at.
	LessonTasks int
	LessonDays  int
}

// The defaults of Reviewer.LessonTasks and Reviewer.LessonDays.
const (
	DefaultLessonTasks = 3
	DefaultLessonDays  = 90
)

// The values of Config.SelfCheck.
const (
	SelfCheckAdvised = "advised"
	SelfCheckAlways  = "always"
	SelfCheckOff     = "off"
)

// selfCheckMode: the setting's value, "advised" for anything else.
func selfCheckMode(s string) string {
	switch s = strings.TrimSpace(s); s {
	case SelfCheckAlways, SelfCheckOff:
		return s
	}
	return SelfCheckAdvised
}

// reviewComments: the setting's value, "" for anything else.
func reviewComments(s string) string {
	switch s = strings.TrimSpace(s); s {
	case "issue", "mr", "both":
		return s
	}
	return ""
}

// Notifications' AutoTakeOnly.
const (
	AutoTakeAll        = "all"
	AutoTakeSmallClear = "smallClear"
	AutoTakeSmall      = "small"
	// AutoTakeLikely: clear, and the history of tasks like it says it will likely go through (core.Outlook):
	// its first pass at AutoTakeFirstPass or more, its expected cost at AutoTakeMaxCost or less.
	AutoTakeLikely = "likely"
)

// DefaultAutoTakeFirstPass: the first pass, in percent, the history must give a task for «likely» to take it.
const DefaultAutoTakeFirstPass = 70

// Comments Mako leaves in the issue, written as the developer: {branch} {taskId} {title} {url} {mr} are filled
// in; "-" leaves no comment.
type Comments struct {
	OnStart  string // when the task goes to Orca
	OnReview string // when its MR appears
}

// The built-in comments, by language.
var (
	DefaultCommentOnStart  = Localized{EN: "Took the task. Branch {branch}. I'll open an MR once it's ready.", UK: "Взяв задачу в роботу. Гілка {branch}. Як буде готово, відкрию MR."} // cyrillic-ok: Ukrainian default of the uk slot
	DefaultCommentOnReview = Localized{EN: "Opened an MR: {mr}", UK: "Відкрив MR: {mr}"}                                                                                                // cyrillic-ok: Ukrainian default of the uk slot
)

type Notifications struct {
	Enabled  bool
	AutoTake bool
	// AutoTakeOnly: which new tasks AutoTake takes, by their estimate: "all", "smallClear" (small, clear and
	// with no questions about the description), "small" or "likely" (clear, and likely to go through by the
	// history of tasks like it). In all but "all" a task with no estimate stays.
	AutoTakeOnly string
	// AutoTakeFirstPass: for "likely", the first pass the history must give, in percent (1–100);
	// AutoTakeMaxCost: the most it may be expected to cost, USD (0: no ceiling).
	AutoTakeFirstPass int
	AutoTakeMaxCost   float64
	SnoozeMin         int
	Port              int
	// MorningMin: when the morning digest goes out, in minutes after midnight; -1: never.
	MorningMin int
	// AwayHours: the board not seen this long shows what happened meanwhile.
	AwayHours int
	// Off: the kinds of notifications turned off (notify.NewTask, notify.Finished…); the rest are shown.
	Off map[string]bool
}

// Muted says the kind of notifications is turned off.
func (n Notifications) Muted(kind string) bool { return n.Off[kind] }

const DefaultMorningDigest = "09:00"

// Orca is the executor the dispatcher hands every task to.
type Orca struct {
	Command      string
	Agent        string
	Setup        string // "run", "skip", "inherit"
	LinkIssue    bool
	Instructions string // replaces the default end of the prompt; {branch} {baseBranch} {taskId} {title} {url} {closingRef}
	MRBy         string // "agent" (default) or "me"
	// SummaryComment: when the MR appears, the summary of the agent's work also goes to the issue as a comment.
	SummaryComment bool
	ExtraArgs      []string
}

// Terminal is Mako's own terminal: the agent runs in a pseudo-terminal the board shows.
type Terminal struct {
	Command string // "claude" when empty
	// Model, Effort: what the agent's run starts with; the routes fill them for each run, the config does not.
	Model, Effort string
	// PermissionMode: how much Claude may do without asking: "auto" (default, also with AutoTake and AutoFix),
	// "acceptEdits", "default", "bypassPermissions".
	PermissionMode string
	AllowedTools   []string // used without asking: "Bash(git:*)", "WebFetch"
	// SetupCommand prepares every new worktree unless its repo has its own setup; "" guesses from the
	// lockfile, "-" runs nothing.
	SetupCommand string
	// View: how the agent is shown, "chat" (default) or "terminal".
	View string
	// AutoFix: the agent is asked by itself to fix its MR's failed CI, conflicts and new review comments.
	AutoFix bool
	// NoPushConfirm: while tasks are taken (AutoTake) or MRs fixed (AutoFix) by themselves, even a critical push
	// (beyond the task's branch, or in a task with an outsider's text) goes without asking you («confirmPush»:
	// false). Off by default: such a push waits for you; the task's own branch and its MR never do.
	NoPushConfirm bool
	// AutoRebase: how Mako brings the branch of an MR in review up to its base branch by itself, "rebase" or
	// "merge"; "" never. Conflicts go to the agent.
	AutoRebase string
	// RebaseEveryMin: how often a branch is looked at for that, in minutes (30 when not set).
	RebaseEveryMin int
	// PlanFirst: when the agent starts in Claude Code's plan mode, exploring and writing a plan you approve
	// before it changes anything: "never" (default), "big" (a task labelled plan or with a long description),
	// "always".
	PlanFirst string
	// CheckTimeoutSec: how long one of a repo's checks before the MR may run (600 by default); CheckAttempts:
	// failed runs of the checks before the task needs your attention (3 by default).
	CheckTimeoutSec int
	CheckAttempts   int
}

type Transitions struct {
	OnStart, OnSuccess, OnFailure, OnMerged *string // nil: not set (Mako guesses where it can); "": leave alone
}

type Tracker struct {
	Type            string // "gitlab", "github", "jira"
	ID              string
	BaseURL         string // GitLab, Jira
	APIURL          string // GitHub Enterprise
	JQL             string
	Trigger         string // the label a task needs; "" with TriggerOff: any of my tasks
	TriggerOff      bool
	Assignee        string
	AllProjects     bool
	AssignOnTake    bool
	SkipOthersTasks bool
	// Paused: no searching this tracker for new tasks (its VPN is off, its work is put aside); the others go on.
	Paused bool
	// ReviewRequests: the MRs where I am a reviewer show on the board (GitLab, GitHub); on unless turned off.
	ReviewRequests bool
	Transitions    Transitions
	Projects       map[string]*Project

	// projMu guards Projects against the one writer that runs while a poll reads it (ProjectKeys, HasProject,
	// AddProject). The other readers hold the dispatcher's lock, which the writer holds too.
	projMu sync.RWMutex
}

// HasProject: the project is in the config. Safe against a concurrent AddProject.
func (t *Tracker) HasProject(key string) bool {
	t.projMu.RLock()
	defer t.projMu.RUnlock()
	return t.Projects[key] != nil
}

// ProjectKeys: a snapshot of the projects' keys, safe to range over while AddProject runs.
func (t *Tracker) ProjectKeys() []string {
	t.projMu.RLock()
	defer t.projMu.RUnlock()
	keys := make([]string, 0, len(t.Projects))
	for k := range t.Projects {
		keys = append(keys, k)
	}
	return keys
}

// AddProject adds p under key.
func (t *Tracker) AddProject(key string, p *Project) {
	t.projMu.Lock()
	defer t.projMu.Unlock()
	if t.Projects == nil {
		t.Projects = map[string]*Project{}
	}
	t.Projects[key] = p
}

type Project struct {
	Key   string
	Repos map[string]*Repo
	// Default repo when a project has several; with one it is that one.
	Default string
	// Watch: what of the project's default branch Mako keeps an eye on, making a task when it goes wrong.
	Watch Watch
	// Routines: the tasks Mako writes in the project by a schedule.
	Routines []Routine
}

// Watch: the project's default branch's CI and its security alerts, each making a task assigned to me when they
// go wrong; AutoTake takes those tasks at once. MaxCost: the most Mako's agent may spend on a task the watch or a
// routine wrote, USD; it is never off.
type Watch struct {
	CI       bool
	Security bool
	AutoTake bool
	MaxCost  float64
}

// DefaultWatchMaxCost: the ceiling of a task Mako wrote itself when the config sets none, USD.
const DefaultWatchMaxCost = 5.0

// Routine: a task Mako writes by Cron, when the last one with its title is closed.
type Routine struct {
	Cron     core.Cron
	Title    string
	Body     string
	AutoTake bool
}

type Repo struct {
	Name          string
	Path          string
	CloneURL      string
	BaseBranch    string // "" until known: the remote's default
	BranchPattern string
	WorktreesDir  string
	// Setup: what prepares a new worktree of Mako's terminal ("pnpm install"); "-": nothing.
	Setup string
	// Checks: the commands that must pass in the agent's worktree before Mako opens its MR ("go vet ./...",
	// "go test ./..."); none: the agent opens the MR itself, as before.
	Checks []string
	// Try: what runs a task's branch for «Try this build» in its worktree, listening on $PORT ("npm run dev --
	// --port $PORT"); "": no such button (Mako's own repository has it built in).
	Try string
	// RouteMode: the routes' mode for this repo's tasks; "" the global one.
	RouteMode string
	// RiskPaths: the paths that make a change risky, each with its level ("high": always high; "block": the
	// change does not merge and the agent is told not to touch it); Risk: the thresholds and what each level does.
	RiskPaths []RiskPath
	Risk      RiskPolicy
}

// RiskPath: a pattern of paths in the repo ("migrations/**", "**/auth/**", "*.env*") and the level a change that
// touches it gets: core.RiskMedium, core.RiskHigh or core.RiskBlock.
type RiskPath struct {
	Pattern string `json:"pattern"`
	Level   string `json:"level"`
}

// RiskPolicy: the repo's policy by the change's risk. AutoMergeLow: Mako merges a low-risk MR by itself once its
// CI is green and the reviewer agent found nothing to fix (off by default). GateHigh: a high-risk MR gets the
// reviewer agent by itself and waits for «Check it yourself» (on by default). MaxLines, MaxFiles: a diff above them
// is medium, above three times them high.
type RiskPolicy struct {
	AutoMergeLow bool
	GateHigh     bool
	MaxLines     int
	MaxFiles     int
}

// The default thresholds of RiskPolicy.
const (
	DefaultRiskMaxLines = 400
	DefaultRiskMaxFiles = 15
)

// rawRisk: RiskPolicy in the JSON.
type rawRisk struct {
	AutoMergeLow bool  `json:"autoMergeLow"`
	GateHigh     *bool `json:"gateHigh"`
	MaxLines     int   `json:"maxLines"`
	MaxFiles     int   `json:"maxFiles"`
}

// riskPolicy: the policy with the defaults for thresholds not set (0 or less).
func riskPolicy(r *rawRisk) RiskPolicy {
	p := RiskPolicy{GateHigh: true, MaxLines: DefaultRiskMaxLines, MaxFiles: DefaultRiskMaxFiles}
	if r == nil {
		return p
	}
	p.AutoMergeLow = r.AutoMergeLow
	if r.GateHigh != nil {
		p.GateHigh = *r.GateHigh
	}
	if r.MaxLines > 0 {
		p.MaxLines = r.MaxLines
	}
	if r.MaxFiles > 0 {
		p.MaxFiles = r.MaxFiles
	}
	return p
}

// riskPaths: the patterns set, without the empty ones; a level Mako does not know (or none) is high, low is no
// reason to name a path.
func riskPaths(list []RiskPath) []RiskPath {
	var out []RiskPath
	for _, p := range list {
		pattern := strings.TrimSpace(p.Pattern)
		if pattern == "" {
			continue
		}
		level := strings.TrimSpace(strings.ToLower(p.Level))
		if level != core.RiskMedium && level != core.RiskBlock {
			level = core.RiskHigh
		}
		out = append(out, RiskPath{Pattern: pattern, Level: level})
	}
	return out
}

// ErrMissing: there is no mako.config.json yet (first start).
var ErrMissing = errors.New("the config is missing: Mako is not set up yet")

// Load reads mako.config.json and .env from home. When the file is missing or wrong it still returns a config
// with the defaults, so Mako starts and the settings page can fix it; the error says what is wrong.
func Load(home string) (*Config, error) {
	path := filepath.Join(home, "mako.config.json")
	env := readEnv(filepath.Join(home, ".env"))
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		err = ErrMissing
	}
	var c *Config
	if err == nil {
		c, err = Parse(data, env)
	}
	if err != nil {
		c, _ = Parse([]byte("{}"), env)
	}
	c.Path, c.Home = path, home
	// As the TypeScript Mako: .mako next to the config, a relative stateDir from there too.
	if c.StateDir == "" {
		c.StateDir = ".mako"
	}
	if !filepath.IsAbs(c.StateDir) {
		c.StateDir = filepath.Join(home, c.StateDir)
	}
	return c, err
}

// CheckEnv refuses what .env cannot hold: a value on more than one line.
func CheckEnv(updates map[string]string) error {
	keys := slices.Sorted(maps.Keys(updates))
	for _, k := range keys {
		if strings.ContainsAny(updates[k], "\r\n") {
			return fmt.Errorf("%s: a value cannot span lines", k)
		}
	}
	return nil
}

// SaveEnv sets keys in home/.env, keeping its other lines; an empty value removes the key. The file is replaced
// at once and only its owner may read it, as it holds the tokens.
func SaveEnv(home string, updates map[string]string) error {
	if err := CheckEnv(updates); err != nil {
		return err
	}
	path := filepath.Join(home, ".env")
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var lines []string
	done := map[string]bool{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		k, _, ok := strings.Cut(strings.TrimSpace(line), "=")
		k = strings.TrimSpace(k)
		if v, change := updates[k]; ok && change && !strings.HasPrefix(k, "#") {
			done[k] = true
			if v == "" {
				continue
			}
			line = k + "=" + encodeEnv(v)
		}
		if line != "" || len(lines) > 0 {
			lines = append(lines, line)
		}
	}
	keys := make([]string, 0, len(updates))
	for k := range updates {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !done[k] && updates[k] != "" {
			lines = append(lines, k+"="+encodeEnv(updates[k]))
		}
	}
	return fsx.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

var plainEnv = regexp.MustCompile(`^[A-Za-z0-9_./:@+=,-]*$`)

// encodeEnv writes a value so that readEnv gives it back as it was: a plain one as it is, any other (spaces, #,
// quotes) in double quotes with \ and " escaped.
func encodeEnv(v string) string {
	if plainEnv.MatchString(v) {
		return v
	}
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v) + `"`
}

// decodeEnv reads a value as encodeEnv writes it; a value in single quotes is taken as it is between them, one
// without quotes as it stands.
func decodeEnv(v string) string {
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		return strings.NewReplacer(`\\`, `\`, `\"`, `"`).Replace(v[1 : len(v)-1])
	}
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	return v
}

// The bounds of the numbers in the settings. Check refuses a number outside them; Parse, which reads files written
// before the bounds, takes the default for it instead, so an old config still loads.
const (
	MinPollSec     = 5 // below this the polling loop would hammer the trackers and the CPU
	MaxPollSec     = 86400
	MaxWip         = 100
	MaxArchiveDays = 3650
	MaxSnoozeMin   = 10080 // a week
)

// Check refuses a config whose numbers are out of range, with what to enter instead. The settings page runs it
// before saving; Parse does not fail on these, so a file already on disk keeps loading.
func Check(data []byte) error {
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("mako.config.json: %w", err)
	}
	for _, f := range []struct {
		name     string
		v        *int
		min, max int
		hint     string
	}{
		{"Look for tasks every, s", raw.PollIntervalSec, MinPollSec, MaxPollSec, ""},
		{"Tasks in Orca at once (WIP)", raw.WipLimit, 0, MaxWip, "; leave it empty for no limit"},
		{"Pause the queue near the Claude limit, %", raw.LimitPausePct, 0, 100, "; 0 turns the pause off"},
		{"Archive finished ones after, days", raw.ArchiveAfterDays, 0, MaxArchiveDays, "; 0 means don't archive"},
		{"«Later» without a choice, min", raw.Notifications.SnoozeMin, 1, MaxSnoozeMin, ""},
	} {
		if f.v != nil && (*f.v < f.min || *f.v > f.max) {
			return fmt.Errorf("%s: %d is not allowed, enter a number from %d to %d%s", f.name, *f.v, f.min, f.max, f.hint)
		}
	}
	return nil
}

// Parse turns the raw config into what the dispatcher needs and fills the defaults.
func Parse(data []byte, env map[string]string) (*Config, error) {
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mako.config.json: %w", err)
	}
	lang := strings.TrimSpace(raw.AgentLanguage)
	if lang == "" {
		lang = strings.TrimSpace(raw.Language) // an older config: "language" is what became "agentLanguage"
	}
	if lang == "" {
		lang = i18n.Default
	}
	unknown := ""
	if !i18n.Supported(lang) {
		unknown, lang = lang, i18n.Default // a wrong language does not cost the trackers: English, with a problem
	}
	c := &Config{
		AgentLanguage:    lang,
		UnknownLanguage:  unknown,
		StateDir:         Expand(raw.StateDir),
		PollIntervalSec:  inRange(orInt(raw.PollIntervalSec, 60), MinPollSec, MaxPollSec, 60),
		WipLimit:         inRange(orInt(raw.WipLimit, 0), 0, MaxWip, 0),
		WipAuto:          raw.WipAuto,
		LimitPausePct:    inRange(orInt(raw.LimitPausePct, 85), 0, 100, 85),
		ArchiveAfterDays: inRange(orInt(raw.ArchiveAfterDays, 2), 0, MaxArchiveDays, 2),
		KeepAwake:        raw.KeepAwake == nil || *raw.KeepAwake,
		ResumeAfterLimit: raw.ResumeAfterLimit == nil || *raw.ResumeAfterLimit,
		ProjectsDir:      Expand(orStr(raw.ProjectsDir, DefaultProjectsDir)),
		BranchPattern:    orStr(raw.BranchPattern, DefaultBranchPattern),
		Env:              env,
		Notifications: Notifications{
			Enabled:           raw.Notifications.Enabled == nil || *raw.Notifications.Enabled,
			AutoTake:          raw.Notifications.AutoTake != nil && *raw.Notifications.AutoTake,
			AutoTakeOnly:      autoTakeOnly(raw.Notifications.AutoTakeOnly),
			AutoTakeFirstPass: inRange(orInt(raw.Notifications.AutoTakeFirstPass, DefaultAutoTakeFirstPass), 1, 100, DefaultAutoTakeFirstPass),
			AutoTakeMaxCost:   maxCost(raw.Notifications.AutoTakeMaxCost),
			SnoozeMin:         inRange(orInt(raw.Notifications.SnoozeMin, 60), 1, MaxSnoozeMin, 60),
			Port:              orInt(raw.Notifications.InboxPort, 7777),
			AwayHours:         orInt(raw.Notifications.AwayHours, 4),
			Off:               map[string]bool{},
		},
		Orca: Orca{Agent: "claude", Setup: "inherit", LinkIssue: true, MRBy: "agent"},
		Reviewer: Reviewer{Model: strings.TrimSpace(raw.AgentReview.Model), Comments: reviewComments(raw.AgentReview.Comments),
			LessonTasks: atLeast(orInt(raw.AgentReview.LessonTasks, DefaultLessonTasks), 2, DefaultLessonTasks),
			LessonDays:  atLeast(orInt(raw.AgentReview.LessonDays, DefaultLessonDays), 1, DefaultLessonDays)},
		SelfCheck:     selfCheckMode(raw.SelfCheck),
		Estimate:      raw.Estimate.Enabled == nil || *raw.Estimate.Enabled,
		EstimateModel: raw.Estimate.UseModel == nil || *raw.Estimate.UseModel,
		CodeModel:     orStr(strings.TrimSpace(raw.CodeAsk.Model), DefaultCodeModel),
		Routes:        routes(raw.Routes.Table),
		RouteMode:     routeMode(raw.Routes.Mode),
		Prices:        prices(raw.Routes.Prices),
		Comments: Comments{
			OnStart:  orStr(raw.Comments.OnStart.In(lang), DefaultCommentOnStart.In(lang)),
			OnReview: orStr(raw.Comments.OnReview.In(lang), DefaultCommentOnReview.In(lang)),
		},
	}
	if c.WipAuto {
		c.WipLimit = AutoWip()
	}
	// Mako runs the agent itself unless Orca is the default executor, or the only one there is.
	c.Executor, c.Terminal = "terminal", Terminal{PermissionMode: "auto"}
	if e, ok := raw.Executors[raw.DefaultExecutor]; ok {
		if e.Type == "orca" {
			c.Executor = "orca"
		}
	} else if raw.has("orca") && !raw.has("terminal") {
		c.Executor = "orca"
	}
	for _, name := range raw.executorOrder() {
		if e := raw.Executors[name]; e.Type == "terminal" {
			c.Terminal = Terminal{Command: e.Command, PermissionMode: orStr(e.PermissionMode, "auto"), AllowedTools: e.AllowedTools, SetupCommand: e.SetupCommand, View: e.View, AutoFix: e.AutoFix, NoPushConfirm: e.ConfirmPush != nil && !*e.ConfirmPush, AutoRebase: autoRebase(e.AutoRebase), RebaseEveryMin: e.RebaseEveryMin, PlanFirst: e.PlanFirst,
				CheckTimeoutSec: e.CheckTimeoutSec, CheckAttempts: e.CheckAttempts}
			break
		}
	}
	// The dispatcher's Orca: the first orca executor, else the defaults.
	for _, name := range raw.executorOrder() {
		e := raw.Executors[name]
		if e.Type != "orca" {
			continue
		}
		c.Orca.Command = e.Command
		c.Orca.Agent = orStr(e.Agent, "claude")
		c.Orca.Setup = orStr(e.Setup, "inherit")
		c.Orca.LinkIssue = e.LinkIssue == nil || *e.LinkIssue
		c.Orca.Instructions = e.Instructions.Either(lang)
		c.Orca.MRBy = orStr(e.MRBy, "agent")
		c.Orca.SummaryComment = e.SummaryComment
		c.Orca.ExtraArgs = e.ExtraArgs
		break
	}
	if err := CheckModel(c.CodeModel); err != nil {
		return nil, fmt.Errorf("\"Code questions\": %w", err)
	}
	morning, err := clock(orStr(strings.TrimSpace(raw.Notifications.MorningDigest), DefaultMorningDigest))
	if err != nil {
		return nil, fmt.Errorf("morning digest: %w", err)
	}
	c.Notifications.MorningMin = morning
	for kind, on := range raw.Notifications.Events {
		if !on {
			c.Notifications.Off[kind] = true
		}
	}
	for i, rt := range raw.Trackers {
		t, err := c.tracker(rt)
		if err != nil {
			return nil, fmt.Errorf("tracker %d: %w", i+1, err)
		}
		c.Trackers = append(c.Trackers, t)
	}
	return c, nil
}

func (c *Config) tracker(rt rawTracker) (*Tracker, error) {
	t := &Tracker{
		Type:            rt.Type,
		ID:              orStr(rt.ID, rt.Type),
		BaseURL:         strings.TrimRight(rt.BaseURL, "/"),
		APIURL:          strings.TrimRight(rt.APIURL, "/"),
		JQL:             rt.JQL,
		Assignee:        strings.TrimPrefix(strings.TrimSpace(rt.Assignee), "@"),
		AllProjects:     rt.AllProjects,
		AssignOnTake:    rt.AssignOnTake == nil || *rt.AssignOnTake,
		SkipOthersTasks: rt.SkipOthersTasks == nil || *rt.SkipOthersTasks,
		Paused:          rt.Paused,
		ReviewRequests:  rt.Type != "jira" && (rt.ReviewRequests == nil || *rt.ReviewRequests),
		Projects:        map[string]*Project{},
	}
	switch t.Type {
	case "gitlab", "github", "jira":
	default:
		return nil, fmt.Errorf("unknown tracker type %q", t.Type)
	}
	// labels.trigger: missing → "claude-agent"; null → no label needed (all of my tasks).
	if rt.Labels != nil {
		if raw, ok := rt.Labels["trigger"]; ok {
			if string(raw) == "null" {
				t.TriggerOff = true
			} else {
				_ = json.Unmarshal(raw, &t.Trigger)
			}
		}
	}
	if !t.TriggerOff && t.Trigger == "" && t.Type != "jira" {
		t.Trigger = "claude-agent"
	}
	if t.Type == "gitlab" && t.BaseURL == "" {
		t.BaseURL = "https://gitlab.com"
	}
	if t.Type == "jira" && t.BaseURL == "" {
		t.BaseURL = strings.TrimRight(c.Env["JIRA_BASE_URL"], "/")
	}
	t.Transitions = Transitions{
		OnStart: transition(rt.Transitions, "onStart"), OnSuccess: transition(rt.Transitions, "onSuccess"),
		OnFailure: transition(rt.Transitions, "onFailure"), OnMerged: transition(rt.Transitions, "onMerged"),
	}
	for key, rp := range rt.Projects {
		var p rawProject
		if err := json.Unmarshal(rp, &p); err != nil {
			return nil, fmt.Errorf("project %s: %w", key, err)
		}
		project := &Project{Key: key, Repos: map[string]*Repo{}, Default: p.Default}
		project.Watch.MaxCost = DefaultWatchMaxCost
		if p.Watch != nil {
			project.Watch = Watch{CI: p.Watch.CI, Security: p.Watch.Security, AutoTake: p.Watch.AutoTake, MaxCost: DefaultWatchMaxCost}
			if p.Watch.MaxCost != nil && *p.Watch.MaxCost > 0 {
				project.Watch.MaxCost = *p.Watch.MaxCost
			}
			if (p.Watch.CI || p.Watch.Security) && len(p.Repos) > 1 {
				// The CI and the alerts are a repository's: with several, the project's key names none of them.
				return nil, fmt.Errorf("project %s: watching the CI and the security alerts needs a project with one repository; set it on that repository's own project", key)
			}
		}
		for i, r := range p.Routines {
			cron, err := core.ParseCron(r.Cron)
			if err != nil {
				return nil, fmt.Errorf("project %s: routine %d: %w", key, i+1, err)
			}
			title := strings.TrimSpace(r.Title)
			if title == "" {
				return nil, fmt.Errorf("project %s: routine %d: the title is missing", key, i+1)
			}
			project.Routines = append(project.Routines, Routine{Cron: cron, Title: title, Body: strings.TrimSpace(r.Body), AutoTake: r.AutoTake})
		}
		repos := p.Repos
		several := len(repos) > 1
		if len(repos) == 0 {
			// Short form: the project itself is its one repo.
			var own rawRepo
			_ = json.Unmarshal(rp, &own)
			repos = map[string]rawRepo{lastPart(key): own}
		}
		for name, r := range repos {
			clone := ""
			if !several {
				clone = DefaultCloneURL(t, key)
			}
			project.Repos[name] = c.NewRepo(name, r.RepoPath, orStr(r.CloneURL, clone), r.BaseBranch, r.BranchPattern)
			project.Repos[name].Setup = r.Setup
			project.Repos[name].Checks = cleanChecks(r.Checks)
			project.Repos[name].Try = strings.TrimSpace(r.Try)
			if m := strings.TrimSpace(r.RouteMode); slices.Contains(core.Modes, m) {
				project.Repos[name].RouteMode = m // an unknown one: the global mode
			}
			project.Repos[name].RiskPaths = riskPaths(r.RiskPaths)
			project.Repos[name].Risk = riskPolicy(r.Risk)
		}
		t.Projects[key] = project
	}
	if len(t.Projects) == 0 && !t.AllProjects {
		return nil, fmt.Errorf("%s: add a project or turn on \"My tasks from all projects\"", t.ID)
	}
	return t, nil
}

// NewRepo is one local repository with the defaults: ~/mako/projects/<name>, the general branch pattern.
func (c *Config) NewRepo(name, path, cloneURL, base, pattern string) *Repo {
	p := Expand(path)
	if p == "" {
		p = filepath.Join(c.ProjectsDir, name)
	}
	return &Repo{
		Name:          name,
		Path:          p,
		CloneURL:      cloneURL,
		BaseBranch:    base,
		BranchPattern: orStr(pattern, c.BranchPattern),
		WorktreesDir:  filepath.Join(filepath.Dir(p), ".worktrees", name),
		Risk:          riskPolicy(nil),
	}
}

// DefaultCloneURL is the SSH address of a GitLab/GitHub project ("group/app"), the way developers clone.
func DefaultCloneURL(t *Tracker, project string) string {
	if !strings.Contains(project, "/") {
		return "" // a Jira key says nothing about where the code is
	}
	host := ""
	switch t.Type {
	case "gitlab":
		if u, err := url.Parse(t.BaseURL); err == nil {
			host = u.Hostname()
		}
	case "github":
		host = "github.com"
		if t.APIURL != "" {
			if u, err := url.Parse(t.APIURL); err == nil {
				host = strings.TrimPrefix(u.Hostname(), "api.")
			}
		}
	}
	if host == "" {
		return ""
	}
	return fmt.Sprintf("git@%s:%s.git", host, project)
}

// Token of a tracker from the environment (.env, then the process).
func (c *Config) Get(name string) string {
	if v := c.Env[name]; v != "" {
		return v
	}
	return os.Getenv(name)
}

// Expand turns a leading ~/ into the home folder.
func Expand(p string) string {
	if strings.HasPrefix(p, "~/") {
		if h, err := os.UserHomeDir(); err == nil {
			return filepath.Join(h, p[2:])
		}
	}
	return p
}

func readEnv(path string) map[string]string {
	env := map[string]string{}
	f, err := os.Open(path)
	if err != nil {
		return env
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		env[strings.TrimSpace(k)] = decodeEnv(v)
	}
	return env
}

// transition: absent → nil (Mako guesses where it can), null → "" (leave the status alone), a name → it.
func transition(m map[string]json.RawMessage, key string) *string {
	raw, ok := m[key]
	if !ok {
		return nil
	}
	name := ""
	if string(raw) != "null" {
		_ = json.Unmarshal(raw, &name)
	}
	return &name
}

func lastPart(key string) string {
	if i := strings.LastIndex(key, "/"); i >= 0 {
		return key[i+1:]
	}
	return key
}

func orStr(v, def string) string {
	if strings.TrimSpace(v) == "" {
		return def
	}
	return strings.TrimSpace(v)
}

func orInt(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// inRange: v, or def when v is outside min..max.
func inRange(v, min, max, def int) int {
	if v < min || v > max {
		return def
	}
	return v
}

// atLeast: v, or def when v is below min.
func atLeast(v, min, def int) int {
	if v < min {
		return def
	}
	return v
}

type rawConfig struct {
	AgentLanguage    string                 `json:"agentLanguage"`
	Language         string                 `json:"language"` // older configs: read as agentLanguage
	StateDir         string                 `json:"stateDir"`
	PollIntervalSec  *int                   `json:"pollIntervalSec"`
	WipLimit         *int                   `json:"wipLimit"`
	WipAuto          bool                   `json:"wipAuto"`
	LimitPausePct    *int                   `json:"limitPausePct"`
	ResumeAfterLimit *bool                  `json:"resumeAfterLimit"`
	ArchiveAfterDays *int                   `json:"archiveAfterDays"`
	KeepAwake        *bool                  `json:"keepAwake"`
	ProjectsDir      string                 `json:"projectsDir"`
	BranchPattern    string                 `json:"branchPattern"`
	Notifications    rawNotifications       `json:"notifications"`
	Executors        map[string]rawExecutor `json:"executors"`
	Comments         struct {
		OnStart  Localized `json:"onStart"`
		OnReview Localized `json:"onReview"`
	} `json:"comments"`
	AgentReview struct {
		Model       string `json:"model"`
		Comments    string `json:"comments"`
		LessonTasks *int   `json:"lessonTasks"`
		LessonDays  *int   `json:"lessonDays"`
	} `json:"agentReview"`
	SelfCheck string `json:"selfCheck"`
	Estimate  struct {
		Enabled  *bool `json:"enabled"`
		UseModel *bool `json:"useModel"`
	} `json:"estimate"`
	CodeAsk struct {
		Model string `json:"model"`
	} `json:"codeAsk"`
	Routes struct {
		Mode  string                                               `json:"mode"`
		Table map[string]map[string]struct{ Model, Effort string } `json:"table"`
		// Prices: by model family, each kind optional: {"opus": {"input": 5, "output": 25}}.
		Prices map[string]rawPrice `json:"prices"`
	} `json:"routes"`
	DefaultExecutor string       `json:"defaultExecutor"`
	Trackers        []rawTracker `json:"trackers"`
}

// autoTakeOnly: Notifications' AutoTakeOnly, "all" for anything else.
func autoTakeOnly(s string) string {
	switch s = strings.TrimSpace(s); s {
	case AutoTakeSmallClear, AutoTakeSmall, AutoTakeLikely:
		return s
	}
	return AutoTakeAll
}

// maxCost: a ceiling in USD; 0 (none) for none, zero or less.
func maxCost(v *float64) float64 {
	if v == nil || *v <= 0 {
		return 0
	}
	return *v
}

// executorOrder: the default executor first, so a config with several Orcas uses the chosen one.
func (r rawConfig) executorOrder() []string {
	names := []string{}
	if _, ok := r.Executors[r.DefaultExecutor]; ok {
		names = append(names, r.DefaultExecutor)
	}
	for n := range r.Executors {
		if n != r.DefaultExecutor {
			names = append(names, n)
		}
	}
	return names
}

type rawNotifications struct {
	Enabled  *bool `json:"enabled"`
	AutoTake *bool `json:"autoTake"`
	// AutoTakeOnly: "all" (default), "smallClear", "small" or "likely".
	AutoTakeOnly string `json:"autoTakeOnly"`
	// AutoTakeFirstPass: for "likely", percent (default 70); AutoTakeMaxCost: USD, none or 0 for no ceiling.
	AutoTakeFirstPass *int     `json:"autoTakeFirstPass"`
	AutoTakeMaxCost   *float64 `json:"autoTakeMaxCost"`
	SnoozeMin         *int     `json:"snoozeMin"`
	InboxPort         *int     `json:"inboxPort"`
	// MorningDigest: "09:00", "-" for none.
	MorningDigest string `json:"morningDigest"`
	AwayHours     *int   `json:"awayHours"`
	// Events: which kinds of notifications to show, {"newTask": false}; a kind not named is shown.
	Events map[string]bool `json:"events"`
}

// clock reads "HH:MM" as minutes after midnight; "-" is -1.
func clock(s string) (int, error) {
	if s == "-" {
		return -1, nil
	}
	var h, m int
	if n, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || n != 2 || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("%q: expected a time HH:MM, for example 09:00, or \"-\"", s)
	}
	return h*60 + m, nil
}

// has: some executor is of type t.
func (r *rawConfig) has(t string) bool {
	for _, e := range r.Executors {
		if e.Type == t {
			return true
		}
	}
	return false
}

type rawExecutor struct {
	Type            string    `json:"type"`
	Command         string    `json:"command"`
	Agent           string    `json:"agent"`
	Setup           string    `json:"setup"`
	LinkIssue       *bool     `json:"linkIssue"`
	Instructions    Localized `json:"instructions"`
	MRBy            string    `json:"mrBy"`
	SummaryComment  bool      `json:"summaryComment"`
	ExtraArgs       []string  `json:"extraArgs"`
	PermissionMode  string    `json:"permissionMode"`
	AllowedTools    []string  `json:"allowedTools"`
	SetupCommand    string    `json:"setupCommand"`
	View            string    `json:"view"`
	AutoFix         bool      `json:"autoFix"`
	ConfirmPush     *bool     `json:"confirmPush"`
	AutoRebase      string    `json:"autoRebase"`
	RebaseEveryMin  int       `json:"rebaseEveryMin"`
	PlanFirst       string    `json:"planFirst"`
	CheckTimeoutSec int       `json:"checkTimeoutSec"`
	CheckAttempts   int       `json:"checkAttempts"`
}

type rawTracker struct {
	Type            string                     `json:"type"`
	ID              string                     `json:"id"`
	BaseURL         string                     `json:"baseUrl"`
	APIURL          string                     `json:"apiUrl"`
	JQL             string                     `json:"jql"`
	Labels          map[string]json.RawMessage `json:"labels"`
	Assignee        string                     `json:"assignee"`
	AllProjects     bool                       `json:"allProjects"`
	AssignOnTake    *bool                      `json:"assignOnTake"`
	SkipOthersTasks *bool                      `json:"skipOthersTasks"`
	Paused          bool                       `json:"paused"`
	ReviewRequests  *bool                      `json:"reviewRequests"`
	Transitions     map[string]json.RawMessage `json:"transitions"`
	Projects        map[string]json.RawMessage `json:"projects"`
}

type rawProject struct {
	Default  string             `json:"default"`
	Repos    map[string]rawRepo `json:"repos"`
	Watch    *rawWatch          `json:"watch"`
	Routines []rawRoutine       `json:"routines"`
}

type rawWatch struct {
	CI       bool     `json:"ci"`
	Security bool     `json:"security"`
	AutoTake bool     `json:"autoTake"`
	MaxCost  *float64 `json:"maxCost"`
}

type rawRoutine struct {
	Cron     string `json:"cron"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	AutoTake bool   `json:"autoTake"`
}

type rawRepo struct {
	RepoPath      string     `json:"repoPath"`
	CloneURL      string     `json:"cloneUrl"`
	BaseBranch    string     `json:"baseBranch"`
	Setup         string     `json:"setup"`
	BranchPattern string     `json:"branchPattern"`
	Checks        []string   `json:"checks"`
	Try           string     `json:"try"`
	RouteMode     string     `json:"routeMode"`
	RiskPaths     []RiskPath `json:"riskPaths"`
	Risk          *rawRisk   `json:"risk"`
}

// routes: the default routes with the config's changes; a model or an effort Mako does not know keeps the
// default.
func routes(table map[string]map[string]struct{ Model, Effort string }) core.RouteTable {
	t := core.DefaultRoutes()
	for phase, bySize := range table {
		if _, ok := t[phase]; !ok {
			continue
		}
		for size, r := range bySize {
			def, ok := t[phase][size]
			if !ok {
				continue
			}
			if m := strings.ToLower(strings.TrimSpace(r.Model)); slices.Contains(core.Models, m) {
				def.Model = m
			}
			if e := strings.ToLower(strings.TrimSpace(r.Effort)); slices.Contains(core.Efforts, e) {
				def.Effort = e
			}
			t[phase][size] = def
		}
	}
	return t
}

// rawPrice: a model's price in the config; a kind left out keeps the default.
type rawPrice struct {
	Input       *float64 `json:"input"`
	Output      *float64 `json:"output"`
	CacheRead   *float64 `json:"cacheRead"`
	CacheCreate *float64 `json:"cacheCreate"`
}

// prices: the default prices with the config's changes; a negative price, or a model of a family Mako has no
// price for (nothing would use it), is ignored, with a line in the log.
func prices(own map[string]rawPrice) core.Prices {
	p := core.DefaultPrices()
	for model, r := range own {
		family := core.ModelFamily(model)
		pr, ok := p[family]
		if !ok {
			log.Printf("config: routes.prices: %q is no model family Mako prices (%s): ignored", model, strings.Join(slices.Sorted(maps.Keys(p)), ", "))
			continue
		}
		for _, f := range []struct {
			v   *float64
			dst *float64
		}{{r.Input, &pr.Input}, {r.Output, &pr.Output}, {r.CacheRead, &pr.CacheRead}, {r.CacheCreate, &pr.CacheCreate}} {
			switch {
			case f.v == nil:
			case *f.v < 0:
				log.Printf("config: routes.prices: %q: a negative price is ignored", model)
			default:
				*f.dst = *f.v
			}
		}
		p[family] = pr
	}
	return p
}

// routeMode: a known mode, else "balanced".
func routeMode(s string) string {
	if s = strings.TrimSpace(s); slices.Contains(core.Modes, s) {
		return s
	}
	return core.ModeBalanced
}

// CheckTimeout: how long one check may run.
func (t Terminal) CheckTimeout() time.Duration {
	if t.CheckTimeoutSec <= 0 {
		return 10 * time.Minute
	}
	return time.Duration(t.CheckTimeoutSec) * time.Second
}

// CheckTries: failed runs of the checks before the task needs your attention.
func (t Terminal) CheckTries() int {
	if t.CheckAttempts <= 0 {
		return 3
	}
	return t.CheckAttempts
}

// cleanChecks: the commands without blank ones.
func cleanChecks(list []string) []string {
	var out []string
	for _, c := range list {
		if c = strings.TrimSpace(c); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// autoRebase: "rebase" or "merge" as set, anything else is off.
func autoRebase(v string) string {
	if v == "rebase" || v == "merge" {
		return v
	}
	return ""
}

// MigrateRaw gives an older config's "language" its new name, "agentLanguage": the choice is kept and the old key
// goes. A config that already has "agentLanguage" only loses "language". Anything that is not a JSON object is
// returned as it is.
func MigrateRaw(data []byte) []byte {
	var m map[string]json.RawMessage
	if json.Unmarshal(data, &m) != nil || m == nil {
		return data
	}
	old, ok := m["language"]
	if !ok {
		return data
	}
	delete(m, "language")
	if _, has := m["agentLanguage"]; !has {
		m["agentLanguage"] = old
	}
	out, err := json.Marshal(m)
	if err != nil {
		return data
	}
	return out
}
