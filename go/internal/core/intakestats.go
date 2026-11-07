package core

import "time"

// IntakeStats: how the intake decided on the tasks taken, and what keeping some of them from the agent saved.
type IntakeStats struct {
	// Ran: the intakes that came to a decision or failed; by decision; Failed: the ones that went to work without one.
	Ran     int `json:"ran"`
	Take    int `json:"take"`
	Clarify int `json:"clarify"`
	Answer  int `json:"answer"`
	Split   int `json:"split"`
	Failed  int `json:"failed"`
	// Filtered: kept from the agent (clarify, answer, split) and not taken anyway; Overridden: taken anyway.
	Filtered   int `json:"filtered"`
	Overridden int `json:"overridden"`
	// Cost: USD the intakes spent. Avoided: what the filtered tasks would have cost the agent, by the median
	// cost of the merged tasks of their size (Priced: the filtered ones such a median is known for). Saved:
	// Avoided less Cost, the intake paid for by itself or not.
	Cost    float64 `json:"cost"`
	Avoided float64 `json:"avoided"`
	Priced  int     `json:"priced"`
	Saved   float64 `json:"saved"`
}

// SummarizeIntake counts the intakes of the records since (zero: all). A task the intake kept from the agent was
// never taken, so Summarize does not see it: this looks at every record with an intake.
func SummarizeIntake(records []Record, since time.Time) IntakeStats {
	var st IntakeStats
	costs := map[string][]float64{}
	var all []float64
	for _, r := range records {
		if r.Status == Done && r.ReviewState == "merged" && r.Cost > 0 {
			s := sizeOfRecord(r)
			costs[s] = append(costs[s], r.Cost)
			all = append(all, r.Cost)
		}
	}
	for _, r := range records {
		i := r.Intake
		if i == nil || i.State == "" {
			continue
		}
		if t, err := time.Parse(time.RFC3339Nano, i.At); !since.IsZero() && (err != nil || t.Before(since)) {
			continue
		}
		st.Ran++
		if r.Metrics != nil {
			st.Cost += r.Metrics.Cost[PhaseIntake]
		}
		if i.State != "done" {
			st.Failed++
			continue
		}
		switch i.Decision {
		case IntakeTake:
			st.Take++
		case IntakeClarify:
			st.Clarify++
		case IntakeAnswer:
			st.Answer++
		case IntakeSplit:
			st.Split++
		}
		if i.Overridden {
			st.Overridden++
		}
		if !i.Filtered() {
			continue
		}
		st.Filtered++
		xs := costs[sizeOfRecord(r)]
		if len(xs) == 0 {
			xs = all
		}
		if len(xs) > 0 {
			st.Avoided += median(xs)
			st.Priced++
		}
	}
	st.Saved = st.Avoided - st.Cost
	return st
}

// sizeOfRecord: the size the estimate gave the task, "medium" without one, as the routes take it.
func sizeOfRecord(r Record) string {
	if e := r.Estimate; e != nil && e.State == "done" && e.Size != "" {
		return e.Size
	}
	return "medium"
}
