// Live terminal of an agent session: xterm.js over a WebSocket to Mako.
(() => {
  "use strict";
  const TOKEN = document.querySelector('meta[name="mako-token"]').content;
  const id = document.body.dataset.session;
  const statusEl = document.getElementById("status");
  const statusMsg = document.getElementById("status-msg");
  const finishBtn = document.getElementById("finish");
  const cancelBtn = document.getElementById("cancel");
  const baseTitle = document.title;

  const STATUS = {
    starting: "Запуск",
    working: "Працює",
    waiting: "Чекає на вас",
    idle: "Закінчив хід",
    exited: "Сесію завершено",
  };

  const term = new Terminal({
    cursorBlink: true,
    fontFamily: 'ui-monospace, SFMono-Regular, Menlo, Consolas, "Liberation Mono", monospace',
    fontSize: 13,
    scrollback: 20000,
    allowProposedApi: true,
    theme: { background: "#14161b", foreground: "#e6e6e6", cursor: "#4c8dff", selectionBackground: "#3a4d7a" },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(document.getElementById("term"));
  fit.fit();
  term.focus();

  let ws;
  let ended = false;

  function send(msg) {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify(msg));
  }

  function setStatus(status, message) {
    statusEl.className = `s ${status}`;
    statusEl.textContent = STATUS[status] || status;
    statusMsg.textContent = message || (status === "idle" ? "Перевірте результат: натисніть «Завершити» або дайте агенту ще інструкції." : "");
    const attention = status === "waiting" || status === "idle";
    document.title = (attention ? "● " : "") + baseTitle;
    const icon = document.querySelector('link[rel="icon"]');
    if (icon) icon.href = attention ? "/ui/favicon-attention.png" : "/ui/favicon.png";
    if (status === "exited") end();
  }

  function end() {
    ended = true;
    finishBtn.disabled = cancelBtn.disabled = true;
    statusMsg.innerHTML = 'Сесію завершено. Mako комітить, пушить і відкриває PR. Результат буде на <a href="/">дошці задач</a>.';
  }

  function connect() {
    ws = new WebSocket(`ws://${location.host}/ws/session/${encodeURIComponent(id)}?token=${TOKEN}`);
    ws.onopen = () => send({ t: "r", cols: term.cols, rows: term.rows });
    ws.onmessage = (ev) => {
      const msg = JSON.parse(ev.data);
      if (msg.t === "init") {
        term.reset();
        term.write(msg.d);
        setStatus(msg.status, msg.message);
      } else if (msg.t === "o") {
        term.write(msg.d);
      } else if (msg.t === "s") {
        setStatus(msg.status, msg.message);
      } else if (msg.t === "x") {
        setStatus("exited");
      }
    };
    ws.onclose = () => {
      if (!ended) {
        statusMsg.textContent = "Звʼязок втрачено, перепідключаюсь…";
        setTimeout(connect, 2000);
      }
    };
  }

  term.onData((d) => send({ t: "i", d }));

  // Refit whenever the terminal's box changes: window resize, the status line appearing, the changes panel opening.
  let resizeTimer;
  new ResizeObserver(() => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => {
      fit.fit();
      send({ t: "r", cols: term.cols, rows: term.rows });
    }, 80);
  }).observe(document.getElementById("term"));

  // «Створити MR»: commit with a message from Claude, push, open the MR; the agent keeps working.
  const publishBtn = document.getElementById("publish");
  const reviewLink = document.getElementById("review-link");
  const review = document.body.dataset.review || "MR";
  publishBtn?.addEventListener("click", async () => {
    const label = publishBtn.textContent;
    publishBtn.disabled = true;
    publishBtn.textContent = "Комічу й пушу…";
    statusMsg.textContent = `Claude пише commit message, потім Mako запушить гілку й відкриє ${review}…`;
    try {
      const res = await fetch(`/api/publish?task=${encodeURIComponent(document.body.dataset.task)}`, {
        method: "POST",
        headers: { "X-Mako-Token": TOKEN },
      });
      const data = await res.json().catch(() => ({}));
      if (!res.ok) throw new Error(data.error || res.statusText);
      if (data.url) {
        reviewLink.href = data.url;
        reviewLink.hidden = false;
        setTimeout(loadReview, 1500);
      }
      publishBtn.textContent = `Оновити ${review}`;
      const commit = data.message ? ` (новий коміт «${data.message.split("\n")[0]}»)` : "";
      statusMsg.textContent =
        (data.url
          ? `${data.updated ? `${review} оновлено` : `${review} відкрито`}: «${data.title}»${commit}. Коли його змерджать, натисніть «Завершити».`
          : `Гілку запушено${commit}, але ${review} не відкрито: перевірте налаштування репозиторію.`) +
        (data.warning ? ` ⚠ ${data.warning}` : "");
    } catch (err) {
      publishBtn.textContent = label;
      statusMsg.textContent = `${review} не створено: ${err.message}`;
    } finally {
      publishBtn.disabled = false;
    }
  });

  // The MR next to the agent: CI, review threads, conflicts, merge; the next step goes to the agent as a message.
  const mrBar = document.getElementById("mr-bar");
  const PIPE = {
    success: ["ok", "✓ CI"], failed: ["err", "✗ CI впав"], running: ["run", "⏳ CI йде"], pending: ["run", "⏳ CI чекає"],
    created: ["run", "⏳ CI"], canceled: ["mute", "CI скасовано"], skipped: ["mute", "CI пропущено"], manual: ["mute", "CI чекає ручного запуску"],
  };
  const esc = (t) => String(t ?? "").replace(/[&<>"']/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" })[c]);
  async function loadReview() {
    if (!reviewLink || reviewLink.hidden) return;
    const res = await fetch(`/api/review?task=${encodeURIComponent(document.body.dataset.task)}`, { headers: { "X-Mako-Token": TOKEN } });
    const data = await res.json().catch(() => ({}));
    const r = data.review;
    if (data.reviewState === "merged") {
      mrBar.hidden = false;
      mrBar.innerHTML = `<div class="kc-mr-facts"><span class="rv ok">✓ ${review} змерджено</span><span class="muted">Задача отримала фінальний статус. Натисніть «Завершити».</span></div>`;
      return;
    }
    if (!r || r.state !== "open") return void (mrBar.hidden = true);
    const p = r.pipeline ? PIPE[r.pipeline.status] || ["mute", `CI: ${r.pipeline.status}`] : null;
    const facts = [
      p ? `<a class="rv ${p[0]}" href="${esc(r.pipeline.url || data.prUrl)}" target="_blank" rel="noreferrer">${p[1]}</a>` : "",
      r.conflicts ? `<span class="rv err">⚠ конфлікти</span>` : "",
      r.comments.length ? `<a class="rv warn" href="${esc(r.comments[0].url || data.prUrl)}" target="_blank" rel="noreferrer">💬 ${r.comments.length}</a>` : "",
      r.mergeable ? `<span class="rv ok">готовий до мерджу</span>` : "",
    ].join("");
    const btn = (act, label, cls) => `<button data-mr="${act}" class="${cls}">${label}</button>`;
    const buttons = [
      r.pipeline?.status === "failed" ? btn("ci", "Агенту: полагодити CI", "soft") : "",
      r.comments.length ? btn("comments", `Агенту: виправити коментарі (${r.comments.length})`, "soft") : "",
      r.conflicts ? btn("conflicts", "Агенту: розвʼязати конфлікти", "soft") : "",
      r.mergeable ? btn("merge", "Змерджити", "success") : "",
    ].join("");
    mrBar.hidden = false;
    mrBar.innerHTML = `<div class="kc-mr-facts"><span class="muted">${review}:</span>${facts || '<span class="rv mute">перевіряю…</span>'}</div>${buttons ? `<div class="kc-mr-actions">${buttons}</div>` : ""}`;
  }
  mrBar.addEventListener("click", async (e) => {
    const b = e.target.closest("button[data-mr]");
    if (!b) return;
    const act = b.dataset.mr;
    if (act === "merge") {
      const ok = await makoConfirm({ title: `Змерджити ${review}?`, body: `${review} буде змерджено в основну гілку, гілку задачі буде видалено. Задача отримає фінальний статус і закриється.`, confirm: "Змерджити", cancel: "Ні", tone: "success" });
      if (!ok) return;
    }
    b.disabled = true;
    statusMsg.textContent = act === "merge" ? `Мерджу ${review}…` : "Збираю деталі й передаю агенту…";
    const task = encodeURIComponent(document.body.dataset.task);
    const res = await fetch(act === "merge" ? `/api/review/merge?task=${task}` : `/api/review/ask?task=${task}&kind=${act}`, { method: "POST", headers: { "X-Mako-Token": TOKEN } });
    const data = await res.json().catch(() => ({}));
    b.disabled = false;
    if (!res.ok) statusMsg.textContent = data.error || res.statusText;
    else if (act === "merge") statusMsg.textContent = `${review} змерджено.`;
    else statusMsg.textContent = "Агент отримав запит у терміналі. Коли закомітить, натисніть «Оновити " + review + "».";
    loadReview();
    term.focus();
  });
  loadReview();
  setInterval(() => !document.hidden && loadReview(), 15_000);

  finishBtn.addEventListener("click", async () => {
    const ok = await makoConfirm({
      title: "Завершити задачу?",
      body: reviewLink && !reviewLink.hidden
        ? `Mako закриє агента й напише його підсумок у задачу. Якщо після ${review} були нові зміни, Mako їх закомітить і допушить.`
        : `Mako закриє агента, закомітить зміни, запушить гілку й відкриє ${review}, а підсумок агента напише в задачу.`,
      confirm: reviewLink && !reviewLink.hidden ? "Завершити" : `Завершити й відкрити ${review}`,
      cancel: "Ще ні",
      tone: "success",
    });
    if (ok) send({ t: "finish" });
    term.focus();
  });
  cancelBtn.addEventListener("click", async () => {
    const ok = await makoConfirm({
      title: "Скасувати задачу?",
      body: "Агента буде зупинено, PR не створюватиметься. Зміни лишаться у worktree, задачу можна буде повторити.",
      confirm: "Зупинити без PR",
      cancel: "Повернутися",
      tone: "danger",
    });
    if (ok) send({ t: "cancel" });
    term.focus();
  });

  // Changes panel next to the terminal; remembered between visits.
  const panel = document.getElementById("changes-panel");
  const toggle = document.getElementById("toggle-changes");
  const setPanel = (open) => {
    panel.hidden = !open;
    if (open && !panel.src) panel.src = panel.dataset.src;
    toggle.classList.toggle("on", open);
    try {
      localStorage.setItem("mako.changesPanel", open ? "1" : "0");
    } catch {
      // storage unavailable: the panel just starts closed
    }
  };
  toggle.addEventListener("click", () => setPanel(panel.hidden));
  try {
    if (localStorage.getItem("mako.changesPanel") === "1") setPanel(true);
  } catch {
    // ignore
  }

  connect();
})();
