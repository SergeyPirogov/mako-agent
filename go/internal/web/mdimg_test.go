package web

import (
	"os/exec"
	"strings"
	"testing"
)

// A GitLab issue writes its pictures without the host: ![image](/uploads/<secret>/image.png){width=632 height=600}.
// The board shows them through Mako (which has the token), takes the {…} size suffix away, and leaves such a
// picture as text where it does not know the task's tracker.
func TestBoardTrackerPictures(t *testing.T) {
	src := string(board)
	from, to := strings.Index(src, "// md:begin"), strings.Index(src, "// md:end")
	if from < 0 || to < from {
		t.Fatal("board.html has no md:begin … md:end block")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is not installed")
	}
	script := `const esc = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c]));
` + src[from:to] + `
const eq = (got, want, what) => { if (got !== want) { console.log(what + ": got " + JSON.stringify(got) + ", want " + JSON.stringify(want)); process.exitCode = 1; } };
const task = { trackerId: "gl", project: "ecom/tests/moq" };
const secret = "35e4b8215c97b932f4083d09e557b2d0";
const proxied = "/api/tracker-image?tracker=gl&project=ecom%2Ftests%2Fmoq&path=" + encodeURIComponent("/uploads/" + secret + "/image.png");
const html = md("![image](/uploads/" + secret + "/image.png){width=632 height=600}", task);
eq(html, '<p><a href="' + proxied.replace(/&/g, "&amp;") + '" target="_blank" rel="noopener"><img class="pic" src="' + proxied.replace(/&/g, "&amp;") + '" alt="image" loading="lazy"></a></p>', "a GitLab upload with a size");
eq(md("before ![a](/uploads/" + secret + "/a.png) after", task).includes("{width"), false, "no suffix");
eq(md("![image](/uploads/" + secret + "/image.png)", task).includes("<img"), true, "without a size");
eq(md("![image](/uploads/" + secret + "/image.png){width=1 height=2}").includes("<img"), false, "no task: stays text");
eq(md("![image](/uploads/" + secret + "/image.png)", { trackerId: "gl" }).includes("<img"), false, "no project: stays text");
eq(md("![x](https://example.com/x.png)", task).includes('src="https://example.com/x.png"'), true, "an absolute picture is as it was");
eq(md("![x](/api/screenshots/t/a.png)").includes('src="/api/screenshots/t/a.png"'), true, "Mako's own picture is as it was");
`
	out, err := exec.Command(node, "-e", script).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
}
