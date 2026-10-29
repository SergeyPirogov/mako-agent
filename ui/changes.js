// Changed files and diff of a task's worktree compared with its base branch; refreshes while the agent works.
(() => {
  "use strict";
  const task = document.body.dataset.task;
  const root = document.getElementById("changes");
  const STATUS = { A: ["додано", "add"], M: ["змінено", "mod"], D: ["видалено", "del"], R: ["перейменовано", "mod"], C: ["скопійовано", "add"], "?": ["новий, не закомічено", "new"] };
  let last = "";
  const collapsed = new Set();

  const h = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
  const anchor = (p) => "f-" + p.replace(/[^\w-]/g, "_");

  /** Splits a unified diff into per-file chunks keyed by the new path. */
  function splitDiff(diff) {
    const out = new Map();
    for (const chunk of diff.split(/^(?=diff --git )/m)) {
      const m = chunk.match(/^diff --git a\/(.*?) b\/(.*)$/m);
      if (m) out.set(m[2], chunk);
    }
    return out;
  }

  function renderDiff(chunk) {
    const lines = chunk.split("\n");
    const start = lines.findIndex((l) => l.startsWith("@@") || l.startsWith("Binary files"));
    if (start < 0) return `<div class="dl meta">Без змін у вмісті (перейменування чи права доступу)</div>`;
    return lines
      .slice(start)
      .map((l) => {
        const cls = l.startsWith("@@") ? "hunk" : l.startsWith("+") ? "add" : l.startsWith("-") ? "del" : l.startsWith("\\") || l.startsWith("Binary") ? "meta" : "";
        return `<div class="dl ${cls}">${h(l) || " "}</div>`;
      })
      .join("");
  }

  function render(data) {
    if (!data.files.length) {
      root.innerHTML = `<p class="muted">Змін відносно <code>${h(data.base)}</code> поки немає.</p>`;
      return;
    }
    const add = data.files.reduce((n, f) => n + f.additions, 0);
    const del = data.files.reduce((n, f) => n + f.deletions, 0);
    const chunks = splitDiff(data.diff);
    const commits = data.commits.length
      ? `<details class="commits"><summary>${data.commits.length} ${data.commits.length === 1 ? "коміт" : "коміти"}</summary>
          ${data.commits.map((c) => `<div><code>${h(c.hash)}</code> ${h(c.subject)}</div>`).join("")}</details>`
      : `<span class="muted">комітів ще немає</span>`;
    const list = data.files
      .map((f) => {
        const [label, cls] = STATUS[f.status] || [f.status, "mod"];
        return `<a class="cf" href="#${anchor(f.path)}"><span class="fs ${cls}" title="${h(label)}">${h(f.status === "?" ? "N" : f.status)}</span>
          <span class="fp">${h(f.path)}</span><span class="fn"><span class="plus">+${f.additions}</span> <span class="minus">−${f.deletions}</span></span></a>`;
      })
      .join("");
    const diffs = data.files
      .map((f) => {
        const chunk = chunks.get(f.path);
        const open = !collapsed.has(f.path);
        return `<section class="fd" id="${anchor(f.path)}"><button class="fh" data-file="${h(f.path)}">${open ? "▾" : "▸"} ${h(f.path)}</button>
          ${open ? `<div class="diff">${chunk ? renderDiff(chunk) : '<div class="dl meta">diff недоступний</div>'}</div>` : ""}</section>`;
      })
      .join("");
    root.innerHTML = `<div class="csum"><strong>${data.files.length} ${data.files.length === 1 ? "файл" : "файлів"}</strong>
        <span class="plus">+${add}</span> <span class="minus">−${del}</span> <span class="muted">відносно <code>${h(data.base)}</code></span> · ${commits}</div>
      <div class="cfiles">${list}</div>${diffs}
      ${data.truncated ? '<p class="muted">Diff дуже великий, показано лише початок.</p>' : ""}`;
  }

  async function refresh() {
    try {
      const res = await fetch(`/api/changes?task=${encodeURIComponent(task)}`, { cache: "no-store" });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || res.statusText);
      const key = JSON.stringify(data);
      if (key === last) return;
      last = key;
      const y = window.scrollY;
      const openDetails = root.querySelector("details.commits")?.open;
      render(data);
      if (openDetails) root.querySelector("details.commits")?.setAttribute("open", "");
      window.scrollTo(0, y);
    } catch (err) {
      root.innerHTML = `<p class="muted">${h(err.message)}</p>`;
      last = "";
    }
  }

  root.addEventListener("click", (e) => {
    const btn = e.target.closest("button.fh");
    if (!btn) return;
    const f = btn.dataset.file;
    collapsed.has(f) ? collapsed.delete(f) : collapsed.add(f);
    last = "";
    refresh();
  });

  refresh();
  setInterval(() => !document.hidden && refresh(), 4000);
})();
