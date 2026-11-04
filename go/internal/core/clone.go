package core

import (
	"maps"
	"slices"
)

// Clone returns a deep copy of the record: no pointer, slice or map of the copy is shared with r. The state store
// hands out clones, so a caller reading one never races with an Update changing the stored record.
//
// A nil slice or map stays nil and an empty one stays empty, as the JSON of a record tells them apart.
// A field added to Record that holds a pointer, slice or map must be copied here: TestRecordCloneIsDeep fails
// until it is.
func (r Record) Clone() Record {
	r.Task = r.Task.deep()
	r.TrackerStatus = clonePtr(r.TrackerStatus, func(v TrackerState) TrackerState { return v })
	r.AgentRoute = clonePtr(r.AgentRoute, func(v Route) Route { return v })
	r.Lesson = clonePtr(r.Lesson, func(v Lesson) Lesson { return v })
	r.AutoFixed = maps.Clone(r.AutoFixed)
	r.BaseConflict = clonePtr(r.BaseConflict, func(v BaseConflict) BaseConflict {
		v.Files = slices.Clone(v.Files)
		return v
	})
	r.EscalatedFor = slices.Clone(r.EscalatedFor)
	r.Metrics = clonePtr(r.Metrics, Metrics.deep)
	r.AgentReviews = cloneEach(r.AgentReviews, AgentReview.deep)
	r.Assessment = clonePtr(r.Assessment, func(v Assessment) Assessment {
		v.Review = clonePtr(v.Review, func(a ReviewAdvice) ReviewAdvice {
			a.Steps = slices.Clone(a.Steps)
			return a
		})
		return v
	})
	r.SelfCheck = clonePtr(r.SelfCheck, func(v SelfCheck) SelfCheck {
		v.Steps = slices.Clone(v.Steps)
		v.Tried = clonePtr(v.Tried, func(m SelfMark) SelfMark { return m })
		v.Skipped = clonePtr(v.Skipped, func(k SelfSkip) SelfSkip { return k })
		v.Synced = slices.Clone(v.Synced)
		return v
	})
	r.Retro = clonePtr(r.Retro, func(v Retro) Retro { return v })
	r.Estimate = clonePtr(r.Estimate, func(v Estimate) Estimate { return v })
	r.Secrets = slices.Clone(r.Secrets)
	r.SecretsAllowed = slices.Clone(r.SecretsAllowed)
	r.Checks = clonePtr(r.Checks, func(v Checks) Checks {
		v.Results = slices.Clone(v.Results)
		return v
	})
	r.Changed = clonePtr(r.Changed, func(v ChangeStat) ChangeStat { return v })
	return r
}

func (t Task) deep() Task {
	t.Labels = slices.Clone(t.Labels)
	t.Components = slices.Clone(t.Components)
	t.Comments = slices.Clone(t.Comments)
	t.Assignees = slices.Clone(t.Assignees)
	t.Attachments = slices.Clone(t.Attachments)
	return t
}

func (m Metrics) deep() Metrics {
	m.Seconds = maps.Clone(m.Seconds)
	m.Cost = maps.Clone(m.Cost)
	m.Tokens = maps.Clone(m.Tokens)
	m.Context = cloneEach(m.Context, ContextRun.deep)
	m.Routes = maps.Clone(m.Routes)
	m.Usage = maps.Clone(m.Usage)
	m.UsageCost = maps.Clone(m.UsageCost)
	return m
}

func (c ContextRun) deep() ContextRun {
	c.Tools = maps.Clone(c.Tools)
	c.Files = slices.Clone(c.Files)
	c.Commands = slices.Clone(c.Commands)
	c.Steps = slices.Clone(c.Steps)
	c.Compacted = slices.Clone(c.Compacted)
	return c
}

func (a AgentReview) deep() AgentReview {
	a.Remarks = slices.Clone(a.Remarks)
	a.Checks = slices.Clone(a.Checks)
	return a
}

// clonePtr copies what p points to with copyOf, which copies the value's own pointers, slices and maps.
func clonePtr[T any](p *T, copyOf func(T) T) *T {
	if p == nil {
		return nil
	}
	v := copyOf(*p)
	return &v
}

// cloneEach is slices.Clone with each element copied deep; nil stays nil.
func cloneEach[T any](s []T, copyOf func(T) T) []T {
	if s == nil {
		return nil
	}
	out := make([]T, len(s))
	for i, v := range s {
		out[i] = copyOf(v)
	}
	return out
}
