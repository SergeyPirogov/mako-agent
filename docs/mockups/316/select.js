// Opens the task named in ?sel= once the board has its data, so both shots show the same task.
(() => { const id = new URLSearchParams(location.search).get("sel"); if (!id) return;
  const t = setInterval(() => { if (typeof data !== "undefined" && data && document.querySelector(".item")) { clearInterval(t); select(id); } }, 100); })();
