// Shows the health of the Plex connection on the Setup page.
(() => {
  const line = document.getElementById("plex-health");
  const button = document.getElementById("plex-check");
  if (!line || !button) return;

  function show(h) {
    line.textContent = h.message || "";
    line.className = "small " + (h.state === "ok" || h.state === "repaired" ? "ok" : "bad");
  }

  async function load(url, options) {
    try {
      const res = await fetch(url, { cache: "no-store", credentials: "same-origin", ...options });
      if (!res.ok) throw new Error();
      show(await res.json());
    } catch {
      show({ state: "offline", message: "Couldn't check Plex. Try again." });
    }
  }

  button.addEventListener("click", async () => {
    button.disabled = true;
    line.className = "small muted";
    line.textContent = "Checking…";
    await load("/setup/plex/check", { method: "POST" });
    button.disabled = false;
  });
  load("/api/plex/health");
})();
