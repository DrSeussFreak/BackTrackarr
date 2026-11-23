// Connections page: test one app or all of them, with live results.
(() => {
  const rows = [...document.querySelectorAll(".conn")];
  const all = document.getElementById("conn-all");
  const summary = document.getElementById("conn-summary");
  if (!all) return;

  function show(row, r) {
    row.dataset.status = r.status;
    row.querySelector(".conn-head").textContent = r.headline;
    row.querySelector(".conn-ms").textContent = r.ms ? r.ms + " ms" : "";
    const list = row.querySelector(".conn-details");
    list.replaceChildren(...(r.details || []).map((d) => {
      const li = document.createElement("li");
      li.textContent = d;
      return li;
    }));
    const fix = row.querySelector(".conn-fix");
    fix.replaceChildren();
    if (r.fix) {
      const b = document.createElement("b");
      b.textContent = "How to fix: ";
      fix.append(b, r.fix);
    }
    row.querySelector(".conn-when").textContent = r.at ? "Last tested just now" : "";
  }

  async function test(row) {
    const button = row.querySelector(".conn-test");
    button.disabled = true;
    row.dataset.status = "testing";
    row.querySelector(".conn-head").textContent = "Testing…";
    try {
      const res = await fetch("/connections/test", { method: "POST", credentials: "same-origin",
        body: new URLSearchParams({ id: row.dataset.id }) });
      if (!res.ok || res.redirected) throw new Error();
      show(row, await res.json());
    } catch {
      show(row, { status: "fail", headline: "Couldn't run the test", fix: "Reload this page and sign in if asked." });
    }
    button.disabled = false;
  }

  function tally() {
    const n = (s) => rows.filter((r) => r.dataset.status === s).length;
    const parts = [n("ok") + " working"];
    if (n("warn")) parts.push(n("warn") + " need attention");
    if (n("fail")) parts.push(n("fail") + " failing");
    summary.textContent = parts.join(", ");
  }

  rows.forEach((row) => row.querySelector(".conn-test").addEventListener("click", async () => {
    await test(row);
    tally();
  }));

  all.addEventListener("click", async () => {
    const queue = rows.filter((r) => r.dataset.status !== "off");
    all.disabled = true;
    let done = 0;
    summary.textContent = "Testing 0 of " + queue.length + "…";
    const total = queue.length;
    const work = async () => {
      while (queue.length) {
        await test(queue.shift());
        summary.textContent = "Testing " + ++done + " of " + total + "…";
      }
    };
    await Promise.all([work(), work(), work()]);
    all.disabled = false;
    tally();
    const problem = rows.find((r) => r.dataset.status === "fail" || r.dataset.status === "warn");
    if (problem) problem.scrollIntoView({ behavior: "smooth", block: "center" });
  });
  if (rows.some((r) => ["ok", "warn", "fail"].includes(r.dataset.status))) tally();
})();
