package dispatch

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/SergeyPirogov/mako-agent/go/internal/config"
	"github.com/SergeyPirogov/mako-agent/go/internal/core"
	"github.com/SergeyPirogov/mako-agent/go/internal/gitx"
	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
	"github.com/SergeyPirogov/mako-agent/go/internal/writer"
)

// Lessons from the reviewer: when the reviewer agent keeps making the same kind of remark on a repo's tasks
// (a forgotten test, a string left untranslated, gofmt not run), Mako offers a rule for the repo's CLAUDE.md, as
// it does for your corrections. The remarks are grouped without a model first; only a group from enough tasks
// goes to the model, which writes the rule or says there is none.

// LessonFromRemarks draws the rule from a group of the reviewer's remarks; tests replace it.
var LessonFromRemarks = writer.LessonFromRemarks

// repoLessonEvery: the remarks of a repo are looked at again no sooner than this.
const repoLessonEvery = time.Hour

// The model is asked about this many groups of a repo at most in one look.
const repoLessonAsks = 3

// remarkItem: one remark of the reviewer, with the words it is compared by.
type remarkItem struct {
	task, weight, file, text string
	words                    map[string]bool
}

// remarkGroup: remarks much alike, from the repo's tasks.
type remarkGroup struct {
	items  []remarkItem
	tasks  []string // the tasks, each once, sorted
	weight int      // «must» counts 2, «should» 1
	key    []string // the words most of its remarks share, sorted
}

// collectRemarks: the reviewer's remarks over the tasks, from the rounds since since; the remarks a round
// carried over from an earlier one (Again) count once, where they were made.
func collectRemarks(records []core.Record, since time.Time) []remarkItem {
	var out []remarkItem
	for _, r := range records {
		for _, a := range r.AgentReviews {
			if at, err := time.Parse(time.RFC3339Nano, a.At); err == nil && at.Before(since) {
				continue
			}
			for _, m := range a.Remarks {
				if m.Again || strings.TrimSpace(m.Text) == "" {
					continue
				}
				w := remarkWords(m.Text)
				if len(w) == 0 {
					continue
				}
				out = append(out, remarkItem{task: r.ID, weight: m.Weight, file: m.File, text: strings.TrimSpace(m.Text), words: w})
			}
		}
	}
	return out
}

// Words that say nothing of what a remark is about.
var remarkStop = setOf(strings.Fields("the and for with this that these those not are was were but from into has have had should must " +
	"also add added adds any all its it's there here when which what only one new use used does doesn don can will would more other " +
	"then than them they out now just like via per see why how where who still yet make makes made need needs needed get set " +
	"call called code line lines file files function method change changes changed instead please missing" +
	" і й та що як не на в у до за по це цей ця ці для від але або чи вже ще так тут там його її їх треба потрібно варто слід немає нема лише також коли щоб бо при під над без усі всі після перед є був була було щоб")) // cyrillic-ok: Ukrainian stop words of the reviewer's remarks

// The endings stems drop: vowels (Latin and Cyrillic) and a plural s.
const stemEnds = "aeiouys" + "аеєиіїоуюяьй" // cyrillic-ok: Ukrainian vowels, for stemming

// remarkWords: the remark's words, lowercased and roughly stemmed, without paths, file names, the code's
// identifiers, numbers and stop words: two remarks of the same kind on different tasks share them.
func remarkWords(text string) map[string]bool {
	out := map[string]bool{}
	for _, tok := range strings.Fields(text) {
		tok = strings.Trim(tok, "`*_()[]{}<>,.;:!?\"'«»—–-")
		if tok == "" || strings.ContainsAny(tok, "/\\") || identifier(tok) {
			continue
		}
		if tok = strings.ToLower(tok); fileName(tok) {
			continue
		}
		for _, w := range strings.FieldsFunc(tok, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
			if remarkStop[w] || utf8.RuneCountInString(w) < 3 || !hasLetter(w) {
				continue
			}
			if s := stem(w); utf8.RuneCountInString(s) >= 3 {
				out[s] = true
			}
		}
	}
	return out
}

// identifier: the token is a name in the code, «AddRule», «lessonsFile», «agent_review», «r.Task»: of this
// task, not of the kind of remark. Acronyms («API», «CI») are words.
func identifier(tok string) bool {
	if strings.Contains(tok, "_") {
		return true
	}
	if i := strings.Index(tok, "."); i > 0 && i < len(tok)-1 && !fileName(strings.ToLower(tok)) {
		return true
	}
	prev := ' '
	for _, r := range tok {
		if unicode.IsUpper(r) && unicode.IsLower(prev) {
			return true
		}
		prev = r
	}
	return false
}

// fileName: the token reads like a file's name: «task.go», «board.html».
func fileName(tok string) bool {
	i := strings.LastIndex(tok, ".")
	if i <= 0 || i == len(tok)-1 {
		return false
	}
	ext := tok[i+1:]
	if len(ext) > 5 {
		return false
	}
	for _, r := range ext {
		if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

func hasLetter(w string) bool {
	for _, r := range w {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// stem: the first five letters of the word without the vowels and the s they end with: «tests», «testing»
// and «test» are one stem, so are the Ukrainian word forms.
func stem(w string) string {
	rs := []rune(w)
	if len(rs) > 5 {
		rs = rs[:5]
	}
	for len(rs) > 3 && strings.ContainsRune(stemEnds, rs[len(rs)-1]) {
		rs = rs[:len(rs)-1]
	}
	return string(rs)
}

// jaccard: how much two sets of words have in common, from 0 to 1.
func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	n := shared(a, b)
	return float64(n) / float64(len(a)+len(b)-n)
}

func shared(a, b map[string]bool) int {
	n := 0
	for w := range a {
		if b[w] {
			n++
		}
	}
	return n
}

// alike: two remarks are of one kind: they share a good part of their words (Dice), and three of them at least
// (two, when one remark is that short); on the same package a smaller part does. The reviewer's remarks are long
// and name the task's own code, so the same kind of remark shares a fourth of its words or so.
func alike(a, b remarkItem) bool {
	n := shared(a.words, b.words)
	if n < 2 || n < 3 && min(len(a.words), len(b.words)) > 3 {
		return false
	}
	dice := 2 * float64(n) / float64(len(a.words)+len(b.words))
	if dice >= 0.25 {
		return true
	}
	return dice >= 0.2 && a.file != "" && b.file != "" && path.Dir(a.file) == path.Dir(b.file)
}

// remarkGroups: the groups of remarks much alike from at least minTasks tasks, the heaviest first. Many remarks
// from fewer tasks do not make one: those are the rounds of one task saying it again, not a habit of the repo. Each group is a remark with the most like it on other tasks, and those: a
// chain of remarks each a bit like the next does not make one.
func remarkGroups(items []remarkItem, minTasks int) []remarkGroup {
	near := make([][]int, len(items))
	for i := range items {
		for j := i + 1; j < len(items); j++ {
			if items[i].task != items[j].task && alike(items[i], items[j]) {
				near[i], near[j] = append(near[i], j), append(near[j], i)
			}
		}
	}
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool { return len(near[order[a]]) > len(near[order[b]]) })
	used := make([]bool, len(items))
	var out []remarkGroup
	for _, c := range order {
		if used[c] || len(near[c]) == 0 {
			continue
		}
		members := []int{c}
		for _, j := range near[c] {
			if !used[j] {
				members = append(members, j)
			}
		}
		g := remarkGroup{}
		seen := map[string]bool{}
		for _, i := range members {
			it := items[i]
			g.items = append(g.items, it)
			if !seen[it.task] {
				seen[it.task] = true
				g.tasks = append(g.tasks, it.task)
			}
			g.weight++
			if it.weight == core.Must {
				g.weight++
			}
		}
		if len(g.tasks) < minTasks {
			continue
		}
		for _, i := range members {
			used[i] = true
		}
		sort.Strings(g.tasks)
		g.key = groupKey(g.items)
		out = append(out, g)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].weight != out[j].weight {
			return out[i].weight > out[j].weight
		}
		return len(out[i].tasks) > len(out[j].tasks)
	})
	return out
}

// groupKey: the words at least half of the group's remarks have; those of its first remark when none are.
func groupKey(items []remarkItem) []string {
	count := map[string]int{}
	for _, it := range items {
		for w := range it.words {
			count[w]++
		}
	}
	var key []string
	for w, n := range count {
		if 2*n >= len(items) {
			key = append(key, w)
		}
	}
	if len(key) == 0 {
		for w := range items[0].words {
			key = append(key, w)
		}
	}
	sort.Strings(key)
	return key
}

// knownGroup: a lesson of the repo (offered, saved, declined or found to be none) was drawn from much the same group.
func knownGroup(g remarkGroup, lessons []core.SavedLesson, same func(core.SavedLesson) bool) bool {
	key := setOf(g.key)
	for _, l := range lessons {
		if l.Key != "" && same(l) && jaccard(key, setOf(strings.Fields(l.Key))) >= 0.5 {
			return true
		}
	}
	return false
}

// lines: the group's remarks as the model reads them, «grp/app#5 [must] path/file.go: text».
func (g remarkGroup) lines() []string {
	var out []string
	for i, it := range g.items {
		if i == 20 {
			break
		}
		where := ""
		if it.file != "" {
			where = " " + it.file
		}
		out = append(out, fmt.Sprintf("%s [%s]%s: %s", it.task, it.weight, where, clipText(it.text, 300)))
	}
	return out
}

// examples: a few of the group's remarks for the board, the «must» ones first, one per task.
func (g remarkGroup) examples(n int) []string {
	items := append([]remarkItem(nil), g.items...)
	sort.SliceStable(items, func(i, j int) bool { return items[i].weight == core.Must && items[j].weight != core.Must })
	seen := map[string]bool{}
	var out []string
	for _, it := range items {
		if len(out) == n {
			break
		}
		if !seen[it.task] {
			seen[it.task] = true
			out = append(out, it.task+": "+clipText(firstLine(it.text), 200))
		}
	}
	return out
}

func setOf(words []string) map[string]bool {
	out := make(map[string]bool, len(words))
	for _, w := range words {
		out[w] = true
	}
	return out
}

// suggestRepoLesson looks for remarks the reviewer keeps making on the task's repo and offers a rule for its
// CLAUDE.md from them: after each of the reviewer's rounds, once an hour per repo at most, and while no other
// such offer of the repo waits for an answer.
func (d *Dispatcher) suggestRepoLesson(id string) {
	r, ok := d.Store.Get(id)
	if !ok {
		return
	}
	t := d.tracker(r.Task.TrackerID)
	if t == nil {
		return
	}
	repo, err := d.route(t, r.Task)
	if err != nil {
		return
	}
	key := r.Task.TrackerID + "|" + r.Task.Project + "|" + repo.Name
	d.mu.Lock()
	if d.repoLessoning[key] || time.Since(d.repoLessoned[key]) < repoLessonEvery {
		d.mu.Unlock()
		return
	}
	d.repoLessoning[key], d.repoLessoned[key] = true, time.Now()
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.repoLessoning, key)
		d.mu.Unlock()
	}()

	same := func(l core.SavedLesson) bool {
		return l.TrackerID == r.Task.TrackerID && l.Project == r.Task.Project && l.Repo == repo.Name
	}
	lessons := d.Lessons()
	for _, l := range lessons {
		if same(l) && l.State == core.LessonOffered {
			return
		}
	}
	var mine []core.Record
	for _, x := range d.Store.All() {
		if x.Task.TrackerID != r.Task.TrackerID || x.Task.Project != r.Task.Project {
			continue
		}
		if xr, err := d.route(t, x.Task); err == nil && xr.Name == repo.Name {
			mine = append(mine, x)
		}
	}
	rv := d.Config().Reviewer
	groups := remarkGroups(collectRemarks(mine, time.Now().AddDate(0, 0, -rv.LessonDays)), rv.LessonTasks)
	asked := 0
	for _, g := range groups {
		if knownGroup(g, lessons, same) {
			continue
		}
		if asked == repoLessonAsks {
			return
		}
		asked++
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		dir := ""
		if exists(repo.Path) {
			dir = repo.Path
		}
		rule, err := LessonFromRemarks(ctx, dir, g.lines(), baseClaudeMD(ctx, repo.Path, repo.BaseBranch))
		cancel()
		if err != nil {
			return // the next look tries again
		}
		l := core.SavedLesson{TrackerID: r.Task.TrackerID, Project: r.Task.Project, Repo: repo.Name, Source: "reviewer",
			Key: strings.Join(g.key, " "), Tasks: g.tasks, Examples: g.examples(3), At: now(), State: core.LessonNone}
		l.ID = lessonID(key, l.Key, l.At)
		if rule != "" {
			l.State, l.Rule = core.LessonOffered, rule
		}
		if d.keepLesson(l) != nil || rule == "" {
			continue
		}
		d.Log(id, fmt.Sprintf("Mako suggests a lesson for CLAUDE.md from the reviewer's remarks on %s: %s", strings.Join(g.tasks, ", "), rule))
		return
	}
}

// baseClaudeMD: the repo's CLAUDE.md on origin's base branch, the clone's own when that cannot be read.
func baseClaudeMD(ctx context.Context, repoPath, base string) string {
	if !exists(repoPath) {
		return ""
	}
	if base == "" {
		base = gitx.DetectBaseBranch(repoPath)
	}
	if md, err := gitx.Run(ctx, repoPath, "show", "origin/"+base+":CLAUDE.md"); err == nil {
		return md
	}
	md, _ := os.ReadFile(filepath.Join(repoPath, "CLAUDE.md"))
	return string(md)
}

func lessonID(parts ...string) string {
	h := sha1.Sum([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:6])
}

// AnswerRepoLesson answers the offer of a lesson from the reviewer's remarks: "base" commits the rule (as you
// edited it) to CLAUDE.md on the repo's base branch and pushes it, "no" declines it; either way the group of
// remarks is not offered again.
func (d *Dispatcher) AnswerRepoLesson(ctx context.Context, lessonID, to, rule string) error {
	var l core.SavedLesson
	for _, x := range d.Lessons() {
		if x.ID == lessonID && x.State == core.LessonOffered {
			l = x
		}
	}
	if l.ID == "" {
		return fmt.Errorf("no such lesson offer")
	}
	switch to {
	case "no":
		return d.setLesson(lessonID, func(x *core.SavedLesson) { x.State, x.At = core.LessonDeclined, now() })
	case "base":
	default:
		return fmt.Errorf("unknown where to save: %q", to)
	}
	rule = strings.Join(strings.Fields(rule), " ")
	if rule == "" {
		return fmt.Errorf("the rule is empty")
	}
	t := d.tracker(l.TrackerID)
	if t == nil {
		return fmt.Errorf("tracker %s was removed from settings", l.TrackerID)
	}
	p := t.Config().Projects[l.Project]
	if p == nil {
		return fmt.Errorf("project %s is not configured", l.Project)
	}
	var repo *config.Repo
	for _, r := range p.Repos {
		if r.Name == l.Repo {
			repo = r
		}
	}
	if repo == nil || !exists(repo.Path) {
		return fmt.Errorf("repository %s of project %s does not exist", l.Repo, l.Project)
	}
	branch := repo.BaseBranch
	if branch == "" {
		branch = gitx.DetectBaseBranch(repo.Path)
	}
	hash, err := lessonToBase(ctx, repo.Path, branch, LessonsHeading, rule, i18n.TA("commit.repoLesson", clipText(rule, 200), strings.Join(l.Tasks, ", ")))
	if err != nil {
		return err
	}
	return d.setLesson(lessonID, func(x *core.SavedLesson) {
		x.State, x.Rule, x.Branch, x.Commit, x.At = "", rule, branch, hash, now()
	})
}

// setLesson changes the lesson with the id in lessons.json.
func (d *Dispatcher) setLesson(id string, change func(*core.SavedLesson)) error {
	return d.updateLessons(func(all []core.SavedLesson) []core.SavedLesson {
		for i := range all {
			if all[i].ID == id {
				change(&all[i])
			}
		}
		return all
	})
}

// LessonOffers: how many lessons from the reviewer's remarks wait for an answer, by project.
func (d *Dispatcher) LessonOffers() map[string]int {
	out := map[string]int{}
	for _, l := range d.Lessons() {
		if l.State == core.LessonOffered {
			out[l.Project]++
		}
	}
	return out
}
