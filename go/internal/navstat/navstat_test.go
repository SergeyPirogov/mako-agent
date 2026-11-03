package navstat

import (
	"fmt"
	"strings"
	"testing"
)

const root = "/w/app"

// use: an assistant line of the main thread with one tool call.
func use(id, name, input string) string {
	return fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":"m-%s","content":[{"type":"tool_use","id":%q,"name":%q,"input":%s}]}}`,
		id, id, name, input)
}

func result(id, text string) string {
	return fmt.Sprintf(`{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q}]}}`, id, text)
}

func analyze(lines ...string) Session {
	return Analyze(strings.NewReader(strings.Join(lines, "\n")), root)
}

func TestSearchesBeforeFirstEdit(t *testing.T) {
	s := analyze(
		`{"type":"user","message":{"content":"Fix the button"}}`,
		use("t1", "Grep", `{"pattern":"payButton","path":"/w/app/web"}`),
		result("t1", "No matches found"),
		use("t2", "Glob", `{"pattern":"**/*.html"}`),
		result("t2", "/w/app/web/board.html"),
		use("t3", "Bash", `{"command":"cd go && grep -rn pay internal/"}`),
		result("t3", "internal/pay.go:1"),
		use("t4", "Bash", `{"command":"go test ./..."}`), // not a search
		result("t4", "ok"),
		use("t5", "Read", `{"file_path":"/w/app/docs/board.md"}`),
		result("t5", "docs"),
		use("t6", "Read", `{"file_path":"/w/app/web/board.html","offset":10,"limit":50}`),
		result("t6", strings.Repeat("x", 100)),
		use("t7", "Edit", `{"file_path":"/w/app/web/board.html","old_string":"a","new_string":"b"}`),
		result("t7", "ok"),
		use("t8", "Grep", `{"pattern":"after"}`), // after the edit: not counted
		result("t8", ""),
		// A subagent's search is not the main agent's.
		`{"type":"assistant","parent_tool_use_id":"x","message":{"content":[{"type":"tool_use","id":"s1","name":"Grep","input":{"pattern":"sub"}}]}}`,
	)
	if s.Searches != 5 || !s.Edited {
		t.Fatalf("searches %d edited %v (%v)", s.Searches, s.Edited, s.Queries)
	}
	if s.Queries[0] != "Grep: payButton in web" || s.Queries[2] != "Bash: grep -rn pay internal/" || s.Queries[3] != "Read: docs/board.md" {
		t.Fatalf("queries %q", s.Queries)
	}
	if len(s.Empty) != 2 || s.Empty[0] != "Grep: payButton in web" || s.Empty[1] != "Grep: after" {
		t.Fatalf("empty %q", s.Empty)
	}
	if len(s.DocsBeforeEdit) != 1 || s.DocsBeforeEdit[0] != "docs/board.md" || len(s.EditedFiles) != 1 || s.EditedFiles[0] != "web/board.html" {
		t.Fatalf("docs %q edited %q", s.DocsBeforeEdit, s.EditedFiles)
	}
}

func TestRepeatedAndBigReads(t *testing.T) {
	big := strings.Repeat("x", BigRead+1)
	s := analyze(
		use("r1", "Read", `{"file_path":"/w/app/big.go"}`),
		result("r1", big),
		use("r2", "Read", `{"file_path":"/w/app/big.go","offset":1,"limit":10}`), // a part: not a big read
		result("r2", big),
		use("r3", "Read", `{"file_path":"/w/app/big.go"}`),
		result("r3", "short now"),
		use("r4", "Read", `{"file_path":"/elsewhere/x.go"}`),
		result("r4", "x"),
	)
	r := s.Reads["big.go"]
	if r == nil || r.Count != 3 || r.Chars != 2*(BigRead+1)+9 {
		t.Fatalf("reads %+v", s.Reads)
	}
	if len(s.Reads) != 1 {
		t.Fatalf("a file outside the worktree is left out: %+v", s.Reads)
	}
	if len(s.Big) != 1 || s.Big[0].File != "big.go" {
		t.Fatalf("big %+v", s.Big)
	}
	if s.Edited || s.Searches != 3 {
		t.Fatalf("no edit: every search counts, %d", s.Searches)
	}
}

func TestDigest(t *testing.T) {
	sess := func(searches int, reads map[string]int, empty []string, docs, edited []string) Session {
		s := Session{Searches: searches, Reads: map[string]*Read{}, Empty: empty, DocsBeforeEdit: docs, EditedFiles: edited, Edited: len(edited) > 0}
		for f, n := range reads {
			s.Reads[f] = &Read{File: f, Count: n, Chars: n * 4000}
		}
		for i := 0; i < searches && i < maxQueries; i++ {
			s.Queries = append(s.Queries, fmt.Sprintf("Grep: q%d", i))
		}
		return s
	}
	tasks := []Task{
		{ID: "a#1", Title: "One", Session: sess(12, map[string]int{"web/board.html": 4, "go.mod": 1}, []string{"Grep: payButton"}, []string{"README.md"}, []string{"web/board.html"})},
		{ID: "a#2", Session: sess(2, map[string]int{"web/board.html": 1}, []string{"Grep: payButton"}, nil, []string{"x.go"})},
		{ID: "a#3", Session: sess(4, map[string]int{"web/board.html": 1}, nil, nil, nil)},
	}
	d := Digest(tasks)
	for _, want := range []string{
		"Tasks analyzed: 3 (the agent edited files in 2).",
		"average 6.0, median 4, most 12",
		"- web/board.html: 3 of 3 tasks, 6 reads, ≈6000 tokens, up to 4 in one task",
		"- go.mod: 1 of 3 tasks",
		"- a#1: web/board.html, 4 times",
		"Searches that found nothing: 2 in all",
		"- Grep: payButton (2)",
		"- a#1: read README.md, then edited web/board.html",
		"- a#1 (One): 12 searches; first ones: Grep: q0",
	} {
		if !strings.Contains(d, want) {
			t.Errorf("no %q in the digest:\n%s", want, d)
		}
	}
	if strings.Index(d, "web/board.html: 3 of 3") > strings.Index(d, "go.mod: 1 of 3") {
		t.Errorf("the files read in more tasks go first:\n%s", d)
	}
	if Digest(nil) != "" {
		t.Error("no tasks, no digest")
	}
}

func TestShellEdits(t *testing.T) {
	for cmd, want := range map[string]bool{
		"cat > docs/x.md <<'EOF'\nline => more\nEOF":                      true,
		"cd go && python3 - <<'EOF'\np='a.go'\nopen(p,'w').write(s)\nEOF": true,
		"sed -i '' 's/a/b/' internal/x.go":                                true,
		"echo hi | tee notes.txt":                                         true,
		"grep -n \"x => y\" board.html":                                   false,
		"go test ./... 2>&1 | tail -3":                                    false,
		"grep foo bar > /dev/null":                                        false,
		"ls /tmp > /tmp/out.txt":                                          false, // outside the worktree
		"python3 -c 'print(1)'":                                           false,
		"grep -n \"sed -i\" README.md":                                    false,
	} {
		s := analyze(use("t1", "Bash", fmt.Sprintf(`{"command":%q}`, cmd)), result("t1", "x"))
		if s.Edited != want {
			t.Errorf("%q: edited %v, want %v", cmd, s.Edited, want)
		}
	}
	s := analyze(use("t1", "Read", `{"file_path":"/var/folders/T/shot.png"}`), result("t1", "img"))
	if s.Searches != 0 || len(s.Reads) != 0 {
		t.Fatalf("a file outside the worktree is not the repository's: %+v", s)
	}
}

func TestShellReads(t *testing.T) {
	s := analyze(
		// «cd go» holds for the next calls too, as in Claude Code.
		use("t1", "Bash", `{"command":"cd go && sed -n 1,60p internal/a.go; sed -n '10,20p' ../docs/b.md | head -5"}`),
		result("t1", strings.Repeat("x", 100)),
		use("t2", "Bash", `{"command":"cat big.go"}`),
		result("t2", strings.Repeat("x", BigRead+1)),
		use("t3", "Bash", `{"command":"cd /w/app && head -n 50 go/big.go && wc -l go/*.go"}`),
		result("t3", "x"),
		use("t4", "Bash", `{"command":"grep -rn nothing go/"}`),
		result("t4", ""),
		use("t5", "Write", `{"file_path":"/home/me/.claude/plans/plan.md"}`), // not the repository's
	)
	if r := s.Reads["go/internal/a.go"]; r == nil || r.Count != 1 || r.Chars != 50 {
		t.Fatalf("reads %+v", s.Reads)
	}
	if len(s.DocsBeforeEdit) != 1 || s.DocsBeforeEdit[0] != "docs/b.md" {
		t.Fatalf("docs %q", s.DocsBeforeEdit)
	}
	if r := s.Reads["go/big.go"]; r == nil || r.Count != 2 || len(s.Big) != 1 {
		t.Fatalf("big %+v %+v", s.Reads, s.Big)
	}
	if len(s.Reads) != 3 || len(s.Empty) != 1 || s.Searches != 4 || s.Edited {
		t.Fatalf("%+v", s)
	}
}
