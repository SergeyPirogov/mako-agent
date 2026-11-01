# Stats page redesign — mockups (#280)

Static HTML mockups of the Stats page. Open any file in a browser; the tabs link to each other. Desktop screens are 1040 px wide, the phone screen 390 px. The numbers are real stats of one Mako for 7 days.

| File | Tab |
|---|---|
| [1-overview.html](1-overview.html) | Key metrics, tasks per day, Needs attention, where the time goes, breakdown |
| [2-time.html](2-time.html) | Lead time, stages explained, by task size, rework |
| [3-cost.html](3-cost.html) | Spent, by phase, per day, by model |
| [4-quality.html](4-quality.html) | Agent review funnel, size estimate matrix, agent's assessment, failure reasons |
| [5-context.html](5-context.html) | Start and peak by phase, cache, what fills the context, heaviest files and commands |
| [6-phone-overview.html](6-phone-overview.html) | Phone, Overview tab |

Common to every screen: the header with the period segments and the project filter, the tab row, cards on `--card` with `--line` borders, colors from `go/internal/web/style.css` (light theme). Text is English only (see #274–#276).
