package web

import (
	"encoding/json"
	"strings"
	"testing"
)

// «Always allow» keeps its rules in the settings for every agent after this one: added to each terminal executor's
// allowedTools once, the rest of the settings as they were; an Orca executor gets none.
func TestAllowInAddsRulesOnce(t *testing.T) {
	var raw map[string]json.RawMessage
	json.Unmarshal([]byte(`{"defaultExecutor":"terminal","executors":{
		"terminal":{"type":"terminal","permissionMode":"auto","allowedTools":["Bash(git add:*)","WebFetch"]},
		"orca":{"type":"orca","command":"orca"}},"wipLimit":3}`), &raw)
	if err := allowIn(raw, []string{"Bash(git add:*)", "Bash(git commit:*)", "Bash(git commit:*)"}); err != nil {
		t.Fatal(err)
	}
	var ex map[string]map[string]any
	json.Unmarshal(raw["executors"], &ex)
	got, _ := json.Marshal(ex["terminal"]["allowedTools"])
	if string(got) != `["Bash(git add:*)","WebFetch","Bash(git commit:*)"]` {
		t.Errorf("allowedTools %s", got)
	}
	if ex["terminal"]["permissionMode"] != "auto" || ex["orca"]["allowedTools"] != nil || string(raw["wipLimit"]) != "3" {
		t.Errorf("the rest changed: %s %s", raw["executors"], raw["wipLimit"])
	}
	// No allowedTools yet: made.
	json.Unmarshal([]byte(`{"executors":{"t":{"type":"terminal"}}}`), &raw)
	if err := allowIn(raw, []string{"Bash(go test:*)"}); err != nil || !strings.Contains(string(raw["executors"]), `"allowedTools":["Bash(go test:*)"]`) {
		t.Errorf("%v %s", err, raw["executors"])
	}
}

// No terminal executor, or settings Mako cannot read: an error that says what to do, nothing written.
func TestAllowInErrors(t *testing.T) {
	for _, cfg := range []string{`{}`, `{"executors":{"orca":{"type":"orca"}}}`} {
		var raw map[string]json.RawMessage
		json.Unmarshal([]byte(cfg), &raw)
		if err := allowIn(raw, []string{"Bash(ls:*)"}); err == nil || !strings.Contains(err.Error(), "Settings → Commands without asking") {
			t.Errorf("%s: %v", cfg, err)
		}
	}
	raw := map[string]json.RawMessage{"executors": json.RawMessage(`{"t":{"type":"terminal","allowedTools":"git"}}`)}
	if err := allowIn(raw, []string{"Bash(ls:*)"}); err == nil || !strings.Contains(err.Error(), "executors.t.allowedTools") {
		t.Errorf("a bad allowedTools: %v", err)
	}
}
