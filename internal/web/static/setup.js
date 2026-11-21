(() => {
  const $ = (id) => document.getElementById(id);

  async function post(url, fields) {
    const res = await fetch(url, { method: "POST", body: new URLSearchParams(fields || {}), credentials: "same-origin" });
    let data = {};
    try { data = await res.json(); } catch { /* handled below */ }
    if (!res.ok) throw new Error(data.error || "Something went wrong. Try again.");
    return data;
  }

  // Saving reloads the app for a moment. Wait for the new one, then refresh.
  async function reloadWhenBack() {
    const bootOf = async () => {
      try {
        const r = await fetch("/healthz", { cache: "no-store" });
        return r.ok ? r.headers.get("X-Backtrackarr-Boot") : null;
      } catch { return null; }
    };
    const before = await bootOf();
    for (let i = 0; i < 40; i++) {
      await new Promise((r) => setTimeout(r, 200));
      const now = await bootOf();
      if (now && now !== before) break;
    }
    location.reload();
  }

  // Fill blanks from the browser.
  const tz = $("tz");
  if (tz && !tz.value) { try { tz.value = Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch { /* leave blank */ } }
  const region = $("region");
  if (region && !region.value) { const m = (navigator.language || "").match(/-([A-Za-z]{2})$/); if (m) region.value = m[1].toUpperCase(); }

  // ---------- Sign in with Plex ----------
  const signin = $("plex-signin");
  const status = $("plex-status");
  const servers = $("plex-servers");
  let popup = null;

  function finished(name) {
    if (popup && !popup.closed) popup.close();
    status.textContent = "Connected to " + name + ". Loading your users and libraries…";
    reloadWhenBack();
  }

  function fail(err) {
    if (popup && !popup.closed) popup.close();
    status.textContent = err.message;
    status.className = "small bad";
    signin.disabled = false;
  }

  async function watch(id) {
    for (let i = 0; i < 300; i++) {
      await new Promise((r) => setTimeout(r, 2000));
      const s = await post("/setup/plex/status", { id });
      if (s.state === "done") return finished(s.server);
      if (s.state === "choose") {
        if (popup && !popup.closed) popup.close();
        status.textContent = "Which server should BackTrackarr use?";
        servers.replaceChildren(...s.servers.map((v) => {
          const b = document.createElement("button");
          b.type = "button";
          b.textContent = v.name;
          b.className = "ghost";
          b.onclick = async () => {
            status.textContent = "Connecting…";
            try { finished((await post("/setup/plex/choose", { id, index: v.index })).server); } catch (e) { fail(e); }
          };
          return b;
        }));
        return;
      }
    }
    throw new Error("Sign-in timed out. Try again.");
  }

  if (signin) signin.onclick = async () => {
    signin.disabled = true;
    status.className = "small";
    status.textContent = "Opening Plex…";
    popup = window.open("about:blank", "plex-signin", "width=600,height=720"); // must open during the click
    try {
      const s = await post("/setup/plex/start");
      if (popup) {
        popup.location = s.auth_url;
        status.textContent = "Approve BackTrackarr in the Plex window.";
      } else {
        status.textContent = "";
        const a = document.createElement("a");
        a.href = s.auth_url; a.target = "_blank"; a.rel = "noopener";
        a.textContent = "Your browser blocked the window. Click here to sign in with Plex.";
        status.append(a);
      }
      await watch(s.id);
    } catch (e) { fail(e); }
  };

  // ---------- Find my apps ----------
  const find = $("find-apps");
  const findStatus = $("find-status");
  const found = $("found-apps");

  function option(value) {
    const o = document.createElement("option");
    o.value = value; o.textContent = value;
    return o;
  }

  function labelled(text, el) {
    const l = document.createElement("label");
    l.className = "small";
    l.append(text + " ", el);
    return l;
  }

  function card(app) {
    const row = document.createElement("div");
    row.className = "found";
    const title = document.createElement("div");
    const b = document.createElement("b"); b.textContent = app.label;
    const u = document.createElement("span"); u.className = "muted mono small"; u.textContent = " " + app.url;
    title.append(b, u);
    const open = document.createElement("a");
    open.href = app.key_url; open.target = "_blank"; open.rel = "noopener noreferrer";
    open.textContent = "Open to copy key ↗";
    const help = document.createElement("span"); help.className = "muted small"; help.textContent = "Find it under " + app.key_help;
    const input = document.createElement("input");
    input.type = "password"; input.placeholder = "Paste key"; input.autocomplete = "off";
    input.setAttribute("aria-label", app.label + " key");
    const save = document.createElement("button"); save.type = "button"; save.textContent = "Connect";
    const msg = document.createElement("span"); msg.className = "small"; msg.setAttribute("role", "status");
    const extra = document.createElement("div"); extra.className = "row"; extra.hidden = true;
    let opts = null, nameEl, rootEl, profileEl, adultEl;

    function showOptions(o) {
      opts = o;
      nameEl = document.createElement("input"); nameEl.value = o.name || ""; nameEl.placeholder = "e.g. radarr-4k";
      rootEl = document.createElement("select"); rootEl.append(...(o.roots || []).map(option));
      profileEl = document.createElement("select"); profileEl.append(...(o.profiles || []).map(option));
      extra.replaceChildren(labelled("Name", nameEl), labelled("Root folder", rootEl), labelled("Quality", profileEl));
      if (!o.always_adult) {
        adultEl = document.createElement("input"); adultEl.type = "checkbox";
        const l = document.createElement("label"); l.className = "small";
        l.append(adultEl, " Adult content");
        extra.append(l);
      }
      extra.hidden = false;
      input.readOnly = true;
      save.textContent = "Save";
      msg.className = "small ok"; msg.textContent = "✓ Key works. Check these, then save.";
    }

    save.onclick = async () => {
      save.disabled = true; msg.className = "small"; msg.textContent = "Checking…";
      try {
        const fields = { kind: app.kind, url: app.url, key: input.value };
        if (!opts) {
          const o = await post("/setup/app/options", fields);
          if (o.is_arr) { showOptions(o); save.disabled = false; return; }
        } else {
          fields.name = nameEl.value;
          fields.root = rootEl.value;
          fields.profile = profileEl.value;
          if (adultEl && adultEl.checked) fields.adult = "1";
        }
        await post("/setup/app", fields);
        msg.className = "small ok"; msg.textContent = "✓ Connected";
        reloadWhenBack();
      } catch (e) { msg.className = "small bad"; msg.textContent = e.message; save.disabled = false; }
    };
    const controls = document.createElement("div"); controls.className = "row";
    controls.append(open, input, save, msg);
    row.append(title, help, controls, extra);
    return row;
  }

  const addApp = $("add-app");
  if (addApp) addApp.onclick = () => {
    const sel = $("add-kind"), addr = $("add-url");
    let url = addr.value.trim().replace(/\/+$/, "");
    if (!url) { addr.focus(); return; }
    if (!/^https?:\/\//i.test(url)) url = "http://" + url;
    const o = sel.options[sel.selectedIndex];
    $("manual-apps").append(card({ kind: o.value, label: o.textContent, url, key_url: url + o.dataset.path, key_help: o.dataset.help }));
    addr.value = "";
  };

  if (find) find.onclick = async () => {
    find.disabled = true;
    findStatus.className = "small";
    findStatus.textContent = "Looking on your network. This can take up to a minute…";
    found.replaceChildren();
    try {
      const { apps } = await post("/setup/scan");
      findStatus.textContent = apps.length ? "Found " + apps.length + ". Paste a key for the ones you want." : "No new apps found.";
      found.replaceChildren(...apps.map(card));
    } catch (e) { findStatus.className = "small bad"; findStatus.textContent = e.message; }
    find.disabled = false;
  };
})();
