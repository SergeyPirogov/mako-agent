package trackers

import (
	"regexp"
	"strings"
)

var (
	// «priority: high», «priority::high», «priority/high», «priority-high», «prio: high».
	priorityLabel = regexp.MustCompile(`(?i)^prio(?:rity)?\s*[:/\-]{1,2}\s*(.+)$`)
	// A bare label counts as a priority only when it is «P0».. «P4» or one of these words (also «high-priority»).
	// Words that mean something else on many repos («minor», «trivial» as a semver or size kind) are left out.
	priorityShort = regexp.MustCompile(`(?i)^p[0-4]$`)
	priorityWord  = regexp.MustCompile(`(?i)^(?:(blocker|critical|urgent|high|medium|low)(?:[\s\-]priority)?)$`)
)

// priorityFromLabels reads a priority from the labels, for trackers (GitHub, GitLab) that have no priority
// field of their own; "" when none of the labels is one.
func priorityFromLabels(labels []string) string {
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if m := priorityLabel.FindStringSubmatch(l); m != nil {
			return strings.TrimSpace(m[1])
		}
	}
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if priorityShort.MatchString(l) {
			return strings.ToUpper(l)
		}
		if m := priorityWord.FindStringSubmatch(l); m != nil {
			return l
		}
	}
	return ""
}
