package core

import (
	"path"
	"strconv"
	"strings"
)

// The risk of a task's change: low, medium or high, or block (the change touches a path the project forbids). It is
// the highest of what Mako reads from the branch's diff by itself (RiskLook: the paths it touches, its size, code
// without tests, removed files, dependencies, the files' history of reverts and fixes) and of the agent's own
// rating (Assessment.Risk). The level says how much of a person the merge needs: low may merge by itself, high
// waits for «Check it yourself», block does not merge.

const (
	RiskLow    = "low"
	RiskMedium = "medium"
	RiskHigh   = "high"
	RiskBlock  = "block"
)

// RiskLevels: the levels, the lowest first.
var RiskLevels = []string{RiskLow, RiskMedium, RiskHigh, RiskBlock}

// riskRank: how high the level is (0: none, or not a level).
func riskRank(level string) int {
	for i, l := range RiskLevels {
		if l == level {
			return i + 1
		}
	}
	return 0
}

// RiskLevel: level if Mako knows it, else "".
func RiskLevel(level string) string {
	if riskRank(level) > 0 {
		return level
	}
	return ""
}

// MaxRisk: the higher of two levels ("" is below low).
func MaxRisk(a, b string) string {
	if riskRank(b) > riskRank(a) {
		return b
	}
	return RiskLevel(a)
}

// Kinds of RiskSignal.
const (
	SignalPath       = "path"       // a path the project's riskPaths name
	SignalSize       = "size"       // lines or files above the thresholds
	SignalNoTests    = "tests"      // code changed with no test
	SignalRemoved    = "removed"    // files deleted or renamed
	SignalDeps       = "deps"       // dependencies: go.mod, package.json, lock files
	SignalHistory    = "history"    // the files were in reverts or fixes before
	SignalAssessment = "assessment" // the agent's rating
)

// RiskSignal: one reason for the level, as the MR card lists it: Text in English, the files it is about.
type RiskSignal struct {
	Kind  string   `json:"kind"`
	Level string   `json:"level"`
	Text  string   `json:"text"`
	Files []string `json:"files,omitempty"`
}

// RiskLook: what Mako read of the branch's diff by itself, at the branch's HEAD Commit; the agent's rating is not
// in it (see RiskOf). No signal: nothing in the diff says the change is risky.
type RiskLook struct {
	At      string       `json:"at"`
	Commit  string       `json:"commit"`
	Signals []RiskSignal `json:"signals,omitempty"`
}

// Level: the highest of the signals ("" none).
func (l *RiskLook) Level() string {
	level := ""
	if l != nil {
		for _, s := range l.Signals {
			level = MaxRisk(level, s.Level)
		}
	}
	return level
}

// Blocked: the signals that block the merge.
func (l *RiskLook) Blocked() []RiskSignal {
	var out []RiskSignal
	if l != nil {
		for _, s := range l.Signals {
			if s.Level == RiskBlock {
				out = append(out, s)
			}
		}
	}
	return out
}

// AssessedRisk: the level of the agent's rating of the change: 1–2 low, 3 medium, 4–5 high; "" while it rates, when
// it failed or did not rate the risk. Older assessments (without Review) count the same.
func AssessedRisk(a *Assessment) string {
	if a == nil || a.State != "done" {
		return ""
	}
	switch v := a.Risk.Value; {
	case v >= 4:
		return RiskHigh
	case v == 3:
		return RiskMedium
	case v >= 1:
		return RiskLow
	}
	return ""
}

// RiskOf: the task's level and why: the diff's signals and the agent's rating, the highest wins. "" when neither
// is there yet (the policy takes it as medium: the merge is yours).
func RiskOf(r Record) (string, []RiskSignal) {
	var signals []RiskSignal
	level := ""
	if r.Risk != nil {
		signals = append(signals, r.Risk.Signals...)
		level = r.Risk.Level()
	}
	if l := AssessedRisk(r.Assessment); l != "" {
		text := "Agent's assessment: risk " + strconv.Itoa(r.Assessment.Risk.Value) + " of 5"
		if why := strings.TrimSpace(r.Assessment.Risk.Why); why != "" {
			text += ": " + why
		}
		signals = append(signals, RiskSignal{Kind: SignalAssessment, Level: l, Text: text})
		level = MaxRisk(level, l)
	}
	return level, signals
}

// MatchPath: p (a path in the repo, with /) matches pattern, as in .gitignore: "*" and "?" within a part of the path,
// "**" for any number of parts ("migrations/**", "**/auth/**"); a pattern without "/" matches the file's name at
// any depth ("*.env*"), one ending in "/" everything under that folder.
func MatchPath(pattern, p string) bool {
	pattern = strings.TrimPrefix(strings.TrimSpace(pattern), "/")
	p = strings.TrimPrefix(p, "/")
	if pattern == "" || p == "" {
		return false
	}
	if strings.HasSuffix(pattern, "/") {
		pattern += "**"
	}
	if !strings.Contains(pattern, "/") {
		pattern = "**/" + pattern
	}
	return matchParts(strings.Split(pattern, "/"), strings.Split(p, "/"))
}

func matchParts(pat, parts []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			for len(pat) > 0 && pat[0] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 0 {
				return len(parts) > 0
			}
			for i := range parts {
				if matchParts(pat, parts[i:]) {
					return true
				}
			}
			return false
		}
		if len(parts) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], parts[0]); err != nil || !ok {
			return false
		}
		pat, parts = pat[1:], parts[1:]
	}
	return len(parts) == 0
}
