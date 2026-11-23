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

  // ---------- auto-save ----------
  const MANUAL = new Set(["ADMIN_USER", "BASE_URL", "TRUST_PROXY", "WEBHOOK_TOKEN", "PLEX_ALLOW_WRITES"]);
  const bar = document.getElementById("settings-save-status");
  const queue = new Map(); // key -> {kind, value}
  const manualDirty = new Set();
  const failed = new Map();
  let saving = null;
  let saveTimer = 0;

  const rowFor = (key) => form.querySelector('tr[data-key="' + key + '"]');
  const labelFor = (key) => rowFor(key)?.querySelector("label b")?.textContent || key;

  function setBar(text, state) {
    if (!bar) return;
    bar.textContent = text;
    bar.dataset.state = state || "";
  }

  function refreshBar() {
    if (saving || queue.size) return setBar("Saving…", "saving");
    if (failed.size) return setBar(failed.size === 1 ? "1 field couldn't be saved. See the message beside it." : failed.size + " fields couldn't be saved. See the messages beside them.", "bad");
    if (manualDirty.size) return setBar("Click Save and apply to change: " + [...manualDirty].map(labelFor).join(", "), "warn");
    setBar("All changes saved.", "ok");
  }

  function fieldNote(key, text) {
    const row = rowFor(key);
    if (!row) return;
    let n = row.querySelector(".save-note");
    if (!text) { n?.remove(); return; }
    if (!n) {
      n = document.createElement("div");
      n.className = "save-note bad small";
      n.setAttribute("role", "alert");
      row.querySelector("td:nth-child(2)").append(n);
    }
    n.textContent = text;
  }

  function applySaved(key, f) {
    const row = rowFor(key);
    const el = form.elements.namedItem(key);
    if (!row || !el) return;
    if (row.dataset.type === "secret") {
      el.value = "";
      el.placeholder = f.is_set ? "•••••• set" : "not set";
    } else if (!queue.has(key)) {
      el.value = f.value;
    }
    const was = form.elements.namedItem("was_" + key);
    if (was) was.value = f.value;
    ["clear_", "reset_"].forEach((p) => {
      const box = form.elements.namedItem(p + key);
      if (box) box.checked = false;
    });
    const label = row.querySelector("td:last-child > span");
    if (label) {
      label.className = f.source === "saved" ? "ok" : f.source === "env" ? "warn" : "muted";
      label.textContent = f.source === "env" ? "from env" : f.source;
    }
  }

  async function sendQueued() {
    const batch = new Map(queue);
    queue.clear();
    const body = new URLSearchParams();
    batch.forEach((v, key) => {
      if (v.kind === "reset") body.set("reset_" + key, "1");
      else if (v.kind === "clear") body.set("clear_" + key, "1");
      else body.set(key, v.value);
    });
    let lastError = "Couldn't save.";
    for (let attempt = 0; attempt < 6; attempt++) {
      try {
        const res = await fetch("/settings/autosave", { method: "POST", credentials: "same-origin", body });
        if (res.redirected || res.status === 401) {
          lastError = "Sign in again to save.";
          break;
        }
        if (!res.ok) throw new Error();
        const data = await res.json();
        batch.forEach((_, key) => { failed.delete(key); fieldNote(key, ""); });
        Object.entries(data.saved || {}).forEach(([key, f]) => applySaved(key, f));
        Object.entries(data.errors || {}).forEach(([key, msg]) => { failed.set(key, msg); fieldNote(key, msg); });
        return;
      } catch {
        // The app may be reloading to apply an earlier change.
        await new Promise((r) => setTimeout(r, 1000 + attempt * 700));
      }
    }
    batch.forEach((_, key) => {
      failed.set(key, lastError);
      fieldNote(key, lastError + " Click Save and apply to try again.");
    });
  }

  function flushSaves() {
    clearTimeout(saveTimer);
    if (!saving && !queue.size) return Promise.resolve();
    if (!saving) {
      saving = (async () => {
        while (queue.size) await sendQueued();
      })().finally(() => {
        saving = null;
        refreshBar();
      });
    }
    refreshBar();
    return saving;
  }

  function queueSave(key, change) {
    if (MANUAL.has(key)) {
      manualDirty.add(key);
      refreshBar();
      return;
    }
    queue.set(key, change);
    refreshBar();
    clearTimeout(saveTimer);
    saveTimer = setTimeout(flushSaves, 350);
  }

  form.addEventListener("change", (e) => {
    const el = e.target;
    if (!el.name) return;
    const m = el.name.match(/^(reset|clear)_(.+)$/);
    if (m) {
      if (el.checked) queueSave(m[2], { kind: m[1] });
      return;
    }
    const row = rowFor(el.name);
    if (!row) return;
    if (el.validity?.badInput) {
      failed.set(el.name, "Enter a number.");
      fieldNote(el.name, "Enter a number. This field wasn't saved.");
      refreshBar();
      return;
    }
    const value = el.value.trim();
    if (row.dataset.type === "secret" && !value) return;
    queueSave(el.name, { kind: "value", value });
  });

  function leaving(e) {
    if (saving || queue.size || manualDirty.size) {
      e.preventDefault();
      e.returnValue = "";
    }
  }
  window.addEventListener("beforeunload", leaving);
  form.addEventListener("submit", () => window.removeEventListener("beforeunload", leaving));

  // ---------- connection tests ----------
  let probes = [];
  try { probes = JSON.parse(form.dataset.probes || "[]"); } catch { /* no test buttons */ }
  probes.forEach((p) => {
    const firstEl = p.keys.map((k) => form.elements.namedItem(k)).find(Boolean);
    if (!firstEl) return;
    const cell = firstEl.closest("td");
    const button = document.createElement("button");
    button.type = "button";
    button.className = "ghost conn-inline";
    button.textContent = "Test " + p.name;
    const out = document.createElement("div");
    out.className = "conn-result small";
    out.setAttribute("role", "status");
    cell.append(button, out);
    button.addEventListener("click", async () => {
      button.disabled = true;
      out.dataset.status = "testing";
      out.textContent = "Saving and testing…";
      document.activeElement?.blur?.();
      await new Promise((r) => setTimeout(r, 30));
      await flushSaves();
      const notSaved = p.keys.filter((k) => failed.has(k) || manualDirty.has(k));
      const body = new URLSearchParams({ id: p.id });
      p.keys.forEach((k) => {
        const el = form.elements.namedItem(k);
        if (el && el.value.trim()) body.set(k, el.value.trim());
      });
      out.textContent = "Testing…";
      try {
        const res = await fetch("/connections/test", { method: "POST", credentials: "same-origin", body });
        if (!res.ok || res.redirected) throw new Error();
        const r = await res.json();
        out.dataset.status = r.status;
        const head = document.createElement("b");
        head.textContent = r.headline;
        out.replaceChildren(head);
        (r.details || []).forEach((d) => {
          const line = document.createElement("div");
          line.className = "muted";
          line.textContent = d;
          out.append(line);
        });
        if (r.fix) {
          const fix = document.createElement("div");
          fix.textContent = "How to fix: " + r.fix;
          out.append(fix);
        }
        if (notSaved.length) {
          const warn = document.createElement("div");
          warn.className = "bad";
          warn.textContent = "Not saved yet: " + notSaved.map(labelFor).join(", ") + ". This test used what's typed.";
          out.append(warn);
        }
      } catch {
        out.dataset.status = "fail";
        out.textContent = "Couldn't run the test. Reload this page and sign in if asked.";
      }
      button.disabled = false;
    });
  });

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
      await flushSaves();
      const url = urlEl.value.trim();
      const key = keyEl.value.trim();
      const row = field(prefix + "_ROOT_FOLDER").closest("tr");
      const clear = form.elements.namedItem("clear_" + prefix + "_API_KEY");
      const hasKey = key || (keyEl.placeholder.startsWith("•") && !(clear && clear.checked));
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
