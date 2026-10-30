// Task log: refreshes while the task runs, keeping the log's scroll position (only the log scrolls).
(() => {
  "use strict";
  const key = `mako.tlog.${location.search}`;
  const list = () => document.querySelector("ol.tlog");
  try {
    const top = Number(sessionStorage.getItem(key));
    if (top && list()) list().scrollTop = top;
  } catch {
    // storage blocked: start at the top
  }
  list()?.addEventListener("scroll", () => {
    try {
      sessionStorage.setItem(key, String(list().scrollTop));
    } catch {
      // not remembered
    }
  });
  if (!document.body.dataset.live) return;
  setInterval(() => {
    // Not while the developer selects text to copy.
    if (String(getSelection() ?? "").length) return;
    location.reload();
  }, 3000);
})();
