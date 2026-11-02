# Task sidebar — prototype

Not a drawing: the real board with `proto.css` and `proto.js` on top. `proxy.py` serves Mako's board on
`127.0.0.1:7790` with the two files injected and passes everything else to Mako on `:7777`; POSTs are dropped,
so the prototype never changes anything.

```
python3 docs/mockups/316/proxy.py
open "http://127.0.0.1:7790/?mode=after"     # the prototype
open "http://127.0.0.1:7790/?mode=before"    # the board as it is
```

`?sel=<task id>` opens a task (`before.png` and `after.png` show `SergeyPirogov/mako-agent#305`).

What it changes:

1. **Top: one row instead of three.** The filter and a "+" button. Under them, only when there is more than one project or an archive, a full-width menu "All tasks 36" (a project, the archive) styled like the filter field, no "·" in the labels.
2. **Footer:** "Checked 1 min ago", check now, pause and "?" (why is a task missing) move to the bottom.
3. **Groups by what moves:** At work, Blocked, In review, Reviews for me, New, Not done, Postponed, Done. Headings in sentence case, the count on the right.
4. **Rows:** the title in two lines at most, no tracker icon when there is one tracker, the time on the right.
5. **Group by project.** A folder button next to the filter switches the list between "by status" (the default) and "by project"; the choice is remembered.
   - By project: a group per project (a coloured square, the short name, the path, the count), its tasks in work order, then Postponed and Done, each a folded line of its own with its count.
   - A project folds with a click on its heading (remembered); folded, it shows a one-line summary instead of its tasks ("1 new", "1 at work · 2 new", "nothing in work").
   - By status with several projects: inside a status group, a thin label per project over its tasks.

`?group=project` opens the prototype grouped by project. The live board has tasks in one project only, so
`?demo=projects` makes the proxy move a few tasks to two other real projects (market-autotests, ecom/tests/moq)
in what it hands the page — `group-status.png` and `group-project.png` show that demo split.

`?fold=market-autotests,moq` folds those projects before a shot (`group-folded.png`); `?scrolltest=1` runs the scroll check (the result lands in `<html data-st>`).
