package contextstat

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SergeyPirogov/mako-agent/go/internal/core"
)

// call: an assistant line of a main-thread model call with its context split as input/cache read/cache write.
func call(id string, in, read, create, out int, content string) string {
	if content == "" {
		content = "[]"
	}
	return fmt.Sprintf(`{"type":"assistant","parent_tool_use_id":null,"message":{"id":%q,"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d,"output_tokens":%d},"content":%s}}`,
		id, in, read, create, out, content)
}

func use(id, name, input string) string {
	return fmt.Sprintf(`[{"type":"tool_use","id":%q,"name":%q,"input":%s}]`, id, name, input)
}

func result(id string, chars int, isErr bool) string {
	return fmt.Sprintf(`{"type":"user","parent_tool_use_id":null,"message":{"content":[{"type":"tool_result","tool_use_id":%q,"content":%q,"is_error":%v}]}}`,
		id, strings.Repeat("x", chars), isErr)
}

func analyze(lines ...string) Result {
	return Analyze(strings.NewReader(strings.Join(lines, "\n")), core.PhaseWork)
}

func TestUsageStartPeakCache(t *testing.T) {
	r := analyze(
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"user","message":{"content":"`+strings.Repeat("я", 300)+`"}}`,
		call("m1", 10, 0, 20000, 5, ""),
		call("m1", 10, 0, 20000, 50, use("t1", "Read", `{"file_path":"a.go"}`)), // the same call, its last line
		result("t1", 4000, false),
		call("m2", 10, 20000, 1100, 10, ""),
		`{"type":"result","modelUsage":{"claude-opus-5-5":{"contextWindow":1000000}}}`,
	)
	if r.Session != "s-1" || r.Window != 1_000_000 || len(r.Runs) != 1 {
		t.Fatalf("%+v", r)
	}
	x := r.Runs[0]
	if x.Phase != core.PhaseWork || x.Session != "s-1" || x.Start != 20010 || x.Peak != 21110 || x.Calls != 2 || x.Prompt != 100 {
		t.Fatalf("%+v", x)
	}
	if x.Input != 20 || x.CacheRead != 20000 || x.CacheCreate != 21100 {
		t.Fatalf("cache %+v", x)
	}
	// The growth of 1100: the reply's 50 output tokens, the rest the file.
	if x.Tools[core.SourceReplies] != 50 || x.Tools["Read"] != 1050 {
		t.Fatalf("tools %v", x.Tools)
	}
	if len(x.Files) != 1 || x.Files[0] != (core.Heavy{Name: "a.go", Count: 1, Tokens: 1050}) {
		t.Fatalf("files %+v", x.Files)
	}
	if fmt.Sprint(x.Steps) != "[20010 21110]" {
		t.Fatalf("steps %v", x.Steps)
	}
}

func TestGrowthSplitByToolResultSize(t *testing.T) {
	r := analyze(
		call("m1", 0, 10000, 0, 0, `[{"type":"tool_use","id":"r","name":"Read","input":{"file_path":"big.go"}},`+
			`{"type":"tool_use","id":"b","name":"Bash","input":{"command":"go test ./...\necho done"}},`+
			`{"type":"tool_use","id":"m","name":"mcp__jira__get","input":{}},{"type":"tool_use","id":"s","name":"Task","input":{}}]`),
		result("r", 3000, false), result("b", 1000, false), result("m", 500, false), result("s", 500, false),
		call("m2", 0, 15000, 0, 0, ""),
	)
	x := r.Runs[0]
	if x.Tools["Read"] != 3000 || x.Tools["Bash"] != 1000 || x.Tools["MCP: jira"] != 500 || x.Tools[core.SourceSubagents] != 500 {
		t.Fatalf("tools %v", x.Tools)
	}
	if len(x.Commands) != 1 || x.Commands[0].Name != "go test ./..." || x.Commands[0].Tokens != 1000 {
		t.Fatalf("commands %+v", x.Commands)
	}
}

func TestCompactions(t *testing.T) {
	r := analyze(
		call("m1", 0, 100000, 0, 0, ""),
		`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":100000}}`,
		call("m2", 0, 20000, 0, 0, ""), // the boundary's drop: one compaction, no growth
		call("m3", 0, 90000, 0, 0, ""),
		call("m4", 0, 30000, 0, 0, ""), // a sharp drop with no boundary
		call("m5", 0, 31000, 0, 0, ""),
	)
	x := r.Runs[0]
	if x.Compactions != 2 || fmt.Sprint(x.Compacted) != "[1 3]" || x.Peak != 100000 {
		t.Fatalf("%+v", x)
	}
	if x.Tools[core.SourceOther] != 70000+1000 {
		t.Fatalf("tools %v", x.Tools)
	}
}

func TestPlanThenWork(t *testing.T) {
	r := analyze(
		call("m1", 0, 10000, 0, 0, use("p1", "ExitPlanMode", `{"plan":"x"}`)),
		result("p1", 10, true), // revised: still the plan
		call("m2", 0, 11000, 0, 0, use("p2", "ExitPlanMode", `{"plan":"y"}`)),
		result("p2", 10, false),
		call("m3", 0, 12000, 0, 0, ""),
	)
	if len(r.Runs) != 2 || r.Runs[0].Phase != core.PhasePlan || r.Runs[1].Phase != core.PhaseWork {
		t.Fatalf("%+v", r.Runs)
	}
	if r.Runs[0].Calls != 2 || r.Runs[1].Start != 12000 || r.Runs[1].Tools["ExitPlanMode"] != 1000 {
		t.Fatalf("%+v", r.Runs)
	}
}

func TestSubagentsCountOnlyInCache(t *testing.T) {
	r := analyze(
		call("m1", 0, 10000, 0, 0, ""),
		`{"type":"assistant","parent_tool_use_id":"t9","message":{"id":"sub1","usage":{"input_tokens":5,"cache_read_input_tokens":500000,"cache_creation_input_tokens":0}}}`,
		`{"type":"assistant","isSidechain":true,"message":{"id":"sub2","usage":{"input_tokens":5,"cache_read_input_tokens":1,"cache_creation_input_tokens":0}}}`,
		call("m2", 0, 10500, 0, 0, ""),
	)
	x := r.Runs[0]
	if x.Peak != 10500 || x.Calls != 2 || x.CacheRead != 10000+500000+1+10500 || x.Input != 10 {
		t.Fatalf("%+v", x)
	}
}

func TestTopAndDownsample(t *testing.T) {
	var lines []string
	for i := 0; i < 300; i++ {
		id := fmt.Sprintf("t%d", i)
		lines = append(lines, call(fmt.Sprintf("m%d", i), 0, 1000+i*100, 0, 0, use(id, "Read", fmt.Sprintf(`{"file_path":"f%d.go"}`, i%15))))
		lines = append(lines, result(id, 100, false))
	}
	x := analyze(lines...).Runs[0]
	if len(x.Files) != core.TopHeavy {
		t.Fatalf("files %d", len(x.Files))
	}
	if len(x.Steps) > maxSteps || x.Steps[len(x.Steps)-1] != 1000+299*100 || x.Calls != 300 {
		t.Fatalf("steps %d, last %d", len(x.Steps), x.Steps[len(x.Steps)-1])
	}
}

func TestSubagentFilesAddToTheirRun(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "s-1.jsonl")
	at := func(line, ts string) string { return line[:len(line)-1] + `,"timestamp":"` + ts + `"}` }
	os.WriteFile(main, []byte(strings.Join([]string{
		at(call("m1", 0, 10000, 0, 0, use("p", "ExitPlanMode", `{}`)), "2026-10-01T10:00:00Z"),
		at(result("p", 10, false), "2026-10-01T10:05:00Z"),
		at(call("m2", 0, 11000, 0, 0, ""), "2026-10-01T10:06:00Z"),
	}, "\n")), 0o600)
	subs := filepath.Join(dir, "s-1", "subagents")
	os.MkdirAll(subs, 0o700)
	sub := func(name, ts string, read int) {
		os.WriteFile(filepath.Join(subs, name), []byte(fmt.Sprintf(`{"type":"user","timestamp":%q,"message":{"content":"look"}}`+"\n"+
			`{"type":"assistant","timestamp":%q,"message":{"id":%q,"usage":{"input_tokens":1,"cache_read_input_tokens":%d,"cache_creation_input_tokens":0}}}`,
			ts, ts, name, read)), 0o600)
	}
	sub("agent-a.jsonl", "2026-10-01T10:01:00Z", 500) // during the plan
	sub("agent-b.jsonl", "2026-10-01T10:07:00Z", 700) // during the work
	r := AnalyzeFile(main, core.PhaseWork)
	if len(r.Runs) != 2 || r.Runs[0].CacheRead != 10000+500 || r.Runs[1].CacheRead != 11000+700 || r.Runs[0].Input != 1 {
		t.Fatalf("%+v", r.Runs)
	}
}

// A subagent that ran after the plan's approval, before the work's first call: its cache goes to the plan's run,
// not to a run with no call that is not kept.
func TestSubagentBeforeTheWorksFirstCall(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "s-1.jsonl")
	os.WriteFile(main, []byte(strings.Join([]string{
		call("m1", 0, 10000, 0, 0, use("p", "ExitPlanMode", `{}`)),
		strings.TrimSuffix(result("p", 10, false), "}") + `,"timestamp":"2026-10-01T10:05:00Z"}`,
	}, "\n")), 0o600)
	os.MkdirAll(filepath.Join(dir, "s-1", "subagents"), 0o700)
	os.WriteFile(filepath.Join(dir, "s-1", "subagents", "agent-a.jsonl"), []byte(
		`{"type":"assistant","timestamp":"2026-10-01T10:06:00Z","message":{"id":"x","usage":{"input_tokens":1,"cache_read_input_tokens":800}}}`), 0o600)
	r := AnalyzeFile(main, core.PhaseWork)
	if len(r.Runs) != 1 || r.Runs[0].Phase != core.PhasePlan || r.Runs[0].CacheRead != 10800 {
		t.Fatalf("%+v", r.Runs)
	}
}
