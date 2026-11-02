// The markup changes the CSS cannot do: "+" next to the filter, the search status and "?" in a footer.
(() => {
  const side = document.querySelector(".side"), bar = document.getElementById("search")?.parentElement;
  const filter = document.getElementById("filter"), plus = document.getElementById("new-task"), why = document.getElementById("why-btn");
  const top = document.createElement("div"); top.className = "p-top";
  const field = document.createElement("label"); field.className = "p-find";
  field.innerHTML = '<svg width="13" height="13" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.2" stroke-linecap="round"><circle cx="11" cy="11" r="7"/><path d="M20 20l-3.5-3.5"/></svg>';
  const q = document.getElementById("q"); q.removeAttribute("data-t-ph"); q.placeholder = "Filter tasks"; field.append(q);
  plus.removeAttribute("data-t"); plus.textContent = "+"; plus.title = "New task"; plus.className = "p-plus";
  top.append(field, plus); side.prepend(top);
  why.removeAttribute("data-t"); why.removeAttribute("data-t-title"); why.textContent = "?"; why.title = "Why is a task missing?"; why.className = "icon-btn p-why"; bar.append(why);
  // the filter row keeps only the project/archive select and the "Needs attention" chip
  const keep = () => { q.hidden = false; filter.hidden = document.getElementById("scope").hidden && document.getElementById("need-chip").hidden; };
  new MutationObserver(keep).observe(document.getElementById("list"), { childList: true }); keep();
})();

// Group by project: a toggle next to the filter; the list then has a group per project, its active tasks by
// status order and its Postponed and Done each behind its own folded line.
(() => {
  const KEY = "p-group-by";
  let byProject = (() => { try { return localStorage.getItem(KEY) === "project"; } catch { return false; } })();
  if (new URLSearchParams(location.search).get("group") === "project") byProject = true;
  const open = new Set();
  // Folded projects, remembered like the folded status groups.
  const FOLD = "p-folded-projects";
  const folded = new Set((() => { try { return JSON.parse(localStorage.getItem(FOLD) || "[]"); } catch { return []; } })());
  const saveFold = () => { try { localStorage.setItem(FOLD, JSON.stringify([...folded])); } catch {} };
  const tg = document.createElement("button");
  tg.className = "icon-btn p-grp"; tg.title = "Group by project";
  tg.innerHTML = '<svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>';
  const plus = document.querySelector(".p-plus"); plus.parentElement.insertBefore(tg, plus);
  const paint = () => { tg.classList.toggle("on", byProject); tg.setAttribute("aria-pressed", byProject); };
  tg.onclick = () => { byProject = !byProject; try { localStorage.setItem(KEY, byProject ? "project" : "status"); } catch {} paint(); render(); };
  paint();

  const ORDER = ["doing", "blocked", "review", "forme", "new", "failed", "later", "done"];
  const QUIET = new Set(["later", "done"]);
  const HUES = ["#2f6fde", "#2f9e5b", "#c27c0e", "#7b61d9", "#d14343", "#0f8b8d"];
  const projs = () => [...new Set(visible().map((r) => r.task.project))].sort();
  const hue = (p) => HUES[Math.max(0, projs().indexOf(p)) % HUES.length];
  const row = (r) => {
    const st = state(r);
    const [say, tone, more] = cardStatus(r), why = r.error && r.status !== "cancelled" ? r.error : more ?? say;
    return `<button class="item ${r.id === selected ? "sel" : ""} ${st === "later" ? "later" : ""}" data-id="${esc(r.id)}" title="${esc(r.id)} · ${esc(r.task.title)}">
      <span class="t">${esc(r.task.title)}</span>
      <span class="m"><span class="s ${r.error && r.status !== "cancelled" ? "failed" : st}"></span>${prioBadge(r.task)}${r.type === "fix" ? kind(r) : ""}<span class="say ${tone}" title="${esc(why)}">${esc(say)}</span>${r.column === "new" && estimated(r) ? `<span class="size ${esc(r.estimate.size)}">${SIZE_NAMES[r.estimate.size] ?? ""}</span>` : ""}${reviewBadge(r)}${checksBadge(r)}<span class="ago">${agoShort(r.updatedAt)}</span></span></button>`;
  };
  let busy = false;
  // By status with several projects: inside a status group, its tasks under a thin label per project.
  const tag = () => {
    if (projs().length < 2) return;
    busy = true;
    for (const box of $("list").querySelectorAll(".group-box")) {
      const items = [...box.querySelectorAll(":scope > .item[data-id]")];
      const of = (b) => data.records.find((x) => x.id === b.dataset.id)?.task.project || "";
      const ps = [...new Set(items.map(of))];
      if (ps.length < 2) continue;
      ps.sort((x, y) => projs().indexOf(x) - projs().indexOf(y));
      for (const p of ps) {
        const lab = document.createElement("div");
        lab.className = "p-sub"; lab.title = p;
        lab.innerHTML = `<i style="background:${hue(p)}"></i>${esc(p.split("/").pop())}`;
        box.append(lab);
        for (const b of items.filter((b) => of(b) === p)) box.append(b);
      }
    }
    queueMicrotask(() => { busy = false; });
  };
  const regroup = () => {
    if (!byProject && data && !busy) { tag(); return; }
    if (busy || !data) return;
    busy = true;
    const words = ($("q").value || "").trim().toLowerCase().split(/\s+/).filter(Boolean);
    const rows = visible().filter((r) => words.every((w) => `${r.task.title} ${r.id} ${r.task.project}`.toLowerCase().includes(w)));
    const by = new Map();
    for (const r of rows) { const p = r.task.project; if (!by.has(p)) by.set(p, []); by.get(p).push(r); }
    const rank = (r) => ORDER.indexOf(groupOf(r));
    const projects = [...by.keys()].sort((a, b) => {
      const act = (p) => by.get(p).filter((r) => !QUIET.has(groupOf(r))).length;
      return (act(b) > 0) - (act(a) > 0) || a.split("/").pop().localeCompare(b.split("/").pop());
    });
    let html = "";
    for (const p of projects) {
      const all = by.get(p).sort((a, b) => rank(a) - rank(b));
      const active = all.filter((r) => !QUIET.has(groupOf(r))), quiet = all.filter((r) => QUIET.has(groupOf(r)));
            const isFolded = folded.has(p) && !words.length;
      const counts = {};
      for (const r of all) { const g = groupOf(r); counts[g] = (counts[g] || 0) + 1; }
      const WORD = { doing: "at work", blocked: "blocked", review: "in review", forme: "to review", new: "new", failed: "not done" };
      const summary = ORDER.filter((g) => WORD[g] && counts[g]).map((g) => `${counts[g]} ${WORD[g]}`).join(" · ") || "nothing in work";
      html += `<div class="group-box p-proj${isFolded ? " folded" : ""}"><button class="p-ph" data-proj="${esc(p)}" aria-expanded="${!isFolded}" title="${esc(p)}">${ICON.chevron}<span class="p-dot" style="background:${hue(p)}"></span><b>${esc(p.split("/").pop())}</b>${isFolded ? `<span class="p-sum">${esc(summary)}</span>` : `<span class="p-path">${esc(p.split("/").slice(0, -1).join("/"))}</span>`}<span class="n">${all.length}</span></button>`;
      if (isFolded) { html += `</div>`; continue; }
      html += active.map(row).join("");
      if (!active.length) html += `<div class="p-none">Nothing in work</div>`;
      // Postponed and Done: each its own folded line, opened on its own.
      for (const [col, name] of [["later", "Postponed"], ["done", "Done"]]) {
        const rs = quiet.filter((r) => groupOf(r) === col);
        if (!rs.length) continue;
        const key = `${p}|${col}`, isOpen = open.has(key) || words.length;
        html += `<button class="p-quiet${isOpen ? " open" : ""}" data-key="${esc(key)}">${ICON.chevron}${name}<span class="n">${rs.length}</span></button>`;
        if (isOpen) html += rs.map(row).join("");
      }
      html += `</div>`;
    }
    $("list").innerHTML = html || `<div class="empty">Nothing found</div>`;
    for (const b of $("list").querySelectorAll(".item")) b.onclick = () => select(b.dataset.id);
    for (const b of $("list").querySelectorAll(".p-ph")) b.onclick = () => {
      folded.has(b.dataset.proj) ? folded.delete(b.dataset.proj) : folded.add(b.dataset.proj); saveFold(); regroup();
    };
    for (const b of $("list").querySelectorAll(".p-quiet")) {
      b.onclick = () => { open.has(b.dataset.key) ? open.delete(b.dataset.key) : open.add(b.dataset.key); regroup(); };
    }
    queueMicrotask(() => { busy = false; });
  };
  new MutationObserver(regroup).observe($("list"), { childList: true });
  regroup();
  window.pRegroup = regroup;
})();

// The select's labels: "All tasks 36", "mako-agent 27", "Archive 12" — no "·".
(() => {
  const sel = document.getElementById("scope");
  // The select sits invisible over a label as wide as the chosen option, so the arrow follows the text.
  const wrap = document.createElement("span"); wrap.className = "p-scope";
  const label = document.createElement("span"); label.className = "p-scope-l";
  sel.parentElement.insertBefore(wrap, sel); wrap.append(label, sel);
  const tidy = () => {
    for (const o of sel.options) { const t = o.textContent.replace(/\s*·\s+/, " "); if (t !== o.textContent) o.textContent = t; }
    label.textContent = sel.selectedOptions[0]?.textContent || "";
    wrap.hidden = sel.hidden;
  };
  new MutationObserver(tidy).observe(sel, { childList: true, subtree: true, characterData: true, attributes: true });
  sel.addEventListener("change", tidy); new MutationObserver(tidy).observe($("list"), { childList: true }); tidy();
})();
