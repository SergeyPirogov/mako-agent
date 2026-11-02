# Agent chat and terminal redesign — mockups (#290, #291, #292, #294)

Static HTML mockups of the agent chat. Desktop screens are 1040 px wide, the phone screen 390 px. The conversation is sample data modelled on task #280.

| File | State | Issue |
|---|---|---|
| [1-working.html](1-working.html) | Turns, meaningful steps, pinned progress, status line with Stop, composer with context meter | #290 |
| [2-steps-opened.html](2-steps-opened.html) | A diff opened in the stream, a failed test opened by itself, «↓ 4 new» | #290 |
| [3-question.html](3-question.html) | The agent's question docked in place of the composer | #291 |
| [4-plan.html](4-plan.html) | The plan as a document, a note on a step, approve with model and mode | #291 |
| [5-turn-finished.html](5-turn-finished.html) | Turn summary, next actions by state, context warning | #292 |
| [6-phone.html](6-phone.html) | Phone, a question waiting | #290, #291 |
| [7-terminal-working.html](7-terminal-working.html) | Terminal: status bar, key bar, the switch to the chat waits for the turn | #294, #289 |
| [8-terminal-asks.html](8-terminal-asks.html) | Terminal: a permission prompt docked under the terminal | #294 |
| [9-phone-terminal.html](9-phone-terminal.html) | Phone terminal: big keys and a text input | #294 |

They replace the chat part of screens 7–8 in `docs/mockups/272/`. Colors come from `go/internal/web/style.css`; text is English only.
