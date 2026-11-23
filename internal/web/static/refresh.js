// Keeps job status current while something runs. The page only reloads when
// that can't lose anything: never with files selected, an upload under way,
// or a field being edited. Otherwise just the Jobs panel updates.
(() => {
  const meta = document.querySelector('meta[name="bt-refresh"]');
  if (!meta) return;
  const every = (Number(meta.content) || 4) * 1000;
  const startForms = new Map();
  const formState = (f) => [...new FormData(f).entries()].filter(([, v]) => typeof v === "string").join("&");
  document.querySelectorAll("form").forEach((f) => startForms.set(f, formState(f)));

  const busy = () => {
    if (window.btUploadBusy && window.btUploadBusy()) return true;
    if (document.querySelector(".dropzone.has-file, .home.editing")) return true;
    const a = document.activeElement;
    if (a && a.matches("input:not([type=file]):not([type=checkbox]):not([type=radio]), textarea, select")) return true;
    return [...startForms].some(([f, s]) => f.isConnected && formState(f) !== s);
  };

  async function tick() {
    if (document.hidden) return;
    if (!busy()) {
      location.reload();
      return;
    }
    const panel = document.getElementById("jobs-panel");
    if (!panel) return;
    try {
      const res = await fetch(location.href, { credentials: "same-origin", cache: "no-store" });
      if (!res.ok || res.redirected) return;
      const next = new DOMParser().parseFromString(await res.text(), "text/html").getElementById("jobs-panel");
      if (next) panel.replaceChildren(...[...next.childNodes].map((n) => document.importNode(n, true)));
    } catch { /* keep the last view until the app is back */ }
  }
  setInterval(tick, every);
})();
