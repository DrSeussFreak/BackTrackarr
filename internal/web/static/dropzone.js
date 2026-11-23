// Turns every file field into a large "drop or choose" area.
(() => {
  const allowed = (input, file) => {
    const accept = (input.getAttribute("accept") || "").toLowerCase().split(",").map((s) => s.trim()).filter(Boolean);
    return !accept.length || accept.some((a) => file.name.toLowerCase().endsWith(a));
  };

  document.querySelectorAll('input[type="file"]').forEach((input) => {
    const zone = document.createElement("label");
    zone.className = "dropzone";
    const text = document.createElement("span");
    const empty = input.multiple ? "Drop files here, or click to choose" : "Drop a file here, or click to choose";
    text.textContent = empty;
    const types = document.createElement("span");
    types.className = "muted small";
    types.textContent = (input.getAttribute("accept") || "").replaceAll(",", " ");
    input.parentNode.insertBefore(zone, input);
    zone.append(input, text, types);

    const show = () => {
      const f = input.files && input.files[0];
      const n = input.files ? input.files.length : 0;
      text.textContent = !f ? empty : n > 1 ? "✓ " + n + " files" : "✓ " + f.name;
      zone.classList.toggle("has-file", !!f);
      zone.classList.remove("bad-file");
    };
    input.addEventListener("change", show);

    ["dragenter", "dragover"].forEach((ev) => zone.addEventListener(ev, (e) => {
      e.preventDefault();
      zone.classList.add("over");
    }));
    ["dragleave", "dragend"].forEach((ev) => zone.addEventListener(ev, () => zone.classList.remove("over")));
    zone.addEventListener("drop", (e) => {
      e.preventDefault();
      zone.classList.remove("over");
      const dropped = [...e.dataTransfer.files];
      if (!dropped.length) return;
      const chosen = input.multiple ? dropped : dropped.slice(0, 1);
      const bad = chosen.find((f) => !allowed(input, f));
      if (bad) {
        text.textContent = bad.name + " isn't a supported file type";
        zone.classList.add("bad-file");
        return;
      }
      const dt = new DataTransfer();
      chosen.forEach((f) => dt.items.add(f));
      input.files = dt.files;
      show();
    });
  });

  // A file dropped beside the zone shouldn't navigate away from the page.
  if (document.querySelector(".dropzone")) {
    ["dragover", "drop"].forEach((ev) => window.addEventListener(ev, (e) => {
      if (!e.target.closest || !e.target.closest(".dropzone")) e.preventDefault();
    }));
  }
})();
