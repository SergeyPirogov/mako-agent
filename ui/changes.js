// Changed files and diff of a task's worktree compared with its base branch; refreshes while the agent works.
(() => {
  "use strict";
  const task = document.body.dataset.task;
  const root = document.getElementById("changes");
  const STATUS = { A: ["додано", "add"], M: ["змінено", "mod"], D: ["видалено", "del"], R: ["перейменовано", "mod"], C: ["скопійовано", "add"], "?": ["новий, не закомічено", "new"] };
  let last = "";
  let lastData = null;
  const collapsed = new Set(); // files whose diff is folded
  const closedDirs = new Set(); // folders folded in the tree
  let view = "tree";
  try {
    view = localStorage.getItem("mako.changesView") || "tree";
  } catch {
    // storage unavailable: default view
  }

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
    const list = view === "tree" ? renderTree(buildTree(data.files), 0) : data.files.map((f) => fileRow(f, f.path, 0)).join("");
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
      <div class="cview"><button data-view="tree" class="${view === "tree" ? "on" : ""}">Дерево</button><button data-view="list" class="${view === "list" ? "on" : ""}">Список</button></div>
      <div class="cfiles">${list}</div>${diffs}
      ${data.truncated ? '<p class="muted">Diff дуже великий, показано лише початок.</p>' : ""}`;
  }

  function fileRow(f, label, depth) {
    const [title, cls] = STATUS[f.status] || [f.status, "mod"];
    return `<a class="cf" href="#${anchor(f.path)}" style="padding-left:${10 + depth * 16}px" title="${h(f.path)}">
      <span class="fs ${cls}" title="${h(title)}">${h(f.status === "?" ? "N" : f.status)}</span>
      <span class="fp">${h(label)}</span><span class="fn"><span class="plus">+${f.additions}</span> <span class="minus">−${f.deletions}</span></span></a>`;
  }

  /** Folders → files, with single-child folder chains merged ("src/executors") like VS Code's compact folders. */
  function buildTree(files) {
    const root = { name: "", path: "", dirs: new Map(), files: [] };
    for (const f of files) {
      const parts = f.path.split("/");
      let node = root;
      for (const part of parts.slice(0, -1)) {
        if (!node.dirs.has(part)) node.dirs.set(part, { name: part, path: node.path ? `${node.path}/${part}` : part, dirs: new Map(), files: [] });
        node = node.dirs.get(part);
      }
      node.files.push({ ...f, name: parts[parts.length - 1] });
    }
    const compact = (node) => {
      for (const [key, child] of node.dirs) {
        let c = child;
        while (c.files.length === 0 && c.dirs.size === 1) {
          const [only] = c.dirs.values();
          c = { ...only, name: `${c.name}/${only.name}` };
        }
        node.dirs.set(key, c);
        compact(c);
      }
      return node;
    };
    return compact(root);
  }

  function totals(node) {
    let add = 0, del = 0, count = 0;
    for (const f of node.files) (add += f.additions), (del += f.deletions), count++;
    for (const d of node.dirs.values()) {
      const t = totals(d);
      (add += t.add), (del += t.del), (count += t.count);
    }
    return { add, del, count };
  }

  function renderTree(node, depth) {
    const dirs = [...node.dirs.values()].sort((a, b) => a.name.localeCompare(b.name));
    const files = [...node.files].sort((a, b) => a.name.localeCompare(b.name));
    return (
      dirs
        .map((d) => {
          const open = !closedDirs.has(d.path);
          const t = totals(d);
          return `<button class="cdir" data-dir="${h(d.path)}" style="padding-left:${10 + depth * 16}px">
              <span class="caret">${open ? "▾" : "▸"}</span><span class="fp">📁 ${h(d.name)}</span>
              <span class="fn"><span class="muted">${t.count}</span> <span class="plus">+${t.add}</span> <span class="minus">−${t.del}</span></span></button>` +
            (open ? renderTree(d, depth + 1) : "");
        })
        .join("") + files.map((f) => fileRow(f, f.name, depth)).join("")
    );
  }

  async function refresh() {
    try {
      const res = await fetch(`/api/changes?task=${encodeURIComponent(task)}`, { cache: "no-store" });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || res.statusText);
      const key = JSON.stringify(data);
      if (key === last) return;
      last = key;
      lastData = data;
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

  function rerender() {
    if (!lastData) return;
    const y = window.scrollY;
    render(lastData);
    window.scrollTo(0, y);
  }

  root.addEventListener("click", (e) => {
    const dir = e.target.closest("button.cdir");
    if (dir) {
      const p = dir.dataset.dir;
      closedDirs.has(p) ? closedDirs.delete(p) : closedDirs.add(p);
      return rerender();
    }
    const switcher = e.target.closest(".cview button");
    if (switcher) {
      view = switcher.dataset.view;
      try {
        localStorage.setItem("mako.changesView", view);
      } catch {
        // ignore
      }
      return rerender();
    }
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
