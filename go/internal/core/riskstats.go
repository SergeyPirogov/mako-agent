package core

import (
	"regexp"
	"strings"
	"time"
)

// RiskStats: the merged tasks of the period by the change's risk, and how many merged by themselves (a low risk with
// autoMergeLow) and needed a fix after: many fixes after auto-merges say the policy is too soft.
type RiskStats struct {
	// Merged: the merged tasks with a level known; Low…Block: of them, by the level at the merge (an older task:
	// the level Mako reads of it now).
	Merged int `json:"merged"`
	Low    int `json:"low"`
	Medium int `json:"medium"`
	High   int `json:"high"`
	Block  int `json:"block"`
	// AutoMerged: merged by Mako itself; AutoMergedFixed: of those, the ones a later bug-fix or revert task points at.
	AutoMerged      int `json:"autoMerged"`
	AutoMergedFixed int `json:"autoMergedFixed"`
}

// riskStats: the merged tasks of the period (since; zero: all) out of records; the later fixes are looked for among
// all of them.
func riskStats(records []Record, since time.Time) RiskStats {
	var st RiskStats
	var fixes []Record
	for _, r := range records {
		if isFix(r) || isRevert(r) {
			fixes = append(fixes, r)
		}
	}
	for _, r := range records {
		if r.ReviewState != "merged" || !since.IsZero() && !takenSince(r, since) {
			continue
		}
		level := RiskLevel(r.RiskAtMerge)
		if level == "" {
			level, _ = RiskOf(r)
		}
		switch level {
		case RiskLow:
			st.Low++
		case RiskMedium:
			st.Medium++
		case RiskHigh:
			st.High++
		case RiskBlock:
			st.Block++
		default:
			continue
		}
		st.Merged++
		if r.MergedBy == "auto" {
			st.AutoMerged++
			if fixedAfter(r, fixes) {
				st.AutoMergedFixed++
			}
		}
	}
	return st
}

// revertWord: a task that undoes a merge, by its labels or its title ("Revert «…»", "Rollback the cart").
var revertWord = regexp.MustCompile(`\b(revert|rollback)\b`)

// isRevert: the task undoes something merged, by its title or its labels.
func isRevert(r Record) bool {
	if revertWord.MatchString(strings.ToLower(strings.TrimSpace(r.Task.Title))) {
		return true
	}
	for _, l := range r.Task.Labels {
		if revertWord.MatchString(strings.ToLower(l)) {
			return true
		}
	}
	return false
}
