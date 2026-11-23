// Turns every file field into a large "drop or choose" area. Fields marked
// multiple take any number of files and whole folders, and send them in small
// pieces so a large batch can't hit a request or proxy limit.
(() => {
  const CHUNK = 8 * 1024 * 1024;
  let working = 0; // drops being read and uploads under way
  let pickerUntil = 0; // a file dialog is open, so the page must not reload
  window.btUploadBusy = () => working > 0 || Date.now() < pickerUntil;
  const watchPicker = (el) => {
    el.addEventListener("click", () => { pickerUntil = Date.now() + 10 * 60 * 1000; });
    ["change", "cancel"].forEach((ev) => el.addEventListener(ev, () => { pickerUntil = 0; }));
  };
  const WORKERS = 3;

  const allowed = (input, file) => {
    const accept = (input.getAttribute("accept") || "").toLowerCase().split(",").map((s) => s.trim()).filter(Boolean);
    return !accept.length || accept.some((a) => file.name.toLowerCase().endsWith(a));
  };

  const size = (n) => {
    if (n < 1024 * 1024) return Math.max(1, Math.round(n / 1024)) + " KB";
    if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + " MB";
    return (n / 1024 / 1024 / 1024).toFixed(2) + " GB";
  };

  const entryFile = (entry) => new Promise((resolve) => entry.file(resolve, () => resolve(null)));
  const entryList = (reader) => new Promise((resolve) => reader.readEntries(resolve, () => resolve([])));

  // Folders are read recursively. Browsers return long folders in batches.
  async function walk(entry, out) {
    if (entry.name.startsWith(".")) return;
    if (entry.isFile) {
      const f = await entryFile(entry);
      if (f) out.push(f);
    } else if (entry.isDirectory) {
      const reader = entry.createReader();
      for (;;) {
        const list = await entryList(reader);
        if (!list.length) break;
        for (const e of list) await walk(e, out);
      }
    }
  }

  async function droppedFiles(dt) {
    const entries = [...(dt.items || [])].map((i) => (i.webkitGetAsEntry ? i.webkitGetAsEntry() : null));
    if (!entries.length || entries.some((e) => !e)) return [...dt.files];
    const out = [];
    for (const e of entries) await walk(e, out);
    return out;
  }

  const newBatch = () => [...crypto.getRandomValues(new Uint8Array(16))].map((b) => b.toString(16).padStart(2, "0")).join("");

  async function sendPiece(batch, index, file, offset) {
    const end = Math.min(offset + CHUNK, file.size);
    const q = new URLSearchParams({ batch, index, name: file.name, offset, total: file.size });
    for (let attempt = 0; ; attempt++) {
      let res = null;
      try {
        res = await fetch("/upload/chunk?" + q, { method: "POST", credentials: "same-origin", body: file.slice(offset, end) });
        if (res.ok) return end - offset;
      } catch { /* retried below */ }
      if (res && res.status < 500 && res.status !== 408 && res.status !== 429) {
        if (res.status === 401 || res.redirected) throw new Error("You were signed out. Sign in and try again.");
        throw new Error((await res.text()).trim() || "Upload failed.");
      }
      if (attempt >= 4) throw new Error("Couldn't upload " + file.name + ". Check your connection and try again.");
      await new Promise((r) => setTimeout(r, 1000 * (attempt + 1)));
    }
  }

  async function sendAll(files, progress) {
    const batch = newBatch();
    const total = files.reduce((n, f) => n + f.size, 0);
    let next = 0, done = 0, sent = 0, failed = null;
    const work = async () => {
      while (!failed && next < files.length) {
        const index = next++;
        const f = files[index];
        try {
          let offset = 0;
          do {
            const n = await sendPiece(batch, index, f, offset);
            offset += n;
            sent += n;
            progress(done, files.length, sent, total);
          } while (offset < f.size && !failed);
          done++;
          progress(done, files.length, sent, total);
        } catch (e) {
          failed = failed || e;
        }
      }
    };
    await Promise.all(Array.from({ length: Math.min(WORKERS, files.length) }, work));
    if (failed) throw failed;
    return batch;
  }

  document.querySelectorAll('input[type="file"]').forEach((input) => {
    const many = input.multiple;
    const needed = input.required;
    const zone = document.createElement("label");
    zone.className = "dropzone";
    const text = document.createElement("span");
    const empty = many ? "Drop files or folders here, or click to choose files" : "Drop a file here, or click to choose";
    text.textContent = empty;
    const types = document.createElement("span");
    types.className = "muted small";
    const hint = (input.getAttribute("accept") || "").replaceAll(",", " ");
    types.textContent = hint;
    input.parentNode.insertBefore(zone, input);
    zone.append(input, text, types);
    let picked = [];
    let clear = null;
    watchPicker(input);

    const show = (skipped = 0) => {
      const n = picked.length;
      text.textContent = !n ? empty : n > 1 ? "✓ " + n.toLocaleString() + " files" : "✓ " + picked[0].name;
      types.textContent = n ? size(picked.reduce((t, f) => t + f.size, 0)) + (skipped ? ", skipped " + skipped + " of another type" : "") : hint;
      zone.classList.toggle("has-file", n > 0);
      zone.classList.remove("bad-file");
      if (many) input.required = needed && !n;
      if (clear) clear.hidden = !n;
    };

    if (many) {
      clear = document.createElement("button");
      clear.type = "button";
      clear.className = "ghost dropzone-clear";
      clear.textContent = "Clear";
      clear.hidden = true;
      clear.addEventListener("click", (e) => {
        e.preventDefault();
        e.stopPropagation();
        picked = [];
        input.value = "";
        show();
      });
      zone.append(clear);

      // A normal file dialog can't pick a folder on macOS.
      const folder = document.createElement("input");
      folder.type = "file";
      folder.webkitdirectory = true;
      folder.hidden = true;
      folder.setAttribute("aria-label", "Choose a folder");
      const choose = document.createElement("button");
      choose.type = "button";
      choose.className = "ghost dropzone-folder";
      choose.textContent = "Choose folder";
      choose.addEventListener("click", (e) => {
        e.preventDefault();
        e.stopPropagation();
        folder.click();
      });
      watchPicker(folder);
      folder.addEventListener("click", (e) => e.stopPropagation());
      folder.addEventListener("change", () => {
        add([...folder.files].filter((f) => !f.name.startsWith(".")));
        folder.value = "";
      });
      zone.append(choose, folder);
    }

    const add = (files) => {
      const seen = new Set(picked.map((f) => [f.name, f.size, f.lastModified].join("|")));
      let skipped = 0;
      for (const f of files) {
        if (!allowed(input, f)) { skipped++; continue; }
        const k = [f.name, f.size, f.lastModified].join("|");
        if (!seen.has(k)) { seen.add(k); picked.push(f); }
      }
      if (!picked.length && skipped) {
        text.textContent = "None of those files are a supported type";
        types.textContent = hint;
        zone.classList.add("bad-file");
        return;
      }
      show(skipped);
    };

    input.addEventListener("change", () => {
      if (many) {
        add([...input.files]);
        input.value = "";
      } else {
        picked = input.files[0] ? [input.files[0]] : [];
        show();
      }
    });

    ["dragenter", "dragover"].forEach((ev) => zone.addEventListener(ev, (e) => {
      e.preventDefault();
      zone.classList.add("over");
    }));
    ["dragleave", "dragend"].forEach((ev) => zone.addEventListener(ev, () => zone.classList.remove("over")));
    zone.addEventListener("drop", async (e) => {
      e.preventDefault();
      zone.classList.remove("over");
      if (!many) {
        const f = e.dataTransfer.files[0];
        if (!f) return;
        if (!allowed(input, f)) {
          text.textContent = f.name + " isn't a supported file type";
          zone.classList.add("bad-file");
          return;
        }
        const dt = new DataTransfer();
        dt.items.add(f);
        input.files = dt.files;
        picked = [f];
        show();
        return;
      }
      text.textContent = "Reading files…";
      working++;
      try {
        add(await droppedFiles(e.dataTransfer));
      } finally {
        working--;
      }
    });

    if (!many || !input.form) return;
    const form = input.form;
    form.addEventListener("submit", async (e) => {
      if (!picked.length) return;
      e.preventDefault();
      const buttons = [...form.querySelectorAll("button")];
      buttons.forEach((b) => { b.disabled = true; });
      const leaving = (ev) => { ev.preventDefault(); ev.returnValue = ""; };
      window.addEventListener("beforeunload", leaving);
      working++;
      try {
        const batch = await sendAll(picked, (d, n, sent, total) => {
          text.textContent = "Uploading " + d.toLocaleString() + " of " + n.toLocaleString() + " files";
          types.textContent = size(sent) + " of " + size(total) + ". Keep this page open.";
        });
        text.textContent = "Starting import…";
        const field = (name, value) => {
          const h = document.createElement("input");
          h.type = "hidden";
          h.name = name;
          h.value = value;
          form.append(h);
        };
        field("upload_batch", batch);
        field("upload_count", picked.length);
        input.required = false;
        input.disabled = true;
        window.removeEventListener("beforeunload", leaving);
        form.submit();
      } catch (err) {
        working--;
        window.removeEventListener("beforeunload", leaving);
        buttons.forEach((b) => { b.disabled = false; });
        text.textContent = err.message || "Upload failed.";
        types.textContent = "Your files are still selected. Press the button to try again.";
        zone.classList.add("bad-file");
      }
    });
  });

  // A file dropped beside the zone shouldn't navigate away from the page.
  if (document.querySelector(".dropzone")) {
    ["dragover", "drop"].forEach((ev) => window.addEventListener(ev, (e) => {
      if (!e.target.closest || !e.target.closest(".dropzone")) e.preventDefault();
    }));
  }
})();
