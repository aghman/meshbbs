// Passkey sign-in and enrolment (webui.md §7, §8).
//
// WebAuthn speaks ArrayBuffers; JSON does not. Every conversion below is that
// one problem: base64url in, ArrayBuffer out for the call, base64url back for
// the reply. Getting a single field wrong produces a browser error that says
// nothing useful, which is why they are handled in one place rather than inline.

const b64urlToBuf = (s) => {
  const pad = s.replace(/-/g, "+").replace(/_/g, "/");
  const raw = atob(pad + "=".repeat((4 - (pad.length % 4)) % 4));
  return Uint8Array.from(raw, (c) => c.charCodeAt(0)).buffer;
};

const bufToB64url = (buf) =>
  btoa(String.fromCharCode(...new Uint8Array(buf)))
    .replace(/\+/g, "-")
    .replace(/\//g, "_")
    .replace(/=+$/, "");

const status = document.getElementById("status");
const say = (text, isError) => {
  status.textContent = text;
  status.hidden = !text;
  status.classList.toggle("error", !!isError);
};

async function post(path, body) {
  const res = await fetch(path, {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: body === undefined ? "{}" : JSON.stringify(body),
  });
  const data = await res.json().catch(() => ({}));
  if (!res.ok) throw new Error(data.error || "that did not work");
  return data;
}

// The server sends WebAuthn options as JSON, so the binary fields arrive as
// base64url strings and have to be turned back into buffers before the browser
// will look at them.
function reviveCreation(o) {
  o.publicKey.challenge = b64urlToBuf(o.publicKey.challenge);
  o.publicKey.user.id = b64urlToBuf(o.publicKey.user.id);
  for (const c of o.publicKey.excludeCredentials || []) c.id = b64urlToBuf(c.id);
  return o.publicKey;
}

function reviveRequest(o) {
  o.publicKey.challenge = b64urlToBuf(o.publicKey.challenge);
  for (const c of o.publicKey.allowCredentials || []) c.id = b64urlToBuf(c.id);
  return o.publicKey;
}

const signedIn = (nick) => {
  document.getElementById("signin").hidden = true;
  document.getElementById("hello").hidden = false;
  document.getElementById("nick").textContent = nick;
  say("");
};

document.getElementById("login").addEventListener("click", async () => {
  try {
    say("Waiting for your passkey…");
    const options = reviveRequest(await post("/auth/login/begin"));
    const assertion = await navigator.credentials.get({ publicKey: options });

    const { nick } = await post("/auth/login/finish", {
      id: assertion.id,
      rawId: bufToB64url(assertion.rawId),
      type: assertion.type,
      response: {
        authenticatorData: bufToB64url(assertion.response.authenticatorData),
        clientDataJSON: bufToB64url(assertion.response.clientDataJSON),
        signature: bufToB64url(assertion.response.signature),
        userHandle: bufToB64url(assertion.response.userHandle),
      },
    });
    signedIn(nick);
  } catch (err) {
    // A user who dismisses the browser prompt has not hit an error, so do not
    // shout at them about one.
    say(err.name === "NotAllowedError" ? "" : err.message, true);
  }
});

document.getElementById("enrol").addEventListener("click", async () => {
  const code = document.getElementById("code").value.trim();
  if (!code) return say("Type the code your SSH session showed you.", true);

  try {
    say("Checking that code…");
    const options = reviveCreation(await post("/auth/enrol/begin", { code }));

    say("Now create the passkey…");
    const cred = await navigator.credentials.create({ publicKey: options });

    await post("/auth/enrol/finish", {
      id: cred.id,
      rawId: bufToB64url(cred.rawId),
      type: cred.type,
      response: {
        attestationObject: bufToB64url(cred.response.attestationObject),
        clientDataJSON: bufToB64url(cred.response.clientDataJSON),
      },
    });

    // Enrolment deliberately does not sign anyone in ([D18]), so the passkey
    // gets used immediately — which also proves it works before the user walks
    // away from the terminal that minted the code.
    document.getElementById("code").value = "";
    say("Passkey added. Signing in with it…");
    document.getElementById("login").click();
  } catch (err) {
    say(err.name === "NotAllowedError" ? "" : err.message, true);
  }
});

document.getElementById("logout").addEventListener("click", async () => {
  await post("/auth/logout").catch(() => {});
  location.reload();
});

// Pick up an existing session on load, so a refresh does not look like a
// sign-out.
fetch("/api/me", { credentials: "same-origin" })
  .then((r) => (r.ok ? r.json() : null))
  .then((me) => me && signedIn(me.nick))
  .catch(() => {});
