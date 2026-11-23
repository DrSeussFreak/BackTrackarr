// Keeps the dashboard's Now Playing panel current.
(() => {
  const box = document.getElementById("now-playing");
  if (!box) return;
  const list = box.querySelector("[data-list]");

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
    if (!confirm("Remove " + p.display + " from Now playing?\n\nThis doesn't stop playback in Plex.")) return;
    button.disabled = true;
    button.textContent = "Ending…";
    try {
      const res = await fetch("/api/now-playing/end", {
        method: "POST", credentials: "same-origin",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body: new URLSearchParams({ key: p.key }),
      });
      if (!res.ok && res.status !== 404) throw new Error();
      await refresh();
    } catch {
      button.disabled = false;
      button.textContent = "Couldn't end. Try again";
    }
  }

  async function refresh() {
    if (document.hidden || box.closest("[hidden]")) return;
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
