// Editor-like changes view: file tree on the left, the selected file's diff or full content on the right.
// In the session page it is split in two: ?layout=side is the tree beside the terminal, a click opens the file
// in a tab over the terminal, and that tab is ?layout=file&file=<path>, the preview alone.
(() => {
  "use strict";
  const task = document.body.dataset.task;
  const params = new URLSearchParams(location.search);
  const layout = params.get("layout") || "full"; // "full" | "side" | "file"
  const root = document.getElementById("changes");
  const STATUS = { A: ["додано", "add"], M: ["змінено", "mod"], D: ["видалено", "del"], R: ["перейменовано", "mod"], C: ["скопійовано", "add"], "?": ["новий, не закомічено", "new"] };

  let data = null; // last /api/changes answer
  let lastKey = "";
  let selected = layout === "file" ? params.get("file") : null; // path of the file in the preview (in the side tree: the open tab)
  let mode = "diff"; // "diff" | "file"
  let view = "tree"; // "tree" | "list"
  const closedDirs = new Set();
  const fileCache = new Map(); // path -> /api/file answer, dropped when the diff changes
  try {
    view = localStorage.getItem("mako.changesView") || "tree";
    mode = localStorage.getItem("mako.changesMode") || "diff";
  } catch {
    // storage unavailable: defaults
  }
  const remember = (k, v) => {
    try {
      localStorage.setItem(k, v);
    } catch {
      // ignore
    }
  };

  const h = (s) => String(s ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);

  // ---------- diff parsing ----------

  function splitDiff(diff) {
    const out = new Map();
    for (const chunk of diff.split(/^(?=diff --git )/m)) {
      const m = chunk.match(/^diff --git a\/(.*?) b\/(.*)$/m);
      if (m) out.set(m[2], chunk);
    }
    return out;
  }

  /** Unified diff → rows with old/new line numbers, like an editor's diff view. */
  function diffRows(chunk) {
    const rows = [];
    let oldNo = 0;
    let newNo = 0;
    let started = false;
    for (const line of chunk.split("\n")) {
      const hunk = line.match(/^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@(.*)$/);
      if (hunk) {
        started = true;
        oldNo = Number(hunk[1]);
        newNo = Number(hunk[2]);
        rows.push({ kind: "hunk", text: line });
        continue;
      }
      if (!started) {
        if (line.startsWith("Binary files")) rows.push({ kind: "meta", text: "Двійковий файл: вміст не показується" });
        continue;
      }
      if (line.startsWith("+")) rows.push({ kind: "add", n: newNo++, text: line.slice(1) });
      else if (line.startsWith("-")) rows.push({ kind: "del", o: oldNo++, text: line.slice(1) });
      else if (line.startsWith("\\")) rows.push({ kind: "meta", text: line });
      else if (line !== "" || rows.length) rows.push({ kind: "ctx", o: oldNo++, n: newNo++, text: line.slice(1) });
    }
    while (rows.length && rows[rows.length - 1].kind === "ctx" && rows[rows.length - 1].text === "") rows.pop();
    return rows;
  }

  /** New-side line numbers the task added, to highlight them in the full-file view. */
  function addedLines(chunk) {
    return new Set(diffRows(chunk).filter((r) => r.kind === "add").map((r) => r.n));
  }

  // ---------- tree ----------

  function buildTree(files) {
    const top = { name: "", path: "", dirs: new Map(), files: [] };
    for (const f of files) {
      const parts = f.path.split("/");
      let node = top;
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
    return compact(top);
  }

  function totals(node) {
    let add = 0, del = 0;
    for (const f of node.files) (add += f.additions), (del += f.deletions);
    for (const d of node.dirs.values()) {
      const t = totals(d);
      (add += t.add), (del += t.del);
    }
    return { add, del };
  }

  /** Files in the order they appear on screen, for keyboard navigation. */
  function visibleOrder() {
    if (view === "list") return data.files.map((f) => f.path);
    const out = [];
    const walk = (node) => {
      for (const d of [...node.dirs.values()].sort((a, b) => a.name.localeCompare(b.name))) if (!closedDirs.has(d.path)) walk(d);
      for (const f of [...node.files].sort((a, b) => a.name.localeCompare(b.name))) out.push(f.path);
    };
    walk(buildTree(data.files));
    return out;
  }

  function fileRow(f, label, depth) {
    const [title, cls] = STATUS[f.status] || [f.status, "mod"];
    return `<button class="cx-file${f.path === selected ? " on" : ""}" data-file="${h(f.path)}" style="padding-left:${8 + depth * 14}px" title="${h(f.path)}">
      <span class="fs ${cls}" title="${h(title)}">${h(f.status === "?" ? "N" : f.status)}</span>
      <span class="fp">${h(label)}</span><span class="fn"><span class="plus">+${f.additions}</span> <span class="minus">−${f.deletions}</span></span></button>`;
  }

  function renderTree(node, depth) {
    const dirs = [...node.dirs.values()].sort((a, b) => a.name.localeCompare(b.name));
    const files = [...node.files].sort((a, b) => a.name.localeCompare(b.name));
    return (
      dirs
        .map((d) => {
          const open = !closedDirs.has(d.path);
          const t = totals(d);
          return `<button class="cx-dir" data-dir="${h(d.path)}" style="padding-left:${8 + depth * 14}px">
              <span class="caret">${open ? "▾" : "▸"}</span><span class="fp">${h(d.name)}</span>
              <span class="fn"><span class="plus">+${t.add}</span> <span class="minus">−${t.del}</span></span></button>` + (open ? renderTree(d, depth + 1) : "");
        })
        .join("") + files.map((f) => fileRow(f, f.name, depth)).join("")
    );
  }

  // ---------- rendering ----------

  function render() {
    if (layout === "file") return renderFileTab();
    if (!data.files.length) {
      root.innerHTML = `<div class="cx-empty muted">Змін відносно <code>${h(data.base)}</code> поки немає. Сторінка оновиться сама.</div>`;
      return;
    }
    // Start on the first file as it appears on screen (top of the tree), not git's order.
    if (layout === "full" && (!selected || !data.files.some((f) => f.path === selected))) selected = visibleOrder()[0] ?? data.files[0].path;
    const add = data.files.reduce((n, f) => n + f.additions, 0);
    const del = data.files.reduce((n, f) => n + f.deletions, 0);
    const commits = data.commits.length
      ? `<details class="cx-commits"><summary>${data.commits.length} ${data.commits.length === 1 ? "коміт" : "коміти"}</summary>
          ${data.commits.map((c) => `<div><code>${h(c.hash)}</code> ${h(c.subject)}</div>`).join("")}</details>`
      : `<div class="muted small">комітів ще немає</div>`;
    const list = view === "tree" ? renderTree(buildTree(data.files), 0) : data.files.map((f) => fileRow(f, f.path, 0)).join("");
    const scrollTop = root.querySelector(".cx-files")?.scrollTop ?? 0;
    const bodyScroll = root.querySelector(".cx-body")?.scrollTop ?? 0;
    root.innerHTML = `<div class="cx${layout === "side" ? " side" : ""}">
      <aside class="cx-side">
        <div class="cx-sum"><strong>${data.files.length} ${data.files.length === 1 ? "файл" : "файлів"}</strong>
          <span class="plus">+${add}</span> <span class="minus">−${del}</span>
          <span class="muted small">від <code>${h(data.base)}</code></span></div>
        ${commits}
        <div class="seg"><button data-view="tree" class="${view === "tree" ? "on" : ""}">Дерево</button><button data-view="list" class="${view === "list" ? "on" : ""}">Список</button></div>
        <div class="cx-files" tabindex="0" aria-label="Змінені файли">${list}</div>
      </aside>
      ${layout === "side" ? "" : `<section class="cx-main"><div class="cx-head"></div><div class="cx-body"></div></section>`}
    </div>`;
    root.querySelector(".cx-files").scrollTop = scrollTop;
    renderPreview(bodyScroll);
  }

  /** A tab over the terminal: this one file's preview, no tree. */
  function renderFileTab() {
    const bodyScroll = root.querySelector(".cx-body")?.scrollTop ?? 0;
    if (!data.files.some((f) => f.path === selected)) {
      root.innerHTML = `<div class="cx-empty muted"><code>${h(selected)}</code>: змін відносно <code>${h(data.base)}</code> у цьому файлі більше немає.</div>`;
      return;
    }
    if (!root.querySelector(".cx-main")) root.innerHTML = `<div class="cx file"><section class="cx-main"><div class="cx-head"></div><div class="cx-body"></div></section></div>`;
    renderPreview(bodyScroll);
  }

  /** `keepScroll`: where the preview was scrolled before a refresh (0 for a newly selected file). */
  async function renderPreview(keepScroll = 0) {
    const head = root.querySelector(".cx-head");
    const body = root.querySelector(".cx-body");
    if (!head || !body) return;
    const f = data.files.find((x) => x.path === selected);
    const [title, cls] = STATUS[f.status] || [f.status, "mod"];
    head.innerHTML = `<span class="fs ${cls}" title="${h(title)}">${h(f.status === "?" ? "N" : f.status)}</span>
      <span class="cx-path">${h(f.path)}</span><span class="fn"><span class="plus">+${f.additions}</span> <span class="minus">−${f.deletions}</span></span>
      <div class="seg"><button data-mode="diff" class="${mode === "diff" ? "on" : ""}">Зміни</button><button data-mode="file" class="${mode === "file" ? "on" : ""}"${f.status === "D" ? " disabled" : ""}>Файл</button></div>`;
    const chunk = splitDiff(data.diff).get(f.path) ?? "";
    const y = keepScroll;

    if (mode === "diff" || f.status === "D") {
      const rows = diffRows(chunk);
      body.innerHTML = rows.length
        ? `<table class="code">${rows
            .map((r) =>
              r.kind === "hunk"
                ? `<tr class="hunk"><td class="ln"></td><td class="ln"></td><td class="tx">${h(r.text)}</td></tr>`
                : r.kind === "meta"
                  ? `<tr class="meta"><td class="ln"></td><td class="ln"></td><td class="tx">${h(r.text)}</td></tr>`
                  : `<tr class="${r.kind}"><td class="ln">${r.o ?? ""}</td><td class="ln">${r.n ?? ""}</td><td class="tx"><span class="sign">${r.kind === "add" ? "+" : r.kind === "del" ? "−" : " "}</span>${h(r.text) || " "}</td></tr>`,
            )
            .join("")}</table>`
        : `<div class="cx-empty muted">Вміст не змінився (перейменування чи права доступу).</div>`;
      body.scrollTop = y;
      return;
    }

    // Full file as it is now in the worktree, with the task's added lines highlighted.
    let file = fileCache.get(f.path);
    if (!file) {
      body.innerHTML = `<div class="cx-empty muted">Завантаження…</div>`;
      try {
        const res = await fetch(`/api/file?task=${encodeURIComponent(task)}&path=${encodeURIComponent(f.path)}`, { cache: "no-store" });
        file = await res.json();
        if (!res.ok) throw new Error(file.error || res.statusText);
        fileCache.set(f.path, file);
      } catch (err) {
        body.innerHTML = `<div class="cx-empty err">${h(err.message)}</div>`;
        return;
      }
      if (selected !== f.path || mode !== "file") return; // the user moved on while it loaded
    }
    if (file.binary) body.innerHTML = `<div class="cx-empty muted">Двійковий файл</div>`;
    else if (file.tooLarge) body.innerHTML = `<div class="cx-empty muted">Файл завеликий для перегляду</div>`;
    else if (file.missing) body.innerHTML = `<div class="cx-empty muted">Файлу вже немає в worktree</div>`;
    else {
      const added = addedLines(chunk);
      const lines = file.content.replace(/\n$/, "").split("\n");
      body.innerHTML = `<table class="code">${lines
        .map((l, i) => `<tr class="${added.has(i + 1) ? "add" : ""}"><td class="ln">${i + 1}</td><td class="tx">${h(l) || " "}</td></tr>`)
        .join("")}</table>`;
      // Jump to the first change the first time this file is shown.
      const first = body.querySelector("tr.add");
      if (first && !y) first.scrollIntoView({ block: "center" });
      else body.scrollTop = y;
    }
  }

  function select(path) {
    if (!path || path === selected) return;
    selected = path;
    root.querySelectorAll(".cx-file").forEach((b) => b.classList.toggle("on", b.dataset.file === path));
    root.querySelector(`.cx-file[data-file="${CSS.escape(path)}"]`)?.scrollIntoView({ block: "nearest" });
    if (layout === "full") renderPreview(0);
  }

  /** Side tree: the session page opens the file in a tab over the terminal. */
  function openTab(path) {
    select(path);
    parent.postMessage({ type: "mako-open-file", path }, location.origin);
  }

  // ---------- data ----------

  async function refresh() {
    try {
      const res = await fetch(`/api/changes?task=${encodeURIComponent(task)}`, { cache: "no-store" });
      const next = await res.json();
      if (!res.ok) throw new Error(next.error || res.statusText);
      const key = JSON.stringify(next);
      if (key === lastKey) return;
      if (data && next.diff !== data.diff) fileCache.clear();
      lastKey = key;
      data = next;
      render();
    } catch (err) {
      root.innerHTML = `<div class="cx-empty muted">${h(err.message)}</div>`;
      lastKey = "";
    }
  }

  // ---------- events ----------

  root.addEventListener("click", (e) => {
    const file = e.target.closest(".cx-file");
    if (file) return layout === "side" ? openTab(file.dataset.file) : select(file.dataset.file);
    const dir = e.target.closest(".cx-dir");
    if (dir) {
      closedDirs.has(dir.dataset.dir) ? closedDirs.delete(dir.dataset.dir) : closedDirs.add(dir.dataset.dir);
      return render();
    }
    const v = e.target.closest("[data-view]");
    if (v) {
      view = v.dataset.view;
      remember("mako.changesView", view);
      return render();
    }
    const m = e.target.closest("[data-mode]");
    if (m && !m.disabled) {
      mode = m.dataset.mode;
      remember("mako.changesMode", mode);
      return renderPreview(0);
    }
  });

  // ↑/↓ (or j/k) move through the files, like an editor's file list; in the side tree Enter opens the file's tab.
  document.addEventListener("keydown", (e) => {
    if (layout === "file" || !data?.files.length || e.target.closest?.("input, textarea, select")) return;
    if (layout === "side" && e.key === "Enter" && selected) return void (e.preventDefault(), openTab(selected));
    const step = e.key === "ArrowDown" || e.key === "j" ? 1 : e.key === "ArrowUp" || e.key === "k" ? -1 : 0;
    if (!step) return;
    e.preventDefault();
    const order = visibleOrder();
    const i = order.indexOf(selected);
    select(order[Math.max(0, Math.min(order.length - 1, i + step))]);
  });

  // From the session page: which tab is in front (the tree marks its file), or that this tab came to the front.
  window.addEventListener("message", (e) => {
    if (e.origin !== location.origin || e.source !== parent) return;
    if (e.data?.type === "mako-active-file" && layout === "side") {
      selected = e.data.path;
      root.querySelectorAll(".cx-file").forEach((b) => b.classList.toggle("on", b.dataset.file === selected));
    } else if (e.data?.type === "mako-show") refresh();
  });

  // Tabs behind the terminal (their frame hidden) wait until they are shown.
  const behind = () => document.hidden || Boolean(window.frameElement?.hidden);
  refresh();
  setInterval(() => !behind() && refresh(), 4000);
})();
