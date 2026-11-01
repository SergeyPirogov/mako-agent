package core

import (
	"testing"
	"time"
)

func TestOpenRemarks(t *testing.T) {
	rounds := []AgentReview{
		{State: "notes", Remarks: []Remark{{ID: "R1", Weight: Must}, {ID: "R2", Weight: Should}, {ID: "R3", Weight: Must}}},
		{State: "notes", Checks: []Check{{ID: "R1", Verdict: Closed}, {ID: "R2", Verdict: Accepted}, {ID: "R3", Verdict: Open}},
			Remarks: []Remark{{ID: "R4", Weight: Must}, {ID: "R3", Weight: Must, Again: true, Reply: Fixed}}},
	}
	open := OpenRemarks(rounds)
	if len(open) != 2 || open[0].ID != "R3" || !open[0].Again || open[0].Reply != Fixed || open[1].ID != "R4" {
		t.Fatalf("open %+v", open)
	}
	if NextRemark(rounds) != 5 || NextRemark(nil) != 1 {
		t.Fatalf("next %d", NextRemark(rounds))
	}
	// A free-text round of an older Mako does not count against the rounds; a clean one does.
	if n := ReviewedRounds(append([]AgentReview{{State: "notes", Notes: "- щось"}, {State: "failed"}}, rounds...)); n != 2 {
		t.Fatalf("rounds %d", n)
	}
}

func TestReviewStats(t *testing.T) {
	at := time.Now().UTC().Format(time.RFC3339Nano)
	records := []Record{
		{ID: "a", StartedAt: at, AgentReviews: []AgentReview{
			{State: "notes", Sent: true, ReplyAt: at, Remarks: []Remark{{ID: "R1", Weight: Must, Reply: Fixed}, {ID: "R2", Weight: Should, Reply: NotFixed}, {ID: "R3", Weight: Should}}},
			{State: "clean", Checks: []Check{{ID: "R1", Verdict: Closed}, {ID: "R2", Verdict: Accepted}}},
		}},
		{ID: "b", StartedAt: at, AgentReviews: []AgentReview{
			{State: "notes", Unverified: true, Sent: true, ReplyAt: at, Remarks: []Remark{{ID: "R1", Weight: Must, Reply: Partly}}},
			{State: "notes", Escalated: true, Checks: []Check{{ID: "R1", Verdict: Open}}, Remarks: []Remark{{ID: "R1", Weight: Must, Again: true}}},
		}},
		{ID: "c", StartedAt: at, AgentReviews: []AgentReview{{State: "notes", Remarks: []Remark{{ID: "R1", Weight: Should}}}}},
		{ID: "d", StartedAt: at, AgentReviews: []AgentReview{{State: "failed"}}},
	}
	s := Summarize(records, time.Now(), time.Time{}).Review
	want := ReviewStats{Rounds: 5, Clean: 1, Remarks: 5, Must: 2, Fixed: 1, NotFixed: 1, Partly: 1, NoReply: 1, Closed: 1, Open: 1, Accepted: 1,
		Unverified: 1, Waiting: 2, Escalated: 1, Tasks: 3, Converged: 1, ToClean: 2, PerRound: 1}
	if s != want {
		t.Fatalf("got  %+v\nwant %+v", s, want)
	}
}
