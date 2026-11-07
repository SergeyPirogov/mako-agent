package i18n

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every key the pages and Go ask for is in the dictionary: a missing one would show as «s.o.planHint».
func TestEveryKeyUsedExists(t *testing.T) {
	// The keys put together from a prefix, with all their endings.
	families := map[string][]string{
		"b.col.":         {"later", "new", "doing", "blocked", "review", "merge", "forme", "mako", "done", "failed", "ideas"},
		"b.spec.st.":     {"running", "questions", "ready", "failed", "creating", "created"},
		"idea.title.":    {"fault", "fail", "cost"},
		"idea.intro.":    {"fault", "fail", "cost"},
		"b.idea.why.":    {"fault", "fail", "cost"},
		"b.idea.src.":    {"fault", "fail", "cost"},
		"b.label.":       {"new", "later", "queued", "starting", "doing", "review", "done", "failed", "ag-working", "ag-waiting", "ag-done", "ag-stopped", "ci", "ag-idle", "ag-missing"},
		"b.agent.":       {"waiting", "done", "idle", "unknown"},
		"s.n.kind.":      {"waiting", "finished", "stopped", "review", "agentReview", "secrets", "merged", "newTask", "reviewAsked", "reminder", "gone", "limits"},
		"s.nav.projects": {".one", ".many"},
		"s.mako.commits": {".one", ".many"},
		"s.d.count":      {".one", ".many"},
		"s.perm.agents":  {".one", ".many"},
		"s.perm.note":    {".one", ".many"},
		"tray.col.":      {"new", "doing", "review"},
		"theme.":         {"auto", "light", "dark"},
		"s.r.mode.":      {"cost", "balanced", "quality"},
		"s.r.size.":      {"small", "medium", "big"},
		"s.r.phase.":     {"estimate", "intake", "plan", "work", "review", "cifix", "assess"},
		"s.r.phaseHint.": {"estimate", "intake", "plan", "work", "review", "cifix", "assess"},

		"b.intake.d.":      {"take", "clarify", "answer", "split"},
		"b.intake.status.": {"clarify", "answer", "split"},

		"b.eval.st.": {"passed", "failed", "error", "skipped", "running", "waiting", "done", "stopped"},
	}
	page := regexp.MustCompile(`"((?:s|b|theme|tray)\.[a-zA-Z]+(?:\.[a-zA-Z-]+)*\.?)"`)
	goKey := regexp.MustCompile(`"((?:notify|tray|menu|agent|comment|mr|commit|digest|problem|action|task|summary|idea)\.[a-zA-Z]+(?:\.[a-zA-Z-]+)*\.?)"`)
	files := regexp.MustCompile(`\.(json|md|ts|rb|go|png|js|html|css|txt|ya?ml)$`)

	used := map[string]string{} // key → where
	for _, f := range []string{"../web/board.html", "../web/settings.html", "../web/theme.js"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range page.FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = f
		}
		// data-t="…" and the like
		for _, m := range regexp.MustCompile(`data-t(?:-title|-ph)?="([^"]+)"`).FindAllStringSubmatch(string(b), -1) {
			used[m[1]] = f
		}
	}
	filepath.WalkDir("..", func(path string, e fs.DirEntry, err error) error {
		if err != nil || e.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/i18n/") {
			return nil
		}
		b, _ := os.ReadFile(path)
		for _, m := range goKey.FindAllStringSubmatch(string(b), -1) {
			if !files.MatchString(m[1]) {
				used[m[1]] = path
			}
		}
		return nil
	})
	if len(used) < 300 {
		t.Fatalf("only %d keys found: the search is broken", len(used))
	}
	for key, where := range used {
		if ends, ok := families[key]; ok {
			for _, end := range ends {
				if !known(key + end) {
					t.Errorf("%s: no %q", where, key+end)
				}
			}
			continue
		}
		if strings.HasSuffix(key, ".") {
			t.Errorf("%s: %q is put together; list its endings in families", where, key)
			continue
		}
		if !known(key) {
			t.Errorf("%s: no %q in the dictionary", where, key)
		}
	}
}

// known: the dictionary has key, among the Go texts or the pages' words.
func known(key string) bool {
	if _, ok := dict[key]; ok {
		return true
	}
	_, ok := webStrings[key]
	return ok
}

// The pages get only webStrings: a key that lives among the Go texts (the tray's «tray.poll») would show as
// the raw key. Walks every data-t, data-t-title, data-t-ph and t("…") in the pages.
func TestPagesUseOnlyWebStrings(t *testing.T) {
	attr := regexp.MustCompile(`data-t(?:-title|-ph)?="([^"]+)"`)
	call := regexp.MustCompile(`\bt\("([^"]+)"`)
	for _, f := range []string{"../web/board.html", "../web/settings.html"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, re := range []*regexp.Regexp{attr, call} {
			for _, m := range re.FindAllStringSubmatch(string(b), -1) {
				n++
				if strings.HasSuffix(m[1], ".") {
					continue // put together from a prefix; TestEveryKeyUsedExists lists its endings
				}
				if _, ok := webStrings[m[1]]; !ok {
					t.Errorf("%s: %q is not in the web strings", f, m[1])
				}
			}
		}
		if n < 20 {
			t.Errorf("%s: only %d keys found: the search is broken", f, n)
		}
	}
}
