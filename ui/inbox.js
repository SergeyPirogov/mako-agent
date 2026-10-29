// Keeps the inbox current: reloads when tasks or agent states change, instead of a fixed refresh.
(() => {
  "use strict";
  const sig = document.body.dataset.sig;
  let busy = false;
  // Do not reload under the user's hand (a button being pressed submits a form anyway).
  document.addEventListener("pointerdown", () => (busy = true), true);
  document.addEventListener("pointerup", () => setTimeout(() => (busy = false), 500), true);
  setInterval(async () => {
    if (busy || document.hidden) return;
    try {
      const state = await (await fetch("/api/inbox-state", { cache: "no-store" })).json();
      if (state.sig !== sig) location.reload();
    } catch {
      // Mako restarting; try again on the next tick
    }
  }, 2000);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) fetch("/api/inbox-state", { cache: "no-store" }).then((r) => r.json()).then((s) => s.sig !== sig && location.reload()).catch(() => {});
  });
})();
