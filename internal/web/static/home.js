// Lets the owner hide, resize and reorder the home page sections.
(() => {
  const home = document.getElementById("home");
  const start = document.getElementById("home-customize");
  const bar = document.getElementById("home-bar");
  if (!home || !start || !bar) return;
  const items = () => [...home.querySelectorAll(":scope > .home-item")];
  let before = null;
  let dragging = null;

  const button = (text, label, fn) => {
    const b = document.createElement("button");
    b.type = "button";
    b.className = "ghost";
    b.textContent = text;
    b.setAttribute("aria-label", label);
    b.addEventListener("click", fn);
    return b;
  };

  const refresh = () => {
    const list = items();
    list.forEach((el, i) => {
      const t = el.querySelector(":scope > .home-tools");
      if (!t) return;
      t.querySelector("[data-up]").disabled = i === 0;
      t.querySelector("[data-down]").disabled = i === list.length - 1;
      const off = el.dataset.hidden === "1";
      el.classList.toggle("off", off);
      const hide = t.querySelector("[data-hide]");
      hide.textContent = off ? "Show" : "Hide";
      hide.setAttribute("aria-pressed", off ? "true" : "false");
      t.querySelector("[data-wide]").textContent = el.classList.contains("wide") ? "Make half width" : "Make full width";
    });
  };

  const tools = (el) => {
    const t = document.createElement("div");
    t.className = "home-tools";
    const name = document.createElement("b");
    name.textContent = "⠿ " + el.dataset.title;
    const up = button("↑", "Move " + el.dataset.title + " up", () => {
      if (el.previousElementSibling) home.insertBefore(el, el.previousElementSibling);
      refresh();
      up.focus();
    });
    up.dataset.up = "1";
    const down = button("↓", "Move " + el.dataset.title + " down", () => {
      if (el.nextElementSibling) home.insertBefore(el.nextElementSibling, el);
      refresh();
      down.focus();
    });
    down.dataset.down = "1";
    const wide = button("", "Change width of " + el.dataset.title, () => {
      el.classList.toggle("wide");
      refresh();
    });
    wide.dataset.wide = "1";
    const hide = button("Hide", "Hide or show " + el.dataset.title, () => {
      el.dataset.hidden = el.dataset.hidden === "1" ? "" : "1";
      refresh();
    });
    hide.dataset.hide = "1";
    t.append(name, up, down, wide, hide);
    el.prepend(t);
    el.draggable = true;
  };

  const stop = () => {
    home.classList.remove("editing");
    bar.hidden = true;
    start.hidden = false;
    items().forEach((el) => {
      el.querySelector(":scope > .home-tools")?.remove();
      el.draggable = false;
      el.classList.remove("off");
      el.hidden = el.dataset.hidden === "1";
    });
  };

  start.addEventListener("click", () => {
    before = items().map((el) => ({ el, wide: el.classList.contains("wide"), hidden: el.dataset.hidden }));
    home.classList.add("editing");
    bar.hidden = false;
    start.hidden = true;
    items().forEach((el) => {
      el.hidden = false;
      tools(el);
    });
    refresh();
  });

  document.getElementById("home-cancel").addEventListener("click", () => {
    before.forEach(({ el, wide, hidden }) => {
      home.append(el);
      el.classList.toggle("wide", wide);
      el.dataset.hidden = hidden || "";
    });
    stop();
  });

  document.getElementById("home-save").addEventListener("click", async (e) => {
    const save = e.currentTarget;
    const status = document.getElementById("home-status");
    const layout = items().map((el) => ({ id: el.dataset.id, hidden: el.dataset.hidden === "1", wide: el.classList.contains("wide") }));
    save.disabled = true;
    status.textContent = "Saving…";
    try {
      const res = await fetch("/home/layout", { method: "POST", credentials: "same-origin", body: new URLSearchParams({ layout: JSON.stringify(layout) }) });
      if (!res.ok) throw new Error();
      status.textContent = "";
      stop();
    } catch {
      status.textContent = "Couldn't save. Try again.";
    }
    save.disabled = false;
  });

  home.addEventListener("dragstart", (e) => {
    if (!home.classList.contains("editing")) return;
    dragging = e.target.closest(".home-item");
    if (dragging) {
      dragging.classList.add("dragging");
      e.dataTransfer.effectAllowed = "move";
      e.dataTransfer.setData("text/plain", dragging.dataset.id);
    }
  });
  home.addEventListener("dragover", (e) => {
    if (!dragging) return;
    e.preventDefault();
    const over = e.target.closest(".home-item");
    if (!over || over === dragging) return;
    const box = over.getBoundingClientRect();
    const after = e.clientY > box.top + box.height / 2 || (Math.abs(e.clientY - (box.top + box.height / 2)) < box.height / 4 && e.clientX > box.left + box.width / 2);
    home.insertBefore(dragging, after ? over.nextElementSibling : over);
  });
  home.addEventListener("dragend", () => {
    dragging?.classList.remove("dragging");
    dragging = null;
    refresh();
  });
})();
