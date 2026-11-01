package core

import "strconv"

// The reviewer agent's rounds over a task: its numbered remarks, the agent's answers on them and the next
// round's verdicts, and how they add up over the period.

// RemarkNum: the number in a remark's ID, 0 when it has none.
func RemarkNum(id string) int {
	if len(id) < 2 || id[0] != 'R' {
		return 0
	}
	n, _ := strconv.Atoi(id[1:])
	return n
}

// NextRemark: the number the next round's first remark gets: after every remark of the task's rounds.
func NextRemark(rounds []AgentReview) int {
	n := 0
	for _, a := range rounds {
		for _, r := range a.Remarks {
			n = max(n, RemarkNum(r.ID))
		}
	}
	return n + 1
}

// Verdicts: the last verdict on each remark, by ID, from the rounds that checked them.
func Verdicts(rounds []AgentReview) map[string]Check {
	out := map[string]Check{}
	for _, a := range rounds {
		for _, c := range a.Checks {
			out[c.ID] = c
		}
	}
	return out
}

// OpenRemarks: the remarks of the rounds no later round has closed or accepted, with the agent's answers: the
// ones the next round checks.
func OpenRemarks(rounds []AgentReview) []Remark {
	verdicts := Verdicts(rounds)
	var out []Remark
	at := map[string]int{}
	for _, a := range rounds {
		for _, r := range a.Remarks {
			if verdicts[r.ID].Resolved() {
				continue
			}
			if i, ok := at[r.ID]; ok { // carried over: the later round's, with the agent's later answer
				out[i] = r
				continue
			}
			at[r.ID] = len(out)
			out = append(out, r)
		}
	}
	return out
}

// Structured: the round keeps to the numbered format (rounds of older versions of Mako do not).
func (a AgentReview) Structured() bool { return len(a.Remarks) > 0 || len(a.Checks) > 0 }

// ReviewedRounds: the rounds that read the branch to the end, clean or with numbered remarks: what counts
// against ReviewRounds. Free-text rounds of older versions of Mako do not.
func ReviewedRounds(rounds []AgentReview) int {
	n := 0
	for _, a := range rounds {
		if a.State == "clean" || a.State == "notes" && a.Structured() {
			n++
		}
	}
	return n
}

// ReviewStats is how the reviewer's rounds went over the period.
type ReviewStats struct {
	Rounds  int `json:"rounds"`  // that read a branch to the end, clean or with remarks
	Clean   int `json:"clean"`   // of them, clean
	Remarks int `json:"remarks"` // numbered remarks written
	Must    int `json:"must"`
	// The agent's answers on the remarks.
	Fixed    int `json:"fixed"`
	NotFixed int `json:"notFixed"`
	Partly   int `json:"partly"`
	NoReply  int `json:"noReply"` // handed to the agent, with no answer on it in its report
	// The next rounds' verdicts.
	Closed   int `json:"closed"`
	Open     int `json:"open"`
	Accepted int `json:"accepted"`
	// Unverified: rounds the agent said it fixed with no new commit on the branch.
	Unverified int `json:"unverified"`
	// Waiting: tasks whose last round's remarks wait for «Виправити»; Escalated: ones left to a person.
	Waiting   int `json:"waiting"`
	Escalated int `json:"escalated"`
	// Tasks: with at least one round; Converged: of them, ending clean; ToClean: the mean rounds to it.
	Tasks     int     `json:"tasks"`
	Converged int     `json:"converged"`
	ToClean   float64 `json:"toClean"`
	PerRound  float64 `json:"perRound"` // remarks per round
}

type reviewStats struct {
	st      ReviewStats
	toClean int
}

func (s *reviewStats) add(r Record) {
	rounds := 0
	for _, a := range r.AgentReviews {
		if a.State != "notes" && a.State != "clean" {
			continue
		}
		rounds++
		s.st.Rounds++
		if a.State == "clean" {
			s.st.Clean++
		}
		for _, m := range a.Remarks {
			if !m.Again {
				s.st.Remarks++
			}
			if m.Weight == Must && !m.Again {
				s.st.Must++
			}
			switch m.Reply {
			case Fixed:
				s.st.Fixed++
			case NotFixed:
				s.st.NotFixed++
			case Partly:
				s.st.Partly++
			default:
				if a.Sent && a.ReplyAt != "" {
					s.st.NoReply++
				}
			}
		}
		for _, c := range a.Checks {
			switch c.Verdict {
			case Closed:
				s.st.Closed++
			case Open:
				s.st.Open++
			case Accepted:
				s.st.Accepted++
			}
		}
		if a.Unverified {
			s.st.Unverified++
		}
		if a.Escalated {
			s.st.Escalated++
		}
	}
	if rounds == 0 {
		return
	}
	s.st.Tasks++
	last := r.AgentReviews[len(r.AgentReviews)-1]
	if last.State == "notes" && !last.Sent {
		s.st.Waiting++
	}
	if last.State == "clean" {
		s.st.Converged++
		s.toClean += rounds
	}
}

func (s *reviewStats) done() ReviewStats {
	if s.st.Converged > 0 {
		s.st.ToClean = float64(s.toClean) / float64(s.st.Converged)
	}
	if s.st.Rounds > 0 {
		s.st.PerRound = float64(s.st.Remarks) / float64(s.st.Rounds)
	}
	return s.st
}
