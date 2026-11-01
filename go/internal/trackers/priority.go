package trackers

import (
	"regexp"
	"strings"
)

var (
	// «priority: high», «priority::high», «priority/high», «priority-high», «prio: high».
	priorityLabel = regexp.MustCompile(`(?i)^prio(?:rity)?\s*[:/\-]{1,2}\s*(.+)$`)
	// «P0».. «P4», «high-priority», «urgent».
	priorityShort = regexp.MustCompile(`(?i)^p[0-4]$`)
	priorityWord  = regexp.MustCompile(`(?i)^(?:(blocker|critical|urgent|high|medium|low|minor|trivial)(?:[\s\-]priority)?)$`)
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
