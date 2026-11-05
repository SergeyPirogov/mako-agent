package web

import (
	"strings"
	"testing"
)

// «How to spend less»: a route that does worse than another tried for the same size is pointed out with what the
// period would have saved; a weak route in use is too; big tasks that take the money out of proportion are; a
// route that does well, or one not in use, is not.
func TestCostAdvice(t *testing.T) {
	src := string(board)
	from, to := strings.Index(src, "// cost-advice:begin"), strings.Index(src, "// cost-advice:end")
	if from < 0 || to < from {
		t.Fatal("no cost-advice:begin … cost-advice:end block in board.html")
	}
	runNodeScript(t, nodeEq+`const money = (x) => "$" + (x || 0).toFixed(2), cap = (s) => s ? s[0].toUpperCase() + s.slice(1) : s;
const SIZE_NAMES = { small: "small", medium: "medium", big: "big" };
`+src[from:to]+`
const w = (size, model, effort, taken, ended, clean, cost) => ({ phase: "work", size, model, effort, taken, ended, clean, cost, priced: taken });
const now = { work: { small: { model: "sonnet", effort: "medium" }, medium: { model: "sonnet", effort: "high" }, big: { model: "sonnet", effort: "high" } } };
const routes = [w("small", "sonnet", "medium", 23, 23, 21, 0.43), w("medium", "sonnet", "high", 128, 128, 111, 0.79),
  w("big", "sonnet", "high", 7, 7, 0, 24.46), w("big", "opus", "high", 14, 14, 8, 9.05),
  { phase: "review", size: "big", model: "opus", effort: "medium", taken: 11, cost: 0.85, priced: 11 }];
const bySize = [{ size: "small", priced: 34, cost: 0.38, ended: 34, clean: 28 }, { size: "medium", priced: 48, cost: 1.42, ended: 48, clean: 32 }, { size: "big", priced: 37, cost: 12.46, ended: 36, clean: 9 }];
let a = costAdvice({ routes, bySize }, now);
eq(a.map((x) => x.title), ["Big tasks · Work: Opus · high is cheaper and does better", "Big tasks take 85% of the spend"], "titles, the saving first");
eq(Math.round(a[0].save * 100) / 100, 107.87, "what 7 tasks would have saved");
eq(a[0].rows.map((r) => r[2]), [true, false], "the route in use marked");

// Opus is the route now: nothing better tried, it does well enough.
a = costAdvice({ routes }, { work: { ...now.work, big: { model: "opus", effort: "high" } } });
eq(a.length, 0, "the better route in use");
// A route in use with no record, or too few finished tasks: nothing to say.
eq(costAdvice({ routes }, { work: { big: { model: "haiku", effort: "low" } } }).length, 0, "no record of the route in use");
eq(costAdvice({ routes: [w("big", "sonnet", "high", 2, 2, 0, 30), w("big", "opus", "high", 2, 2, 2, 5)] }, now).length, 0, "too few tasks");
// Weak, and nothing else tried: raise it.
a = costAdvice({ routes: [w("medium", "sonnet", "high", 10, 10, 3, 1)] }, now);
eq(a.map((x) => x.title), ["Medium tasks · Work: Sonnet · high gets 30% through first time"], "a weak route");
// As good and much cheaper: the cheaper one.
a = costAdvice({ routes: [w("medium", "sonnet", "high", 10, 10, 9, 1), w("medium", "sonnet", "medium", 10, 10, 9, 0.5)] }, now);
eq(a.map((x) => x.title), ["Medium tasks · Work: Sonnet · medium is cheaper, as good"], "cheaper, as good");
// Cheaper but worse: not offered.
eq(costAdvice({ routes: [w("medium", "sonnet", "high", 10, 10, 9, 1), w("medium", "haiku", "high", 10, 10, 4, 0.2)] }, now).length, 0, "cheaper but worse");
// Big tasks in proportion to their size: no tip.
eq(costAdvice({ bySize: [{ size: "medium", priced: 10, cost: 1 }, { size: "big", priced: 2, cost: 2 }] }, now).length, 0, "big in proportion");
eq(costAdvice({}, undefined).length, 0, "no data");
`)
}
