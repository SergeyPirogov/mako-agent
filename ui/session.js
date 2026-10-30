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

  finishBtn.addEventListener("click", async () => {
    const ok = await makoConfirm({
      title: "Завершити задачу?",
      body: "Mako закриє агента, закомітить зміни, запушить гілку й відкриє PR, а підсумок агента напише в задачу.",
      confirm: "Завершити й відкрити PR",
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
