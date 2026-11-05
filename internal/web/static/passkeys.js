// Passkeys (WebAuthn) for BackTrackarr: converts between the server's JSON
// (base64url strings) and the ArrayBuffers the browser API uses.
(function () {
  "use strict";
  const toBuf = (s) => {
    s = s.replace(/-/g, "+").replace(/_/g, "/");
    while (s.length % 4) s += "=";
    return Uint8Array.from(atob(s), (c) => c.charCodeAt(0)).buffer;
  };
  const toB64 = (buf) => {
    if (!buf) return null;
    let s = "";
    new Uint8Array(buf).forEach((b) => (s += String.fromCharCode(b)));
    return btoa(s).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
  };

  async function post(url, body) {
    const r = await fetch(url, {
      method: "POST", credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: body ? JSON.stringify(body) : "{}",
    });
    let j = {};
    try { j = await r.json(); } catch (e) { /* not JSON */ }
    if (!r.ok) throw new Error(j.error || r.statusText);
    return j;
  }

  function show(el, msg) {
    if (!el) { alert(msg); return; }
    el.textContent = msg;
    el.hidden = !msg;
  }

  function supported(errEl) {
    if (!window.PublicKeyCredential || !navigator.credentials) {
      show(errEl, "This browser doesn't support passkeys.");
      return false;
    }
    return true;
  }

  // Sign in with a passkey (as the only step, or as the second step).
  window.btPasskeyLogin = async function (next, errEl) {
    if (!supported(errEl)) return;
    show(errEl, "");
    try {
      const opts = await post("/login/passkey/begin");
      const pk = opts.publicKey;
      pk.challenge = toBuf(pk.challenge);
      (pk.allowCredentials || []).forEach((c) => (c.id = toBuf(c.id)));
      const cred = await navigator.credentials.get({ publicKey: pk });
      const res = await post("/login/passkey/finish?next=" + encodeURIComponent(next || "/"), {
        id: cred.id, rawId: toB64(cred.rawId), type: cred.type,
        response: {
          clientDataJSON: toB64(cred.response.clientDataJSON),
          authenticatorData: toB64(cred.response.authenticatorData),
          signature: toB64(cred.response.signature),
          userHandle: toB64(cred.response.userHandle),
        },
        clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
      });
      location.href = res.next || "/";
    } catch (e) {
      show(errEl, e.name === "NotAllowedError" ? "Cancelled or timed out." : e.message);
    }
  };

  // Add a passkey (Security page).
  window.btPasskeyRegister = async function (nameInput, errEl) {
    if (!supported(errEl)) return;
    show(errEl, "");
    try {
      const opts = await post("/security/passkeys/begin");
      const pk = opts.publicKey;
      pk.challenge = toBuf(pk.challenge);
      pk.user.id = toBuf(pk.user.id);
      (pk.excludeCredentials || []).forEach((c) => (c.id = toBuf(c.id)));
      const cred = await navigator.credentials.create({ publicKey: pk });
      const name = (nameInput && nameInput.value) || "";
      const res = await post("/security/passkeys/finish?name=" + encodeURIComponent(name), {
        id: cred.id, rawId: toB64(cred.rawId), type: cred.type,
        response: {
          clientDataJSON: toB64(cred.response.clientDataJSON),
          attestationObject: toB64(cred.response.attestationObject),
          transports: cred.response.getTransports ? cred.response.getTransports() : [],
        },
        clientExtensionResults: cred.getClientExtensionResults ? cred.getClientExtensionResults() : {},
      });
      if (res.recovery_codes && res.recovery_codes.length) {
        const box = document.getElementById("new-codes");
        const list = document.getElementById("new-codes-list");
        if (box && list) {
          list.innerHTML = "";
          res.recovery_codes.forEach((c) => {
            const li = document.createElement("li");
            li.textContent = c;
            list.appendChild(li);
          });
          box.hidden = false;
          box.scrollIntoView();
          return; // stay on the page so the codes can be saved
        }
      }
      location.reload();
    } catch (e) {
      show(errEl, e.name === "InvalidStateError" ? "This device already has a passkey here."
        : e.name === "NotAllowedError" ? "Cancelled or timed out." : e.message);
    }
  };
})();
