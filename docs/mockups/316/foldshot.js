// ?fold=a,b: fold these projects (by short name) before the shot.
(() => { const f = new URLSearchParams(location.search).get("fold"); if (!f) return;
  const names = f.split(","); const t = setInterval(() => { const hs = [...document.querySelectorAll(".p-ph")]; if (!hs.length) return; clearInterval(t);
    for (const h of hs) if (names.includes(h.dataset.proj.split("/").pop())) document.querySelector(`.p-ph[data-proj="${h.dataset.proj}"]`).click(); }, 100); })();
