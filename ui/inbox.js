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

  // The board scrolls sideways, which would clip a menu inside it: float the open menu over the page instead.
  document.addEventListener(
    "toggle",
    (e) => {
      const menu = e.target;
      if (!menu.matches?.("details.take-menu") || !menu.open) return;
      document.querySelectorAll("details.take-menu[open]").forEach((d) => d !== menu && (d.open = false));
      const pop = menu.querySelector(".take-pop");
      const anchor = menu.closest(".take").getBoundingClientRect();
      pop.style.position = "fixed";
      pop.style.left = `${Math.min(anchor.left, innerWidth - pop.offsetWidth - 12)}px`;
      const below = anchor.bottom + 6;
      pop.style.top = below + pop.offsetHeight > innerHeight - 8 ? `${Math.max(8, anchor.top - pop.offsetHeight - 6)}px` : `${below}px`;
    },
    true,
  );
  addEventListener("scroll", () => document.querySelectorAll("details.take-menu[open]").forEach((d) => (d.open = false)), true);

  // One "Куди відправити" menu at a time; a click elsewhere or Esc closes it.
  document.addEventListener("click", (e) => {
    document.querySelectorAll("details.take-menu[open]").forEach((d) => {
      if (!d.contains(e.target)) d.open = false;
    });
  });
  document.addEventListener("keydown", (e) => {
    if (e.key === "Escape") document.querySelectorAll("details.take-menu[open]").forEach((d) => (d.open = false));
  });

  // Drag a card from «Нові» to «В роботі» to take it, or from «Помилка» to retry it.
  const TOKEN = document.querySelector('meta[name="mako-token"]').content;
  let dragged = null;
  document.addEventListener("dragstart", (e) => {
    const card = e.target.closest?.(".kcard[draggable]");
    if (!card) return;
    dragged = card;
    busy = true;
    card.classList.add("dragging");
    e.dataTransfer.effectAllowed = "move";
    e.dataTransfer.setData("text/plain", card.dataset.id);
    document.querySelector('.kcol[data-col="doing"]')?.classList.add("drop-target");
  });
  document.addEventListener("dragend", () => {
    dragged?.classList.remove("dragging");
    dragged = null;
    busy = false;
    document.querySelectorAll(".kcol").forEach((c) => c.classList.remove("drop-target", "drop-over"));
  });
  const doing = document.querySelector('.kcol[data-col="doing"]');
  doing?.addEventListener("dragover", (e) => {
    if (!dragged) return;
    e.preventDefault();
    doing.classList.add("drop-over");
  });
  doing?.addEventListener("dragleave", (e) => {
    if (!doing.contains(e.relatedTarget)) doing.classList.remove("drop-over");
  });
  doing?.addEventListener("drop", async (e) => {
    e.preventDefault();
    if (!dragged) return;
    const action = dragged.dataset.from === "failed" ? "/retry" : "/take";
    const body = new URLSearchParams({ id: dragged.dataset.id, token: TOKEN });
    await fetch(action, { method: "POST", body, redirect: "manual" }).catch(() => {});
    location.reload();
  });

  async function check() {
    const state = await (await fetch("/api/inbox-state", { cache: "no-store" })).json();
    // Do not reload under an open "Куди відправити" menu.
    if (state.sig !== sig && !busy && !document.querySelector("details.take-menu[open], dialog.desc[open]")) return location.reload();
    search = state.search;
    drawClock();
    for (const el of document.querySelectorAll("[data-phase]")) {
      const text = state.phases?.[el.dataset.phase];
      if (text !== undefined && el.textContent !== text) {
        el.textContent = text || "\u00a0";
        el.title = text;
      }
    }
  }
  setInterval(() => {
    if (document.hidden) return;
    check().catch(() => {}); // Mako restarting; try again on the next tick
  }, 2000);
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) check().catch(() => {});
  });

  // The full task description, from the card's preview.
  document.addEventListener("click", (e) => {
    const preview = e.target.closest("[data-desc]");
    if (!preview) return;
    let dlg = document.querySelector("dialog.desc");
    if (!dlg) {
      dlg = document.createElement("dialog");
      dlg.className = "desc";
      dlg.addEventListener("click", (ev) => {
        if (ev.target === dlg || ev.target.closest(".desc-close")) dlg.close();
      });
      document.body.append(dlg);
    }
    dlg.innerHTML = '<button type="button" class="link desc-close" aria-label="Закрити">✕</button><div class="desc-in"></div>';
    const tpl = preview.nextElementSibling;
    dlg.classList.toggle("wide", Boolean(tpl.dataset.wide)); // summary next to its changes
    dlg.querySelector(".desc-in").append(tpl.content.cloneNode(true));
    dlg.querySelector(".sum-text")?.prepend(dlg.querySelector(".desc-close")); // keep ✕ off the changes toolbar
    // Read it, then act on it right here: the card's own buttons (Взяти ▾, Відкласти, …).
    const actions = preview.closest(".kcard")?.querySelector(".kc-actions");
    if (actions) {
      const foot = actions.cloneNode(true);
      foot.classList.add("desc-actions");
      foot.querySelectorAll("details[open]").forEach((d) => (d.open = false));
      foot.querySelectorAll("[data-desc], template").forEach((el) => el.remove()); // already open
      (dlg.querySelector(".sum-text") || dlg.querySelector(".desc-in")).append(foot);
    }
    dlg.showModal();
  });

  // «Взяти ▾»: picking who runs it is the decision, no second button.
  document.addEventListener("click", (e) => {
    const opt = e.target.closest("form.take-pop label.tp-opt");
    if (!opt) return;
    e.preventDefault();
    opt.querySelector("input").checked = true;
    opt.closest("form").requestSubmit();
  });
})();
