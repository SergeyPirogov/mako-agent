import http from "node:http";
import type { Orchestrator } from "./orchestrator.js";
import type { TaskRecord, TaskStatus } from "./state.js";
import { log } from "./log.js";

const LABELS: Record<TaskStatus, string> = {
  asking: "Нова",
  snoozed: "Відкладена",
  queued: "У черзі",
  running: "Виконується",
  "handed-off": "Передано",
  done: "Готово",
  failed: "Помилка",
};

/** Local page with the same Take / Postpone buttons, for notifications that were missed. */
export function startInbox(orchestrator: Orchestrator, port: number): void {
  const server = http.createServer((req, res) => {
    const url = new URL(req.url ?? "/", `http://127.0.0.1:${port}`);
    if (req.method === "POST") {
      const id = url.searchParams.get("id") ?? "";
      const action = url.pathname.slice(1);
      if (action === "take") orchestrator.decide(id, "take");
      if (action === "later") orchestrator.decide(id, "later");
      if (action === "retry") orchestrator.retry(id);
      res.writeHead(303, { Location: "/" }).end();
      return;
    }
    if (url.pathname === "/api/tasks") {
      res.writeHead(200, { "Content-Type": "application/json" }).end(JSON.stringify(orchestrator.state.all()));
      return;
    }
    res.writeHead(200, { "Content-Type": "text/html; charset=utf-8" }).end(render(orchestrator.state.all()));
  });
  server.listen(port, "127.0.0.1", () => log("inbox", `http://127.0.0.1:${port}`));
  server.on("error", (err) => log("inbox", `cannot start: ${err.message}`));
}

const order: TaskStatus[] = ["asking", "snoozed", "queued", "running", "failed", "handed-off", "done"];

function render(records: TaskRecord[]): string {
  const rows = records
    .sort((a, b) => order.indexOf(a.status) - order.indexOf(b.status) || b.updatedAt.localeCompare(a.updatedAt))
    .map((r) => {
      const form = (action: string, label: string) =>
        `<form method="post" action="/${action}?id=${encodeURIComponent(r.id)}"><button>${label}</button></form>`;
      const actions =
        r.status === "asking" || r.status === "snoozed"
          ? form("take", "Взяти") + form("later", "Відкласти")
          : r.status === "failed"
            ? form("retry", "Повторити")
            : "";
      const extra = r.prUrl
        ? `<a href="${esc(r.prUrl)}">PR</a>`
        : r.error
          ? `<span class="err">${esc(r.error.slice(0, 160))}</span>`
          : r.status === "snoozed" && r.snoozedUntil
            ? `до ${new Date(r.snoozedUntil).toLocaleTimeString()}`
            : "";
      return `<tr><td><span class="s ${r.status}">${LABELS[r.status]}</span></td>
        <td><a href="${esc(r.task.url)}">${esc(r.id)}</a>${r.repo ? ` → ${esc(r.repo)}` : ""}${r.executor ? ` · ${esc(r.executor)}` : ""}<div>${esc(r.task.title)}</div></td>
        <td>${extra}</td><td class="act">${actions}</td></tr>`;
    })
    .join("");
  return `<!doctype html><html lang="uk"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width">
<meta http-equiv="refresh" content="15"><title>Mako · Вхідні</title><style>
:root{color-scheme:light dark;--bg:#fff;--fg:#1c1c1e;--mute:#6b6b70;--line:#e3e3e6}
@media (prefers-color-scheme:dark){:root{--bg:#161618;--fg:#eee;--mute:#9a9aa0;--line:#2c2c30}}
body{margin:0;padding:24px 16px;font:15px/1.4 system-ui,sans-serif;background:var(--bg);color:var(--fg)}
main{max-width:900px;margin:auto}h1{font-size:20px}table{width:100%;border-collapse:collapse}
td{padding:10px 8px;border-top:1px solid var(--line);vertical-align:top}td div{color:var(--mute)}
a{color:inherit}.act{white-space:nowrap}form{display:inline}button{margin-left:6px;padding:5px 12px;border-radius:6px;border:1px solid var(--line);background:none;color:inherit;cursor:pointer}
.s{font-size:12px;padding:2px 8px;border-radius:10px;background:var(--line)}.asking{background:#2f6fed;color:#fff}.handed-off{background:#7a5af8;color:#fff}.running{background:#d98a00;color:#fff}.done{background:#2e8b57;color:#fff}.failed,.err{color:#d33}.failed{background:none;border:1px solid #d33}
</style></head><body><main><h1>Задачі для агента</h1>
${rows ? `<table>${rows}</table>` : "<p>Поки немає задач.</p>"}</main></body></html>`;
}

function esc(s: string): string {
  return s.replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" })[c]!);
}
