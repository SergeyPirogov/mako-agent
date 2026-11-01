package dispatch

import (
	"context"
	"fmt"
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/trackers"
)

// BugRepo is where bugs found in Mako itself are reported.
const BugRepo = "SergeyPirogov/mako-agent"

// ReportBug opens a GitHub issue about Mako in BugRepo, through the first GitHub tracker on github.com, and
// answers the issue's link. The issue is not put on the board and not assigned to anyone.
func (d *Dispatcher) ReportBug(ctx context.Context, title, description, version string) (string, error) {
	title, description = strings.TrimSpace(title), strings.TrimSpace(description)
	if title == "" {
		return "", fmt.Errorf("enter a title")
	}
	if description == "" {
		return "", fmt.Errorf("describe the bug: what you did and what happened")
	}
	for _, t := range d.TrackerList() {
		g, ok := t.(*trackers.GitHub)
		if !ok || g.Host() != "github.com" {
			continue
		}
		if version != "" {
			description += "\n\n---\nReported from Mako " + version
		}
		return g.ReportBug(ctx, BugRepo, title, description)
	}
	return "", fmt.Errorf("a GitHub tracker on github.com is needed to report a bug: add one in the settings")
}
