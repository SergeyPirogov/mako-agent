// «Трекер»: filters the issue list by text, assignee and whether it is already in Mako; kept across reloads.
(() => {
  "use strict";
  const search = document.getElementById("tr-search");
  const who = document.getElementById("tr-who");
  const hide = document.getElementById("tr-hide-board");
  const count = document.getElementById("tr-count");
  const rows = [...document.querySelectorAll(".tr-table tbody tr")];
  const KEY = "mako-tracker-filters";
  try {
    const saved = JSON.parse(sessionStorage.getItem(KEY) || "{}");
    search.value = saved.q || "";
    who.value = saved.who || "";
    hide.checked = Boolean(saved.hide);
  } catch {}

  function apply() {
    const words = search.value.toLowerCase().split(/\s+/).filter(Boolean);
    let shown = 0;
    for (const row of rows) {
      const ok =
        words.every((w) => row.dataset.search.includes(w)) &&
        (!who.value || row.dataset.who === who.value) &&
        !(hide.checked && (row.dataset.kind === "board" || row.dataset.kind === "archived"));
      row.hidden = !ok;
      if (ok) shown++;
    }
    for (const table of document.querySelectorAll(".tr-table")) {
      table.hidden = ![...table.querySelectorAll("tbody tr")].some((r) => !r.hidden);
    }
    count.textContent = shown === rows.length ? `${rows.length} задач` : `${shown} з ${rows.length}`;
    try {
      sessionStorage.setItem(KEY, JSON.stringify({ q: search.value, who: who.value, hide: hide.checked }));
    } catch {}
  }
  search.addEventListener("input", apply);
  who.addEventListener("change", apply);
  hide.addEventListener("change", apply);
  document.addEventListener("keydown", (e) => {
    if (e.key === "/" && document.activeElement !== search) {
      e.preventDefault();
      search.focus();
    }
  });
  apply();
})();
