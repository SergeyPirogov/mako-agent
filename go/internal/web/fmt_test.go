package web

import (
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The board writes every moment and every length of time through fmtWhen / fmtDur / fmtDay: the block that
// defines them runs here in Node, and no other toLocale* call is left in the board.
func TestBoardTimeFormats(t *testing.T) {
	src := string(board)
	from, to := strings.Index(src, "// fmt:begin"), strings.Index(src, "// fmt:end")
	if from < 0 || to < from {
		t.Fatal("board.html has no fmt:begin … fmt:end block")
	}
	if n := len(regexp.MustCompile(`toLocale\w*String\(`).FindAllString(src[:from]+src[to:], -1)); n != 0 {
		t.Errorf("board.html formats a date outside fmtWhen/fmtDay %d time(s)", n)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `const LANG = "en"; process.env.TZ;
` + src[from:to] + `
const now = new Date(2026, 10, 3, 15, 0); // Tue, Nov 3, 2026 15:00 (local)
const at = (y, m, d, h, mi) => new Date(y, m, d, h, mi);
const eq = (got, want, what) => { if (got !== want) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
eq(fmtWhen(at(2026, 10, 3, 11, 19), { today: now }), "11:19", "today");
eq(fmtWhen(at(2026, 10, 3, 14, 5), { today: now }), "14:05", "today, 24 hours");
eq(fmtWhen(at(2026, 10, 3, 0, 0), { today: now }), "00:00", "midnight");
eq(fmtWhen(at(2026, 10, 2, 9, 30), { today: now }), "Mon 09:30", "yesterday");
eq(fmtWhen(at(2026, 10, 5, 9, 30), { today: now }), "Thu 09:30", "in two days");
eq(fmtWhen(at(2026, 9, 27, 18, 0), { today: now }), "Oct 27, 18:00", "a week ago");
eq(fmtWhen(at(2026, 10, 9, 18, 0), { today: now }), "Mon 18:00", "six days ahead is still this week");
eq(fmtWhen(at(2026, 10, 10, 18, 0), { today: now }), "Nov 10, 18:00", "a week ahead");
eq(fmtWhen(at(2025, 10, 3, 11, 19), { today: now }), "Nov 3, 2025, 11:19", "another year");
eq(fmtWhen(at(2026, 10, 3, 11, 19).toISOString(), { today: now }), "11:19", "an ISO string");
eq(fmtWhen(at(2026, 10, 3, 11, 19).getTime(), { today: now }), "11:19", "milliseconds");
eq(fmtWhen("", { today: now }), "", "empty");
eq(fmtWhen("not a date", { today: now }), "", "garbage");
eq(fmtDay("2026-11-03"), "Nov 3", "a day");
eq(fmtDay("2026-11-03", true), "Tue, Nov 3", "a day, long");
eq(fmtDur(0), "just now", "zero");
eq(fmtDur(29), "just now", "under half a minute");
eq(fmtDur(60), "1 min", "a minute");
eq(fmtDur(42 * 60), "42 min", "minutes");
eq(fmtDur(4 * 3600), "4 h", "no 0 min");
eq(fmtDur(3 * 3600 + 5 * 60), "3 h 5 min", "hours and minutes");
eq(fmtDur(2 * 86400), "2 d", "no 0 h");
eq(fmtDur(2 * 86400 + 4 * 3600), "2 d 4 h", "days and hours");
eq(fmtDur(-5), "just now", "negative");
eq(fmtDur(38, { fine: 60 }), "38 s", "seconds");
eq(fmtDur(0, { fine: 60 }), "0 s", "zero seconds");
eq(fmtDur(6 * 60, { fine: 60 }), "6 min", "fine: minutes past the limit");
eq(fmtDur(76, { fine: 600 }), "1 min 16 s", "fine: minutes and seconds");
eq(fmtDur(120, { fine: 600 }), "2 min", "fine: no 0 s");
eq(fmtDur(3600, { fine: 600 }), "1 h", "fine: past the limit");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
