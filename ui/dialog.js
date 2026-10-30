// In-page confirmation dialog in Mako's style, instead of the browser's native confirm().
(() => {
  "use strict";
  /**
   * makoConfirm({ title, body, confirm, cancel, tone }) → Promise<boolean>
   * tone: "primary" | "success" | "danger" colours the confirm button like the action it triggers.
   */
  window.makoConfirm = ({ title, body = "", confirm = "Так", cancel = "Скасувати", tone = "primary" }) =>
    new Promise((resolve) => {
      const dialog = document.createElement("dialog");
      dialog.className = "mdialog";
      dialog.innerHTML = `<form method="dialog">
          <h2></h2><p class="mbody"></p>
          <div class="mactions"><button value="cancel" class="mcancel"></button><button value="ok" class="mok ${tone}"></button></div>
        </form>`;
      dialog.querySelector("h2").textContent = title;
      dialog.querySelector(".mbody").textContent = body;
      dialog.querySelector(".mcancel").textContent = cancel;
      dialog.querySelector(".mok").textContent = confirm;
      // Clicking the backdrop (outside the box) is a "no".
      dialog.addEventListener("click", (e) => {
        if (e.target === dialog) dialog.close("cancel");
      });
      dialog.addEventListener("close", () => {
        resolve(dialog.returnValue === "ok");
        dialog.remove();
      });
      document.body.append(dialog);
      dialog.showModal();
      // A destructive action starts on "no", so Enter does not destroy by accident.
      dialog.querySelector(tone === "danger" ? ".mcancel" : ".mok").focus();
    });

  /** A sent form cannot be sent twice: its buttons go off, the pressed one says what is happening (data-busy). */
  const lock = (form) => {
    for (const b of form.querySelectorAll("button")) b.disabled = true;
    const label = form.dataset.busy;
    const main = form.querySelector("button");
    if (label && main) main.textContent = label;
    form.classList.add("busy");
  };
  window.makoLock = lock;

  // Forms with data-confirm ask first. data-confirm-title, -ok and -tone describe the action; the defaults fit «Скасувати».
  document.addEventListener("submit", async (e) => {
    const form = e.target;
    if (form.method !== "post") return;
    if (!form.dataset?.confirm || form.dataset.confirmed) {
      // after the browser has collected the form's fields
      setTimeout(() => lock(form), 0);
      return;
    }
    e.preventDefault();
    const ok = await window.makoConfirm({
      title: form.dataset.confirmTitle || "Скасувати задачу?",
      body: form.dataset.confirm,
      confirm: form.dataset.confirmOk || "Скасувати задачу",
      cancel: "Ні",
      tone: form.dataset.confirmTone || "danger",
    });
    if (ok) {
      form.dataset.confirmed = "1";
      lock(form);
      form.submit();
    }
  });
})();
