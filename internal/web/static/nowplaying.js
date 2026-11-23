// Keeps the dashboard's Now Playing panel current.
(() => {
  const box = document.getElementById("now-playing");
  if (!box) return;
  const list = box.querySelector("[data-list]");
  let busy = false;
  let holdUntil = 0;

  function row(p) {
    const el = document.createElement("div");
    el.className = "np";
    const top = document.createElement("div");
    const title = document.createElement("b");
    title.textContent = p.display;
    const state = document.createElement("span");
    state.className = "tag " + (p.state === "pause" ? "" : "ok");
    state.textContent = p.state === "pause" ? "paused" : "playing";
    top.append(state, title);
    const head = document.createElement("div");
    head.className = "np-head";
    const end = document.createElement("button");
    end.type = "button";
    end.className = "ghost np-end";
    end.textContent = "End session";
    end.setAttribute("aria-label", "End session: " + p.display);
    end.addEventListener("click", () => endSession(p, end));
    head.append(top, end);
    const meta = document.createElement("div");
    meta.className = "muted small";
    meta.textContent = [p.user, p.player || p.source].filter(Boolean).join(" · ");
    const bar = document.createElement("span");
    bar.className = "track";
    const fill = document.createElement("span");
    fill.className = "fill";
    fill.style.width = Math.max(0, Math.min(100, p.progress || 0)) + "%";
    bar.append(fill);
    const at = Math.max(0, Math.min(100, p.finish_at || 90));
    const mark = document.createElement("span");
    mark.className = "mark";
    mark.style.left = at + "%";
    bar.append(mark);
    const note = document.createElement("div");
    note.className = "muted small";
    note.textContent = p.counted ? "Counted" : "Counts at " + at + "%";
    el.append(head, meta, bar, note);
    return el;
  }

  // Resolves with the message to send ("" for the default), or null if cancelled.
  function askToEnd(p, who) {
    return new Promise((resolve) => {
      const dialog = document.createElement("dialog");
      dialog.className = "np-dialog";
      const form = document.createElement("form");
      form.method = "dialog";
      const heading = document.createElement("h2");
      heading.textContent = p.can_stop ? "Stop playback?" : "Remove from Now playing?";
      const what = document.createElement("p");
      what.textContent = p.display + (who ? " · " + who : "");
      form.append(heading, what);

      let input = null;
      if (p.can_stop) {
        const label = document.createElement("label");
        label.textContent = "Message to show in Plex (optional)";
        input = document.createElement("input");
        input.type = "text";
        input.maxLength = 200;
        input.placeholder = "Stopped from BackTrackarr";
        input.autocomplete = "off";
        label.append(input);
        form.append(label);
      } else {
        const note = document.createElement("p");
        note.className = "muted small";
        note.textContent = "BackTrackarr can't stop playback on " + (p.source || "this source") + ".";
        form.append(note);
      }

      const actions = document.createElement("div");
      actions.className = "np-dialog-actions";
      const cancel = document.createElement("button");
      cancel.type = "button";
      cancel.className = "ghost";
      cancel.textContent = "Cancel";
      cancel.addEventListener("click", () => dialog.close("cancel"));
      const go = document.createElement("button");
      go.type = "submit";
      go.value = "end";
      go.textContent = p.can_stop ? "Stop playback" : "Remove";
      actions.append(cancel, go);
      form.append(actions);
      dialog.append(form);
      dialog.addEventListener("close", () => {
        resolve(dialog.returnValue === "end" ? (input ? input.value.trim() : "") : null);
        dialog.remove();
      });
      document.body.append(dialog);
      dialog.showModal();
      if (input) input.focus();
    });
  }

  async function endSession(p, button) {
    const who = [p.user, p.player].filter(Boolean).join(" on ");
    busy = true; // keeps this row in place while the dialog is open
    const message = await askToEnd(p, who);
    if (message === null) {
      busy = false;
      return;
    }
    button.disabled = true;
    button.textContent = "Ending…";
    let problem = "";
    try {
      const res = await fetch("/api/now-playing/end", {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({ key: p.key, message }),
      });
      if (!res.ok && res.status !== 404) {
        const body = await res.json().catch(() => ({}));
        problem = body.error || "Couldn't end this session. Try again.";
      }
    } catch {
      problem = "Couldn't reach BackTrackarr. Try again.";
    }
    busy = false;
    if (!problem) {
      holdUntil = 0; // an earlier failure's message no longer applies
      await refresh();
      return;
    }
    button.disabled = false;
    button.textContent = "End session";
    const row = button.closest(".np");
    let note = row.querySelector(".np-error");
    if (!note) {
      note = document.createElement("div");
      note.className = "err small np-error";
      note.setAttribute("role", "alert");
      row.append(note);
    }
    note.textContent = problem;
    holdUntil = Date.now() + 15000; // leave the reason on screen long enough to read
  }

  async function refresh() {
    if (document.hidden || box.closest("[hidden]") || busy || Date.now() < holdUntil) return;
    try {
      const res = await fetch("/api/now-playing", { cache: "no-store", credentials: "same-origin" });
      if (!res.ok) return;
      const { items } = await res.json();
      if (items.length) {
        list.replaceChildren(...items.map(row));
      } else {
        const none = document.createElement("p");
        none.className = "muted small";
        none.textContent = "Nothing is playing right now.";
        list.replaceChildren(none);
      }
    } catch { /* keep the last view until the app is back */ }
  }

  refresh();
  setInterval(refresh, 10000);
  document.addEventListener("visibilitychange", refresh);
})();
