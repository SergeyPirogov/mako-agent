package core

import (
	"strings"

	"github.com/SergeyPirogov/mako-agent/go/internal/i18n"
)

// ErrorKind: why a task ended badly, as a key that does not depend on the language Record.Error was written in,
// so the statistics count one cause once. Record.Error keeps the text for the card.
const (
	KindCancelled     = "cancelled"      // stopped from the board
	KindMRClosed      = "mr-closed"      // the MR was closed without a merge
	KindWorkspaceGone = "workspace-gone" // Orca's workspace was deleted, no MR was opened
	KindTrackerClosed = "tracker-closed" // the issue was closed in the tracker
	KindTrackerGone   = "tracker-deleted"
	KindRussian       = "russian"      // the task is written in Russian
	KindCostCeiling   = "cost-ceiling" // the agent spent the task's ceiling
)

// kindLabels: the key of the statistics' line for each kind.
var kindLabels = map[string]string{
	KindCancelled:     "stats.reason.cancelled",
	KindMRClosed:      "stats.reason.mrClosed",
	KindWorkspaceGone: "stats.reason.workspaceGone",
	KindTrackerClosed: "stats.reason.trackerClosed",
	KindTrackerGone:   "stats.reason.trackerDeleted",
	KindRussian:       "stats.reason.russian",
	KindCostCeiling:   "stats.reason.costCeiling",
}

// KindLabel: the statistics' line for a kind; "" for a kind it does not know.
func KindLabel(kind string) string {
	if key, ok := kindLabels[kind]; ok {
		return i18n.T(key)
	}
	return ""
}

// ErrorKindFromText: the kind of an error text that older records carry without one, in any language Mako has
// written it in; "" when it is not a cause Mako knows.
func ErrorKindFromText(text string) string {
	e := strings.TrimSpace(text)
	if e == "" {
		return ""
	}
	low := strings.ToLower(e)
	switch {
	case e == "Cancelled", e == "Скасовано", strings.Contains(low, "cancelled by the developer"): // cyrillic-ok
		return KindCancelled
	case e == "MR closed without merging", e == "MR закрито без мерджу": // cyrillic-ok
		return KindMRClosed
	case strings.HasPrefix(e, "Workspace deleted ("), strings.HasPrefix(e, "Workspace видалено ("): // cyrillic-ok
		return KindWorkspaceGone
	case e == "Task closed in the tracker", e == "Задачу закрито в трекері": // cyrillic-ok
		return KindTrackerClosed
	case e == "Task deleted in the tracker", e == "Задачу видалено в трекері": // cyrillic-ok
		return KindTrackerGone
	case e == i18n.In(i18n.EN, "task.russian"), e == i18n.In(i18n.UK, "task.russian"):
		return KindRussian
	case strings.HasPrefix(e, "the cost ceiling of the task is reached"):
		return KindCostCeiling
	}
	return ""
}

// ErrorKindOf: the kind of the record's error: the one written with it, else the one its text tells.
func ErrorKindOf(r Record) string {
	if r.ErrorKind != "" {
		return r.ErrorKind
	}
	return ErrorKindFromText(r.Error)
}
