(() => { const mode = new URLSearchParams(location.search).get("scrolltest"); if (!mode) return;
  const out = []; const l = () => document.getElementById("list");
  const step = (f, ms = 400) => new Promise((r) => setTimeout(() => { f(); r(); }, ms));
  const t = setInterval(async () => {
    if (!document.querySelector(".item")) return; clearInterval(t);
    await step(() => document.querySelectorAll(".p-quiet")[1]?.click());
    await step(() => { l().scrollTop = 350; out.push(`set=${l().scrollTop}`); });
    await step(() => { const q = [...document.querySelectorAll(".p-quiet")].find((b) => b.getBoundingClientRect().top > 300); out.push(`fold=${q?.dataset.key}`); q?.click(); });
    await step(() => out.push(`afterFold=${l().scrollTop}`));
    await step(() => { document.querySelector(".p-grp").click(); });
    await step(() => out.push(`afterToggle=${l().scrollTop}`));
    document.documentElement.dataset.st = out.join(" ");
  }, 100); })();
