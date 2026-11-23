// Settings page: filter by importance, and fill Sonarr, Radarr and Whisparr
// root folders and quality profiles from the apps themselves.
(() => {
  const form = document.getElementById("settings-form");
  if (!form) return;

  // ---------- filter ----------
  const buttons = [...document.querySelectorAll("[data-filter]")];
  const groups = [...form.querySelectorAll("details.panel")];
  const defaultOpen = new Map(groups.map((g) => [g, g.open]));
  const note = document.getElementById("settings-filter-note");
  const remember = (v) => { try { localStorage.setItem("settings-filter", v); } catch { /* optional */ } };
  const recalled = () => { try { return localStorage.getItem("settings-filter") || "all"; } catch { return "all"; } };

  function applyFilter(level, save) {
    if (!buttons.some((b) => b.dataset.filter === level)) level = "all";
    let shown = 0;
    groups.forEach((g) => {
      let inGroup = 0;
      g.querySelectorAll("tr[data-level]").forEach((row) => {
        const match = level === "all" || row.dataset.level === level;
        row.hidden = !match;
        if (match) inGroup++;
      });
      g.hidden = inGroup === 0;
      g.open = level === "all" ? defaultOpen.get(g) : inGroup > 0;
      shown += inGroup;
    });
    buttons.forEach((b) => {
      const on = b.dataset.filter === level;
      b.classList.toggle("on", on);
      b.setAttribute("aria-pressed", on ? "true" : "false");
    });
    if (note) note.textContent = level === "all" ? "" : shown + " " + level + " setting" + (shown === 1 ? "" : "s");
    if (save) remember(level);
  }
  buttons.forEach((b) => b.addEventListener("click", () => applyFilter(b.dataset.filter, true)));
  applyFilter(recalled(), false);

  // ---------- root folders and quality profiles ----------
  const field = (name) => form.elements.namedItem(name);

  function toSelect(el, values, blank) {
    const current = el.value.trim();
    const select = document.createElement("select");
    select.id = el.id;
    select.name = el.name;
    const add = (value, label) => {
      const o = document.createElement("option");
      o.value = value;
      o.textContent = label;
      select.append(o);
    };
    add("", blank);
    values.forEach((v) => add(v, v));
    if (current && !values.includes(current)) add(current, current + " (saved, not found in the app)");
    select.value = current;
    el.replaceWith(select);
    return select;
  }

  function status(row, text, bad) {
    let s = row.querySelector(".arr-status");
    if (!s) {
      s = document.createElement("div");
      s.className = "arr-status small";
      s.setAttribute("role", "status");
      row.querySelector("td:nth-child(2)").append(s);
    }
    s.textContent = text;
    s.classList.toggle("bad", !!bad);
    s.classList.toggle("muted", !bad);
  }

  form.querySelectorAll('input[name$="_ROOT_FOLDER"]').forEach((rootInput) => {
    const prefix = rootInput.name.replace(/_ROOT_FOLDER$/, "");
    const urlEl = field(prefix + "_URL");
    const keyEl = field(prefix + "_API_KEY");
    if (!urlEl || !keyEl) return;
    let seq = 0;
    let timer = 0;

    async function load() {
      const url = urlEl.value.trim();
      const key = keyEl.value.trim();
      const row = field(prefix + "_ROOT_FOLDER").closest("tr");
      const clear = form.elements.namedItem("clear_" + prefix + "_API_KEY");
      const hasKey = key || (keyEl.placeholder.includes("set") && !(clear && clear.checked));
      if (!url || !hasKey) return;
      const mine = ++seq;
      status(row, "Reading from the app…");
      try {
        const res = await fetch("/settings/arr-options", { method: "POST", credentials: "same-origin",
          body: new URLSearchParams({ prefix, url, key }) });
        const data = await res.json();
        if (mine !== seq) return;
        if (data.error) {
          status(row, data.error, true);
          return;
        }
        const roots = data.roots || [];
        const profiles = data.profiles || [];
        const newRoot = toSelect(field(prefix + "_ROOT_FOLDER"), roots, "First root folder (default)");
        toSelect(field(prefix + "_QUALITY_PROFILE"), profiles, "First profile (default)");
        status(newRoot.closest("tr"), "Found " + roots.length + " root folder" + (roots.length === 1 ? "" : "s") +
          " and " + profiles.length + " quality profile" + (profiles.length === 1 ? "" : "s") + ".");
      } catch {
        if (mine === seq) status(row, "Couldn't reach BackTrackarr. Try again.", true);
      }
    }
    const later = () => { clearTimeout(timer); timer = setTimeout(load, 500); };
    urlEl.addEventListener("change", later);
    keyEl.addEventListener("change", later);
    load();
  });
})();
