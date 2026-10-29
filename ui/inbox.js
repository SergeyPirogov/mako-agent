// Keeps the inbox current: reloads when tasks or agent states change, and ticks the "next search" clock.
(() => {
  "use strict";
  const sig = document.body.dataset.sig;
  let search = JSON.parse(document.body.dataset.search || "null");
  let busy = false;
  // Do not reload under the user's hand (a button being pressed submits a form anyway).
  document.addEventListener("pointerdown", () => (busy = true), true);
  document.addEventListener("pointerup", () => setTimeout(() => (busy = false), 500), true);

  const clockText = document.getElementById("clock-text");
  const searchNow = document.getElementById("search-now");
  const time = (ms) => new Date(ms).toLocaleTimeString("uk-UA", { hour: "2-digit", minute: "2-digit", second: "2-digit", hour12: false });

  // Tasks already taken still match the tracker query until they are done, so only new ones are news.
  function lastText(last) {
    if (!last) return "";
    const known = last.found - last.added;
    let text = last.added ? `нових задач: ${last.added}` : "нових задач немає";
    if (known > 0) text += ` (${known} вже в Mako)`;
    if (last.errors) text += `, помилок: ${last.errors}`;
    return ` · останній пошук о ${time(last.at)}: ${text}`;
  }

  function drawClock() {
    if (!clockText) return;
    searchNow.hidden = true;
    if (!search) {
      clockText.textContent = "";
      return;
    }
    let text;
    if (search.state === "searching") text = "🔎 Шукаю задачі в трекерах…";
    else if (search.state === "paused") text = "⏸ На паузі: нові задачі не шукаються";
    else if (search.state === "wip-full") text = `⏸ Не шукаю нових задач: в роботі ${search.wip} з ${search.limit}. Почну, щойно звільниться місце`;
    else if (!search.nextAt) text = "🕐 Перший пошук задач ось-ось почнеться";
    else {
      const left = Math.max(0, Math.round((search.nextAt - Date.now()) / 1000));
      const mm = Math.floor(left / 60);
      const ss = String(left % 60).padStart(2, "0");
      text = left > 0 ? `🕐 Наступний пошук задач через ${mm}:${ss} (о ${time(search.nextAt)})` : "🔎 Шукаю задачі в трекерах…";
      searchNow.hidden = left === 0;
    }
    clockText.textContent = text + lastText(search.last);
  }

  drawClock();
  setInterval(drawClock, 1000);

  async function check() {
    const state = await (await fetch("/api/inbox-state", { cache: "no-store" })).json();
    if (state.sig !== sig && !busy) return location.reload();
    search = state.search;
    drawClock();
  }
  setInterval(() => {
    if (document.hidden) return;
    check().catch(() => {}); // Mako restarting; try again on the next tick
  }, 2000);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) check().catch(() => {});
  });
})();
