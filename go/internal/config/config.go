// Package config reads the same mako.config.json and .env as the TypeScript Mako, keeping what the
// dispatcher uses: trackers and their projects, Orca, limits and notifications.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	DefaultBranchPattern = "{type}/{id}-{title}"
	DefaultProjectsDir   = "~/mako/projects"
)

type Config struct {
	Path            string
	Home            string // the folder of mako.config.json and .env
	StateDir        string
	PollIntervalSec int
	WipLimit        int
	// WipAuto: WipLimit is AutoWip, worked out from this Mac's cores and memory, not the number in the config.
	WipAuto bool
	// LimitPausePct: at this share of a Claude usage limit (5 hours or the week) used, the queue waits for it
	// to reset; 0: never.
	LimitPausePct    int
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
	// Reviewer: a second agent reads the task and the branch's diff before the MR reaches you.
	Reviewer Reviewer
	// Estimate: a new task gets the agent's estimate (clarity, size, risk, advice) as it appears.
	Estimate bool
}

// Reviewer is the agent that reviews another agent's branch with a clean context and writes it its remarks;
// it changes no code. Only Mako's own agents get it: the remarks go into their chat.
type Reviewer struct {
	Enabled   bool
	MaxRounds int    // reviews of one task at most, 1 by default
	Model     string // "" — the agents' model
	// When: which tasks get it, by the risk of their assessment: "always", "medium" (medium and high risk) or
	// "high" (high risk only). A task with no assessment gets it.
	When string
}

// Reviewer's When.
const (
	ReviewAlways = "always"
	ReviewMedium = "medium"
	ReviewHigh   = "high"
)

// Comments Mako leaves in the issue, written as the developer: {branch} {taskId} {title} {url} {mr} are filled
// in; "-" leaves no comment.
type Comments struct {
	OnStart  string // when the task goes to Orca
	OnReview string // when its MR appears
}

const (
	DefaultCommentOnStart  = "Взяв задачу в роботу. Гілка {branch}. Як буде готово, відкрию MR."
	DefaultCommentOnReview = "Відкрив MR: {mr}"
)

type Notifications struct {
	Enabled   bool
	AutoTake  bool
	SnoozeMin int
	Port      int
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
	Model   string // "" — Claude Code's own default
	// PermissionMode: how much Claude may do without asking: "auto" (default), "acceptEdits", "default",
	// "bypassPermissions".
	PermissionMode string
	AllowedTools   []string // used without asking: "Bash(git:*)", "WebFetch"
	// SetupCommand prepares every new worktree unless its repo has its own setup; "" guesses from the
	// lockfile, "-" runs nothing.
	SetupCommand string
	// View: how the agent is shown, "chat" (default) or "terminal".
	View string
	// AutoFix: the agent is asked by itself to fix its MR's failed CI, conflicts and new review comments.
	AutoFix bool
	// AutoRebase: how Mako brings the branch of an MR in review up to its base branch by itself, "rebase" or
	// "merge"; "" never. Conflicts go to the agent.
	AutoRebase string
	// RebaseEveryMin: how often a branch is looked at for that, in minutes (30 when not set).
	RebaseEveryMin int
	// PlanFirst: when the agent starts in Claude Code's plan mode, exploring and writing a plan you approve
	// before it changes anything: "never" (default), "big" (a task labelled plan or with a long description),
	// "always".
	PlanFirst string
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
	Paused      bool
	Transitions Transitions
	Projects    map[string]*Project
}

type Project struct {
	Key   string
	Repos map[string]*Repo
	// Default repo when a project has several; with one it is that one.
	Default string
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
}

// ErrMissing: there is no mako.config.json yet (first start).
var ErrMissing = errors.New("Mako ще не налаштований")

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

// SaveEnv sets keys in home/.env, keeping its other lines; an empty value removes the key.
func SaveEnv(home string, updates map[string]string) error {
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
			line = k + "=" + v
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
			lines = append(lines, k+"="+updates[k])
		}
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600)
}

// Parse turns the raw config into what the dispatcher needs and fills the defaults.
func Parse(data []byte, env map[string]string) (*Config, error) {
	var raw rawConfig
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mako.config.json: %w", err)
	}
	c := &Config{
		StateDir:         Expand(raw.StateDir),
		PollIntervalSec:  orInt(raw.PollIntervalSec, 60),
		WipLimit:         orInt(raw.WipLimit, 0),
		WipAuto:          raw.WipAuto,
		LimitPausePct:    orInt(raw.LimitPausePct, 85),
		ArchiveAfterDays: orInt(raw.ArchiveAfterDays, 2),
		ProjectsDir:      Expand(orStr(raw.ProjectsDir, DefaultProjectsDir)),
		BranchPattern:    orStr(raw.BranchPattern, DefaultBranchPattern),
		Env:              env,
		Notifications: Notifications{
			Enabled:   raw.Notifications.Enabled == nil || *raw.Notifications.Enabled,
			AutoTake:  raw.Notifications.AutoTake != nil && *raw.Notifications.AutoTake,
			SnoozeMin: orInt(raw.Notifications.SnoozeMin, 60),
			Port:      orInt(raw.Notifications.InboxPort, 7777),
			AwayHours: orInt(raw.Notifications.AwayHours, 4),
			Off:       map[string]bool{},
		},
		Orca: Orca{Agent: "claude", Setup: "inherit", LinkIssue: true, MRBy: "agent"},
		Reviewer: Reviewer{
			Enabled:   raw.AgentReview.Enabled,
			MaxRounds: max(orInt(raw.AgentReview.MaxRounds, 1), 1),
			Model:     strings.TrimSpace(raw.AgentReview.Model),
			When:      reviewWhen(raw.AgentReview.When),
		},
		Estimate: raw.Estimate.Enabled == nil || *raw.Estimate.Enabled,
		Comments: Comments{
			OnStart:  orStr(strings.TrimSpace(raw.Comments.OnStart), DefaultCommentOnStart),
			OnReview: orStr(strings.TrimSpace(raw.Comments.OnReview), DefaultCommentOnReview),
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
			c.Terminal = Terminal{Command: e.Command, Model: e.Model, PermissionMode: orStr(e.PermissionMode, "auto"), AllowedTools: e.AllowedTools, SetupCommand: e.SetupCommand, View: e.View, AutoFix: e.AutoFix, AutoRebase: autoRebase(e.AutoRebase), RebaseEveryMin: e.RebaseEveryMin, PlanFirst: e.PlanFirst}
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
		c.Orca.Instructions = e.Instructions
		c.Orca.MRBy = orStr(e.MRBy, "agent")
		c.Orca.SummaryComment = e.SummaryComment
		c.Orca.ExtraArgs = e.ExtraArgs
		break
	}
	morning, err := clock(orStr(strings.TrimSpace(raw.Notifications.MorningDigest), DefaultMorningDigest))
	if err != nil {
		return nil, fmt.Errorf("ранковий підсумок: %w", err)
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
			return nil, fmt.Errorf("трекер %d: %w", i+1, err)
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
		Projects:        map[string]*Project{},
	}
	switch t.Type {
	case "gitlab", "github", "jira":
	default:
		return nil, fmt.Errorf("невідомий тип трекера %q", t.Type)
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
			return nil, fmt.Errorf("проєкт %s: %w", key, err)
		}
		project := &Project{Key: key, Repos: map[string]*Repo{}, Default: p.Default}
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
		}
		t.Projects[key] = project
	}
	if len(t.Projects) == 0 && !t.AllProjects {
		return nil, fmt.Errorf("%s: додайте проєкт або увімкніть «Мої задачі з усіх проєктів»", t.ID)
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
		env[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
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

type rawConfig struct {
	StateDir         string                 `json:"stateDir"`
	PollIntervalSec  *int                   `json:"pollIntervalSec"`
	WipLimit         *int                   `json:"wipLimit"`
	WipAuto          bool                   `json:"wipAuto"`
	LimitPausePct    *int                   `json:"limitPausePct"`
	ArchiveAfterDays *int                   `json:"archiveAfterDays"`
	ProjectsDir      string                 `json:"projectsDir"`
	BranchPattern    string                 `json:"branchPattern"`
	Notifications    rawNotifications       `json:"notifications"`
	Executors        map[string]rawExecutor `json:"executors"`
	Comments         struct {
		OnStart  string `json:"onStart"`
		OnReview string `json:"onReview"`
	} `json:"comments"`
	AgentReview struct {
		Enabled   bool   `json:"enabled"`
		MaxRounds *int   `json:"maxRounds"`
		Model     string `json:"model"`
		When      string `json:"when"`
	} `json:"agentReview"`
	Estimate struct {
		Enabled *bool `json:"enabled"`
	} `json:"estimate"`
	DefaultExecutor string       `json:"defaultExecutor"`
	Trackers        []rawTracker `json:"trackers"`
}

// reviewWhen: the reviewer's When, "always" for anything else.
func reviewWhen(s string) string {
	switch s = strings.TrimSpace(s); s {
	case ReviewMedium, ReviewHigh:
		return s
	}
	return ReviewAlways
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
	Enabled   *bool `json:"enabled"`
	AutoTake  *bool `json:"autoTake"`
	SnoozeMin *int  `json:"snoozeMin"`
	InboxPort *int  `json:"inboxPort"`
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
		return 0, fmt.Errorf("%q: потрібен час ГГ:ХХ, наприклад 09:00, або «-»", s)
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
	Type           string   `json:"type"`
	Command        string   `json:"command"`
	Agent          string   `json:"agent"`
	Setup          string   `json:"setup"`
	LinkIssue      *bool    `json:"linkIssue"`
	Instructions   string   `json:"instructions"`
	MRBy           string   `json:"mrBy"`
	SummaryComment bool     `json:"summaryComment"`
	ExtraArgs      []string `json:"extraArgs"`
	PermissionMode string   `json:"permissionMode"`
	AllowedTools   []string `json:"allowedTools"`
	SetupCommand   string   `json:"setupCommand"`
	View           string   `json:"view"`
	Model          string   `json:"model"`
	AutoFix        bool     `json:"autoFix"`
	AutoRebase     string   `json:"autoRebase"`
	RebaseEveryMin int      `json:"rebaseEveryMin"`
	PlanFirst      string   `json:"planFirst"`
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
	Transitions     map[string]json.RawMessage `json:"transitions"`
	Projects        map[string]json.RawMessage `json:"projects"`
}

type rawProject struct {
	Default string             `json:"default"`
	Repos   map[string]rawRepo `json:"repos"`
}

type rawRepo struct {
	RepoPath      string `json:"repoPath"`
	CloneURL      string `json:"cloneUrl"`
	BaseBranch    string `json:"baseBranch"`
	Setup         string `json:"setup"`
	BranchPattern string `json:"branchPattern"`
}

// autoRebase: "rebase" or "merge" as set, anything else is off.
func autoRebase(v string) string {
	if v == "rebase" || v == "merge" {
		return v
	}
	return ""
}
