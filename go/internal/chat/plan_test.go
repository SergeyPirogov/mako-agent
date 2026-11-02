package chat

import (
	"strings"
	"testing"
)

func TestParsePlanList(t *testing.T) {
	doc := ParsePlan("Here is the plan.\n\n1. **Filter by project** — a `project` parameter in `core/stats.go`\n   - and in `web/web.go`\n2. Relative paths\n   in `core/context.go`\n10) Check: `go test ./...`\n\n## Risks\nboard.html is shared\n")
	if doc.Intro != "Here is the plan." || len(doc.Steps) != 3 || doc.Risk != "board.html is shared" {
		t.Fatalf("%+v", doc)
	}
	s := doc.Steps[0]
	if s.N != 1 || s.Title != "Filter by project" || s.Body != "a `project` parameter in `core/stats.go`\n- and in `web/web.go`" || strings.Join(s.Files, " ") != "core/stats.go web/web.go" {
		t.Fatalf("step 1 %+v", s)
	}
	if doc.Steps[1].Body != "in `core/context.go`" || doc.Steps[2].N != 3 || doc.Steps[2].Title != "Check: `go test ./...`" || doc.Files != 3 {
		t.Fatalf("%+v", doc)
	}
}

func TestParsePlanHeadings(t *testing.T) {
	doc := ParsePlan("# Plan\n\n### 1. Page shell\n1. header\n2. tabs\n\n```\n## 9. not a step\n```\n### Step 2: Overview\ntext\n\n**Risk:** none\n")
	if len(doc.Steps) != 2 || doc.Steps[0].Title != "Page shell" || !strings.Contains(doc.Steps[0].Body, "2. tabs") || !strings.Contains(doc.Steps[0].Body, "## 9. not a step") {
		t.Fatalf("%+v", doc)
	}
	if doc.Steps[1].Title != "Overview" || doc.Steps[1].Body != "text" || doc.Risk != "none" || doc.Intro != "# Plan" {
		t.Fatalf("%+v", doc)
	}
}

func TestParsePlanNoSteps(t *testing.T) {
	if doc := ParsePlan("Just change the colour in `style.css`."); len(doc.Steps) != 0 || doc.Intro != "" {
		t.Fatalf("%+v", doc)
	}
}

func TestPlanNotesText(t *testing.T) {
	n := PlanNotes{Steps: []StepNote{{N: 3, Title: "Page shell", Note: " keep the period in the URL "}, {N: 4, Note: ""}}, Whole: "add a test"}
	if got := n.Text(); got != "Step 3 (Page shell): keep the period in the URL\n\nOn the whole plan: add a test" {
		t.Fatalf("%q", got)
	}
	if (PlanNotes{Whole: " x "}).Text() != "x" || (PlanNotes{}).Text() != "" {
		t.Fatal("whole only")
	}
}
