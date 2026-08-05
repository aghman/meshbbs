# MeshBBS — Web UI Design

*A browser front end that is the same BBS, not a second one.*

**Status:** Draft v0.1
**Date:** 2026-08-04
**Amends:** design.md §2 (non-goals), §5.3 (web terminal), §11.5 (config), §13 (Phase 5)
**New decisions:** `[D16]` web UI shape, `[D17]` web authentication
**New open question:** `[N13]` passkey bootstrap for existing accounts

---

## 1. What changed, and why this document exists

design.md §5.3 specifies the web front end as **`xterm.js` over a WebSocket, same session
API** — a pixel-faithful terminal in a browser tab, listed as "low priority" and justified by
"good for casual visitors and for showing the BBS off." §2 separately lists a web forum UI as a
non-goal, softened to "a read-only web view might come later; SSH is the product."

Both stand, but neither describes what is wanted here: a front end that is **recognisably the same
interface as the SSH session, and easier to read and operate than it**. `xterm.js` cannot deliver
the second half by construction. It reproduces the terminal exactly — including an 80×24 grid on a
phone, text selection that fights the application, a help line that names keys a touch device
cannot press, and column truncation computed for a fixed-width cell grid that a browser does not
have. Making the terminal *look* nicer inside `xterm.js` means picking a font. Everything past that
is out of reach.

So §5.3 is reversed. `[D16]`

The non-goal in §2 is **narrowed rather than reversed**: this is still not a web forum UI. There is
no HTML forum with its own navigation, its own page hierarchy, and its own opinions about what a
message base looks like. There is one BBS, one menu graph, one set of screens, one set of keys —
rendered twice.

---

## 2. The shape: a semantic terminal `[D16]`

The Bubble Tea model stays the single source of truth for navigation and state. What changes is
what comes out of it.

Today:

```
Model.View() → switch on m.screen → render*() → lipgloss string → ANSI bytes
```

Proposed:

```
Model.Screen() → Screen (a typed description of what is on screen)
                    ├─→ ansi.Render(Screen, Styles, w, h) → ANSI bytes   (SSH, telnet)
                    └─→ json.Marshal(Screen)              → WebSocket    (browser)
```

`Model.View()` becomes a two-line method that calls `Screen()` and hands the result to the ANSI
renderer. Every existing screen, key binding, status message and piece of wording survives
untouched.

**Why this is the right seam rather than a JSON API over `bbs.Service`.** A JSON API would mean a
second navigation model, a second set of screens, and a second place for every future feature to be
implemented — which is the thing §2 declined, and it drifts the moment someone adds a screen to one
and not the other. Here, a new screen cannot exist in the SSH UI and be missing from the web one:
there is one `Screen()` method, and both renderers consume its output.

### 2.1 The test that keeps it honest

The ANSI renderer must produce **byte-identical output to today's**. `internal/tui` already has
golden screen frames (`go test ./internal/tui/ -update`), so this is not an aspiration — it is a
test that fails loudly if `Screen` cannot express something `views.go` renders today. That
constraint is what stops the description type from quietly becoming a lossy summary of the real UI.

The refactor is therefore mechanical and verifiable: split each `render*` into "build a `Screen`"
and "render a `Screen`", run the golden tests, done. It can land on its own, with no web server
anywhere near it, and no user-visible change.

---

## 3. The block vocabulary

A `Screen` is a title, a status line, a set of key hints, and a list of blocks:

```go
type Screen struct {
    Kind   string      // "menu", "arealist", "mailread", … — lets the web pick a layout
    Title  string      // what frame() renders as the title bar
    Blocks []Block
    Status Status      // text + isErr; the "* " / "! " line
    Help   []KeyHint   // {Key: "enter", Label: "open"} …
}
```

`Help` is the one that pays for itself immediately. Today it is a pre-joined string —
`"up/down move · enter open · q back"` — assembled at each call site. As structured pairs, the ANSI
renderer joins it exactly as now, and the web renders it as a row of real buttons. On a phone that
is the difference between a usable interface and a read-only one.

Eight block kinds cover all fifteen screens. This is not an estimate; it is the result of walking
every `render*` method in `views.go`, `chat.go` and `sysop.go`:

| Block | Carries | Screens using it | Web rendering |
|---|---|---|---|
| `Text` | body, with a level: `body`, `muted`, `heading`, `warning`, `success` | all | `<p>` with a theme class |
| `Choices` | hotkey + label + enabled | menu, signup Y/N steps | clickable rows |
| `Table` | optional header, rows of cells, optional selected index | area list, mail list, who, all four sysop tabs, node identity | `<table>`; reflows to cards under ~40rem |
| `Article` | heading, byline/meta, body | area read, mail read | `<article>`, real scroll |
| `Form` | fields: name, label, value, masked, active | compose, mail compose, unlock, key setup, signup, chat input | real `<input>`/`<textarea>` |
| `ChatLog` | lines: time, nick, text, isSystem | chat | live region, auto-scrolls, appends |
| `Tabs` | names + selected index | sysop | tab bar |
| `Confirm` | question + the affirmative key | sysop grant/revoke/federate | dialog with two buttons |

`Text` absorbing five levels is deliberate: `Heading`, `Note` and `Warning` as separate kinds would
be three types that differ only in which lipgloss style they select, which is exactly the
information a `level` field carries.

---

## 4. The rule that makes this better rather than a reskin

**The `Screen` carries semantics. The renderer owns geometry.**

Today the model does geometry, in at least three places:

- `chat.go` computes `visible := m.height - 10` and slices the log to fit.
- `views.go` calls `truncate(a.Name, 16)` and pads with `%-16s` to build fixed-width columns.
- `frame()` clamps everything to `MaxWidth(m.frameWidth())`.

All three are correct for a character grid and wrong for a browser. If they stay in the model, the
web UI receives a pre-truncated 80-column snapshot and is a screenshot of a terminal, not a better
version of it.

So they move:

| Concern | Model emits | ANSI renderer does | Web renderer does |
|---|---|---|---|
| Long strings | full string + a column width *hint* | truncate to the hint, pad to width | wrap, or ellipsis in CSS |
| Long lists | the whole list + selected index | window to the terminal height | scroll natively, keep selection in view |
| Wrapping | unwrapped paragraphs | wrap at frame width | let the browser wrap at a readable measure |
| Chat backlog | every retained line | tail that fits | scrollback with auto-scroll-to-bottom |

The ANSI side of that table is what `views.go` does today, moved verbatim — so the golden frames
still pass. The web side is where "easier to read" actually comes from: a post body wrapped at a
comfortable measure instead of at column 78, a mail list that scrolls instead of paging, an area
description that is not cut to 26 characters.

The model keeps the selected index either way, because selection is *state* — `q` still goes back,
`up`/`down` still move, and the web and SSH sessions behave identically.

---

## 5. Input: everything is a keystroke

The browser sends keystrokes. Clicking the `[M]` menu row sends `M`. Tapping the `enter open`
button sends `enter`. Clicking the third row of a list sends however many `up`/`down` presses are
needed, then `enter` — or, better, a `select` message carrying an index, which the model applies as
a cursor move.

This is the simplification the whole design rests on: **the model never learns it is talking to a
browser.** No new state machine, no web-only code paths in `Update`, no divergence between what an
SSH user and a web user can do. `handleKey` in `keys.go` is unchanged.

### 5.1 The one exception, stated plainly

Text entry cannot be keystrokes. Sending one WebSocket frame per character is tolerable on a LAN
and unpleasant on a phone, and it is outright broken with autocorrect, predictive text, and IME
composition — all of which produce and revise multi-character runs rather than a keystroke stream.

So there is one non-keystroke message: `{"field": "subject", "value": "…"}`, which sets a field's
whole value. That needs a `SetField(name, value)` on the model, which the existing `textInput` type
supports trivially.

This is the only place the web path diverges from the SSH path, and it is worth naming as a debt:
it is a second way to mutate input state, and any future input widget has to support both. The
alternative — character streaming — trades that debt for a text box that mangles what people type
on the devices they are most likely to use.

---

## 6. Transport

WebSocket, JSON frames, one connection per session.

- **Server → client:** the full `Screen` on every change. These are small — a few hundred bytes to
  a few kilobytes for a long chat backlog — so there is no diffing, no patch protocol, and no
  reconciliation bug class. Bubble Tea already re-renders the whole screen on every update; this
  matches that model exactly.
- **Client → server:** `{"key": "…"}`, `{"field": …, "value": …}`, `{"select": n}`, `{"resize": {…}}`.

A dropped connection ends the session, as a dropped TCP connection ends an SSH one. No
reconnect-and-resume: it would mean holding an unlocked mail session — and the passphrase in memory
(§8) — for a client that may never return.

The initial page load is a small static bundle served from the same binary via `embed`, consistent
with §4's cgo-free, single-artifact packaging. No CDN, no build step in the release path.

---

## 7. Authentication: passkeys `[D17]`

**WebAuthn, discoverable credentials, no password path.**

design.md §5.1 calls account creation "the one place where SSH is genuinely *worse* than telnet for
a BBS." On the web it inverts. Signup is trivial — there is a page, and it can prompt for anything.
What is missing is the key: SSH hands the server a public key before the session exists, which is
why enrolment is free and the user's next login is passwordless. Browsers cannot reach `ssh-agent`,
and pasting a private key into a web page is not a design, it is an incident.

Passkeys are the honest analogue, and they fit the project's existing instincts:

- The credential is a keypair the server never holds the private half of — the same shape as SSH
  key auth and as the node identity in §6.1.
- Nothing secret crosses the wire, so a compromised BBS leaks no reusable credential.
- Phishing resistance is structural: credentials are bound to the origin.
- **Discoverable credentials mean the user does not type a nick.** "Sign in" → OS prompt → in. That
  is a concrete piece of the "easier to use" brief, not just a security property.

Storage mirrors the existing multi-pubkey SSH enrolment, which is a pleasing symmetry — a user
accumulates authenticators, some SSH keys and some passkeys, and any of them logs them in:

```sql
CREATE TABLE webauthn_credentials (
    nick          TEXT NOT NULL REFERENCES users(nick),
    credential_id BLOB NOT NULL PRIMARY KEY,
    public_key    BLOB NOT NULL,
    sign_count    INTEGER NOT NULL,
    created_at    INTEGER NOT NULL,
    last_used_at  INTEGER
);
```

New users land in the **existing signup screens** (`screenSignup`) with `IntentSignup`, minus the
SSH-key step and plus the passkey ceremony. The nick rules, the passphrase choice, and the §6.7
acknowledgement screen — "if you forget this passphrase your messages become permanently
unreadable" — are the same code and the same words.

### 7.1 Two consequences worth stating rather than discovering

**There is no guest browsing on the web.** A passkey-only front door means no unauthenticated view
of public areas. That removes the "showing the BBS off" purpose §5.3 gave the web terminal in the
first place — a visitor with no account sees a sign-in prompt and nothing else. This is a coherent
choice (the SSH `guest` account still exists), but it is a product decision, not a technical
detail, and it should be revisited if the web UI is ever meant to be the front door.

**The RP ID is a deployment sharp edge.** Passkeys are bound to a domain. The sysop must configure
the public origin correctly, and a mismatch does not degrade — it fails totally, with a browser
error that says nothing useful about the cause. `web.origin` is therefore a **required** key with
no default when the web UI is enabled, validated at startup like every other config error (§11.3),
with a message that names the symptom.

---

## 8. `[N13]` — how does an existing user get their first passkey?

This is the one thing the design does not yet answer, and it is not a small gap: **every account
that exists today was created over SSH or the CLI, and none of them can sign in to the web.** A
passkey has to be registered from a browser, by someone already established as the account holder.
There is no way to bootstrap that from passkeys alone.

**Recommended answer:** an authenticated SSH session gains a menu item that displays a short,
single-use, time-boxed code. The web sign-in page accepts that code for **passkey enrolment only** —
it never mints a session, never grants read access, and expires in minutes. It is an enrolment
ceremony, not a credential.

This is worth stating carefully because it is the kind of thing that becomes a backdoor by
accident. The properties that keep it from being one: single use, short expiry, enrolment-only
authority, invalidated by a second issuance, and logged. If it ever grows the ability to log
someone in, it has become a password with worse ergonomics.

The alternative is sysop-issued invitations (`meshbbs user invite bob`), which is more work for the
sysop and better for an instance that wants deliberate control over who reaches the web at all. The
two are compatible; the question is whether the SSH-initiated path ships in v1.

**Recorded as open** rather than decided, because it is a policy call about how open the instance
is, and that is the sysop's temperament rather than an engineering fact.

---

## 9. Sessions, presence, and the passphrase

**Sessions.** A `Secure`, `HttpOnly`, `SameSite=Strict` cookie carrying an opaque identifier for a
server-side session record. Idle timeout and absolute TTL both configurable, both enforced
server-side.

**Presence is shared.** A web session calls `Presence.Join` exactly as SSH and telnet do
(`server.go:259`), so it gets a node number and appears in `[W] Who's online` next to everyone
else, and in the sysop status panel. One BBS, three doors. This falls out for free from reusing
`tui.Config`, and it is the detail that makes the web UI feel like part of the BBS rather than an
adjacent website.

**The mail passphrase behaves exactly as it does over SSH.** Posted over TLS, held in the session's
model memory, cleared on exit — which is what `m.passphrase` already does (`model.go:118`,
`model.go:351`). No new property is lost: an SSH session already delivers the passphrase to this
process, and §8.2's guarantee is that the sysop *stores* ciphertext, not that the passphrase never
reaches the running server.

One thing genuinely gets weaker, and it is worth writing down: **a browser tab closing is a less
reliable "goodbye" than an SSH connection ending.** A killed tab, a phone locking, a laptop
sleeping — none of these necessarily produce a prompt close. The WebSocket close handler clears the
passphrase, but the idle timer is what actually bounds exposure, so it is doing real security work
here rather than being a convenience. It should be short by default, and shorter for an unlocked
mail session than for a session sitting in a forum.

Client-side decryption (compiling `internal/keyring` to WASM so the passphrase never leaves the
browser) is strictly stronger and remains available later — the crypto is Argon2id +
ChaCha20-Poly1305 + X25519 + BLAKE3, all of which compile to WASM as the *same Go code*, with no
reimplementation and therefore no risk of the two drifting. It belongs with the tier-3 client-held
key work already scheduled for Phase 5 (`[N3]`), not with the web UI's first release.

---

## 10. Security notes

**`term.SanitizeForDisplay` is not sufficient for the web, and assuming otherwise is the most
likely way this ships a vulnerability.** It neutralises terminal control sequences (§5.4) — it does
nothing about `<script>`. The web renderer must HTML-escape every piece of user-controlled text —
nicks, subjects, post bodies, chat lines, area descriptions, aliases — and must never assign user
content through `innerHTML`. Structured `Screen` blocks help here: the renderer walks typed fields
and sets `textContent`, so there is no template concatenating strings and no place for markup to
enter. That is a property to preserve deliberately, not a happy accident.

**CSRF.** Cookie-authenticated WebSockets are not protected by the browser's same-origin policy the
way `fetch` is. The upgrade handler must check the `Origin` header against the configured origin
and reject mismatches.

**TLS is required, not recommended.** WebAuthn refuses to operate outside a secure context, so this
is enforced by the platform rather than by policy — but the config should refuse to start a
plaintext listener on a non-loopback bind anyway, so the failure arrives at startup with an
explanation instead of in a browser console.

**Rate limiting.** The existing `auth_attempts_per_ip_per_hour` and `registrations_per_ip_per_day`
(§11.5) apply unchanged. `max_sessions` needs a web equivalent for the same reason telnet has one
(`telnet.go:133`): a public listener with no cap is a file-descriptor exhaustion away from taking
SSH down with it.

**Determinism.** `internal/tui` and `internal/sshd` are *not* exempt from the checker's clock rule —
`tools/checkdeterminism/main.go:32` exempts only `internal/clock`, `internal/rng`, `tools/` and
`cmd/`. A new `internal/webd` inherits that: it takes an injected `clock.Clock` like every other
package, and uses `crypto/rand` (not `math/rand`) for session identifiers and enrolment codes, which
the checker permits and security requires independently.

---

## 11. Look and feel

**Modernised retro.** Clearly terminal-descended, deliberately more readable than a terminal.

- **Monospace throughout, sized like prose.** The single highest-leverage change: the terminal look
  survives, while 1.6 line-height, a ~72ch measure, and a comfortable font size fix most of what
  makes an 80×24 screen tiring. The problem with reading a terminal in a browser was never the
  typeface.
- **Colours come from the existing theme TOML.** The eight `Theme` fields (`Primary`, `Secondary`,
  `Accent`, `Muted`, `Danger`, `Success`, `Text`, `Highlight`) map directly to CSS custom
  properties, so `themes/*.toml` retheming (`[N5]`) works on the web with no second mechanism and
  no second file format. ANSI indices 0–15 need one fixed palette mapping; `#rrggbb` values pass
  through.
- **`Border` is honoured as a CSS border style** — `double`, `single`, `ascii` → 3px double, 1px
  solid, 1px dashed. Small touch, keeps the four built-in themes visually distinct.
- **Selection is a real focus ring**, not a `>` prefix — though the `>` can stay for texture.
- **Key hints become buttons** that are still labelled with their key, so the web teaches the SSH
  interface rather than replacing it. Someone who learns `p post` in the browser knows it over SSH.
- **Mobile:** key hints become a fixed bottom bar; `Table` reflows to cards; `Form` uses native
  inputs with correct `inputmode` and `autocomplete` attributes.

What is deliberately *not* here: CRT glow, scanlines, and phosphor burn. They are charming and they
are the opposite of the brief — the request was for something nicer to look at and easier to
understand, and a simulated 1985 monitor is the single most effective way to make text harder to
read.

---

## 12. Configuration

Extends §11.5's "Listeners" group. Replaces the placeholder `Web terminal: enabled (default false),
Phase 5`.

**Web — Phase 5**
`enabled` (default **false**), `bind`, `port`, `origin` (**required when enabled**, no default —
the WebAuthn RP ID, §7.1), `tls_cert` / `tls_key`, `max_sessions`, `max_sessions_per_user`,
`idle_timeout`, `unlocked_idle_timeout` (§9), `session_ttl`, `default_theme` (falls back to the
global one).

Off by default, like telnet — but without telnet's plaintext warning, since TLS is mandatory rather
than optional here.

---

## 13. Build order

Four steps, each independently useful, each shippable without the next.

**1 — Screen extraction.** Split `render*` into description + ANSI renderer. Move geometry out of
the model (§4). No web code, no new dependency, no user-visible change; golden frames prove it.
*This is the whole structural risk of the project, and it lands first and alone.*

**2 — Auth and the HTTP server.** `internal/webd`, TLS, the static bundle, WebAuthn registration
and login, the credentials table, `[N13]`'s bootstrap path. Verifiable with no UI beyond a page
that says who you are.

**3 — The session bridge.** WebSocket, the JSON renderer, `tui.Config` construction mirroring
`telnet.go:191`, presence integration. At the end of this step the web UI works and is plain.

**4 — Look, and touch.** Theme → CSS, typography, the clickable key bar, mobile reflow. This is
where the brief is actually satisfied, and it is last because it is the only step that benefits
from having the real thing to look at.

Steps 2 and 3 are where an `xterm.js` stopgap would have paid off — it exercises the same TLS,
auth, session lifecycle and WebSocket plumbing. If step 1 slips, shipping `xterm.js` on top of
steps 2–3 remains a valid intermediate release, and step 1 replaces the renderer underneath it
without touching either.

---

## 14. What this costs, and the risk to watch

`views.go`, `chat.go` and `sysop.go` are about 1,000 lines that currently work, and step 1 rewrites
their structure while preserving their output exactly. The golden frames make that safe in the
sense that breakage is *detected* — they do not make it free.

**The risk that outlives the project is renderer drift**: two renderers, one of which somebody
forgets. Three things hold it off, and they are worth defending in review:

1. `Screen()` is the *only* way to render. `View()` goes through it. There is no path that builds
   ANSI directly, so a screen cannot exist for SSH and be missing on the web.
2. The block vocabulary is small and closed (§3). Adding a ninth block kind should feel like a
   decision, because a screen that needs its own bespoke block is usually a screen that wants
   rethinking.
3. Geometry lives in renderers and semantics lives in the model (§4). The moment a `width` or a
   `height` appears in a `Screen` field, the abstraction has started leaking and the web UI has
   started becoming a terminal emulator again.

---

## 15. Decisions

| # | Question | Decision | Sections affected |
|---|---|---|---|
| **D16** | What shape is the web UI? | **Semantic terminal, reversing §5.3's `xterm.js`.** The Bubble Tea model emits a typed `Screen` description; ANSI and HTML renderers both consume it. Same screens, same keys, same wording, rendered twice. §2's "no web forum UI" non-goal is **narrowed, not reversed** — there is no second navigation model and no second UI to maintain. Geometry (truncation, windowing, wrapping) moves from the model into the renderers, which is what makes the web version more readable rather than a screenshot of a terminal. | design.md §2, §5.3, §13; this doc §2–§5 |
| **D17** | How do people authenticate on the web? | **Passkeys / WebAuthn only.** No password path, no SSH-key path, no guest browsing. Discoverable credentials so no nick is typed. The credential is a keypair the server never holds — the same shape as SSH key auth and §6.1's node identity. Two accepted consequences, both stated rather than discovered: the web has **no unauthenticated front door**, removing §5.3's "showing the BBS off" rationale; and `web.origin` becomes a **required** config key, because an RP ID mismatch fails totally rather than degrading. | design.md §5.1, §11.5; this doc §7 |

## 16. Open questions

**`N13` — how does an existing account get its first passkey?** Every account today predates the
web and cannot sign in to it, and passkey enrolment requires a browser plus an already-established
account holder. Recommended: a single-use, short-expiry, **enrolment-only** code issued from an
authenticated SSH session — never a session-minting credential. Alternative: sysop-issued
invitations, which suit an instance wanting deliberate control over who reaches the web at all. The
two are compatible. Open because it is a policy call about how open the instance is, not an
engineering fact. (§8)
