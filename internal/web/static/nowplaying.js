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

  async function endSession(p, button) {
    const who = [p.user, p.player].filter(Boolean).join(" on ");
    const ask = p.can_stop
      ? "Stop " + p.display + (who ? " for " + who : "") + "?\n\nThis stops playback in Plex right away."
      : "Remove " + p.display + " from Now playing?\n\nBackTrackarr can't stop playback on " + (p.source || "this source") + ".";
    if (!confirm(ask)) return;
    button.disabled = true;
    button.textContent = "Ending…";
    busy = true;
    let problem = "";
    try {
      const res = await fetch("/api/now-playing/end", {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({ key: p.key }),
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
