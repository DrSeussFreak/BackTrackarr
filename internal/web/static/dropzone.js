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
    const empty = "Drop a file here, or click to choose";
    text.textContent = empty;
    const types = document.createElement("span");
    types.className = "muted small";
    types.textContent = (input.getAttribute("accept") || "").replaceAll(",", " ");
    input.parentNode.insertBefore(zone, input);
    zone.append(input, text, types);

    const show = () => {
      const f = input.files && input.files[0];
      text.textContent = f ? "✓ " + f.name : empty;
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
