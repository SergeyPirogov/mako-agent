package web

import (
	"os/exec"
	"strings"
	"testing"
)

// cutBlock: the part of a page between "// <name>:begin" and "// <name>:end".
func cutBlock(t *testing.T, page []byte, name string) string {
	t.Helper()
	src := string(page)
	from, to := strings.Index(src, "// "+name+":begin"), strings.Index(src, "// "+name+":end")
	if from < 0 || to < from {
		t.Fatalf("no %s:begin … %s:end block", name, name)
	}
	return src[from:to]
}

func runNodeScript(t *testing.T, script string) {
	t.Helper()
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil || len(out) > 0 {
		t.Fatalf("%v\n%s", err, out)
	}
}

const nodeEq = `const eq = (got, want, what) => { if (JSON.stringify(got) !== JSON.stringify(want)) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
const esc = (s) => String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/"/g, "&quot;");
`

// A request's «Always allow» shows the rules it would keep, ticked and editable; a change to one stays with the
// request (the board redraws), and a request that suggests a mode, not rules, has no list.
func TestBoardPermRulePicks(t *testing.T) {
	runNodeScript(t, `const document = { addEventListener() {} };
`+nodeEq+cutBlock(t, board, "perm-rules")+`
const it = { id: "r1", suggestions: [{ type: "addRules", rules: [{ toolName: "Bash", ruleContent: "cd:*" }, { toolName: "Bash", ruleContent: "go vet:*" }, { toolName: "WebFetch" }] }] };
const html = permRulesHTML(it);
eq((html.match(/type="checkbox"/g) || []).length, 3, "a box for each rule");
eq(html.includes('value="Bash(go vet:*)"') && html.includes('value="WebFetch"'), true, "the rules as --allowedTools takes them");
const box = { dataset: { permRules: "r1" } };
const ev = (ds, extra) => ({ target: { dataset: ds, closest: () => box, ...extra } });
permRuleInput(ev({ ruleOn: "0" }, { checked: false }));
permRuleInput(ev({ rule: "1" }, { value: "Bash(go:*)" }));
eq(permPicks.r1, [{ rule: "Bash(cd:*)", on: false }, { rule: "Bash(go:*)", on: true }, { rule: "WebFetch", on: true }], "the edits kept");
eq(permRulesHTML(it).includes('value="Bash(go:*)"') && !permRulesHTML(it).includes('data-rule-on="0" checked'), true, "redrawn as edited");
eq(permRulesHTML({ id: "r2", suggestions: [{ type: "setMode", mode: "acceptEdits" }] }), "", "a mode, not rules");
eq(permRulesHTML({ id: "r3" }), "", "no suggestion");
permRuleInput({ target: { dataset: {}, closest: () => null } }); // a field of another card: nothing
`)
}

// Settings → Permissions: what each button asks of the server; an edited rule is kept in place of the one asked,
// which is dismissed with it.
func TestSettingsPermActions(t *testing.T) {
	runNodeScript(t, nodeEq+`const t = (k) => k, plural = (n, k) => k, diskAgo = () => "now";
`+cutBlock(t, settingsPage, "perms")+`
const row = (rule, typed) => ({ dataset: { g: rule }, querySelector: () => ({ value: typed }) });
const btn = (ds, r) => ({ dataset: ds, closest: () => r });
eq(permAction(btn({ permAllow: "" }, row("Bash(go run x:*)", " Bash(go run:*) "))), { allow: ["Bash(go run:*)"], dismiss: ["Bash(go run x:*)"] }, "allow edited");
eq(permAction(btn({ permAllow: "" }, row("WebFetch", "WebFetch"))), { allow: ["WebFetch"], dismiss: [] }, "allow as asked");
eq(permAction(btn({ permDismiss: "" }, row("WebFetch", "x"))), { dismiss: ["WebFetch"] }, "dismiss");
eq(permAction(btn({ permRemove: "Bash(ls:*)" }, null)), { remove: ["Bash(ls:*)"] }, "remove");
eq(permAction(btn({}, null)), null, "another button");
const html = permGroupRow({ rule: 'Bash(echo "x":*)', count: 3, agents: 2, last: 1, example: "echo <b>" });
eq(html.includes('value="Bash(echo &quot;x&quot;:*)"') && html.includes("echo &lt;b>") && !html.includes("<b>"), true, "escaped");
eq(allowedRow("Bash(<x>:*)").includes("&lt;x>"), true, "allowed escaped");
`)
}
