# MeshBBS — High-Level Design

*A modern, cross-platform BBS in Go with SSH and browser access, door games, file areas, forums, and DMs — federated between independent BBS instances over Meshtastic LoRa.*

**Status:** Draft v0.16 — the browser front end
**Date:** 2026-08-06
**All 15 open questions from v0.1 are answered. Decisions are recorded in §15 and referenced inline as `[D#]`. New questions raised *by* those decisions are in §14.**
**The browser front end has its own document: [webui.md](webui.md), which owns §5.3 in detail and records `[D16]`–`[D18]`.**

*v0.3 added account creation and registration (§5.1, §6.7) and configuration and administration (§11).*
*v0.16 ships the browser front end and **reverses §5.3's `xterm.js`** `[D16]`. The model no longer renders ANSI directly: it emits a typed `Screen` description that an ANSI renderer and an HTML renderer both consume, so there is one menu graph rather than two and a screen cannot exist over SSH and be missing from the web. Geometry — truncation, windowing, wrapping — moves out of the model and into the renderers, which is what makes the browser version more readable rather than a screenshot of a terminal; the golden frames prove the ANSI output is byte-identical. Authentication is passkeys only `[D17]`, bootstrapped for pre-existing accounts by a single-use enrolment code minted from an authenticated SSH session `[D18]`. Two consequences are recorded rather than discovered: **there is no guest browsing on the web**, which removes the "showing the BBS off" rationale §5.3 originally gave, and **`web.origin` is a required key**, because an RP ID mismatch fails every sign-in totally instead of degrading. §2's "no web forum UI" non-goal is **narrowed, not withdrawn**. The front end therefore lands ahead of the Phase 5 slot §13 gave it.*
*v0.15 implements §7.6 and corrects its central justification: bytes are a bad proxy for airtime not because airtime is superlinear in payload but because it is affine, which under-prices SMALL packets by an order of magnitude. Cost is charged per packet. Also records the refusal reasons a status screen needs to tell "over budget" from "the channel is busy", both of which look like silence.*
*v0.14 records the first two-instance run over real LoRa (§7.1.2.1): discovery, broadcast and attribution all work, and two things hardware taught that no amount of simulation would. Packet IDs are part of the wire contract — Meshtastic drops duplicate `(sender, packet_id)` pairs, so a deterministically seeded node goes silent after a restart. And unicast is PKC-encrypted by firmware 2.5+, so direct packets to a peer whose Meshtastic key we have not exchanged are acknowledged by the firmware and never seen by the application; broadcast, which uses the channel PSK, is unaffected.*
*v0.13 fills a gap this document had: nothing said how an 8-byte node ID maps to a 4-byte Meshtastic radio address (§7.1.2, `[N12]`). Resolved by a signed, self-certifying `ANNOUNCE` that binds the two, with the radio number inside the signature so a captured announcement cannot be replayed from another radio, and demand-driven `WHO_IS` discovery rather than announcing often enough to be heard — which would cost about 1% of the whole channel at fifty instances. No packet carries identity bytes.*
*v0.12 opens Phase 3 with the local wire (§7.1.1): the config exchange that must complete before a node will transmit, the resynchronising framer a shared-with-debug-output UART requires, and the correction that **USB VID/PID scanning cannot work on macOS** without the cgo dependency §4 forbids — detection ranks and explains candidate ports instead of picking one.*
*v0.11 completes Phase 2: `SUCCESSION` with all four `[N2]` guardrails (§6.1.6), lazy `PROFILE` publication (§6.7), and the IP link — **corrected from "QUIC/Noise", which is not implementable as written**, to TCP with mutually-authenticated TLS 1.3 over self-signed Ed25519 certificates pinned to node IDs (§7.9). Same no-PKI property, no third-party dependency, and no extra key material to bind to the node identity.*
*v0.10 adds the §12.3 invariant suite — monotonicity, immutability, vector honesty, signature integrity and a rolling-window airtime budget, asserted after EVERY simulated event rather than at the end of a run — and records the capacity number it produced: about six records per hour federation-wide on LongFast at R=4, against the 5% budget (§7.3).*
*v0.9 records what building the anti-entropy engine (§7.3) and simulating fifty nodes established: `bundle_id` must be content-derived or an interrupted transmission livelocks under the airtime governor; ε is flat in K only for K ≥ 5 and K=1 is exact; the 50-node digest interval is ~5 hours, not 2–3; reply suppression is needed on the request path, not just the digest path; and control-message limits must derive from the MTU in bytes. Measured result at fifty nodes: convergence in 3h20m at 2.3% of the channel, with 95% of digest beats suppressed.*
*v0.8 corrects two §7.2 claims that implementation disproved: the fountain degree distribution (uniform-half, not "heavy on degree 2–3" — that intuition belongs to belief propagation, and we decode by Gaussian elimination), and the repair count (`ceil(αK)+1` decoded for only 5 of 12 receivers at K=10 and 20% loss; replaced by a fitted formula with ε = 3.4, z = 1.8). Both corrections come from the Phase 2 simulator harness (§12.1) doing its job.*
*v0.7 closes `N11`: door API capability levels 1–3 always available, `act_as_user` a per-door sysop grant with capability intersection (§9.1.1); `DOOR_EVENT` stays node-signed. No open questions remain except `N10`, which resolves by measurement.*
*v0.6 adds the testing strategy (§12) and, from auditing for it, three Phase-0 encoding constraints that silently break signatures (§6.2.1). Raises `N11` (door API authority). `N4` closed; `N10` open pending measurement.*
*v0.5 resolves `N1`–`N9` (§15.2): sysop-owned aliases resolving in addressing, `SUCCESSION` with auto-follow, standalone key helper, dual base32/word ID rendering, a theme loader, an R measurement method and `mesh-survey` (§7.8), open-plus-gated registration, no email, listed-by-default. Adopts `spf13/cobra` for all CLI. One question remains open (`N10`: the actual value of R).*
*v0.4 **reverses `[D9]`**: node addressing moves from FidoNet-style numeric `zone:net/node` back to self-certifying key-derived IDs with a local petname layer (§6.1). Knock-on changes in §6.2, §6.4, §7.3, §7.7, §8.4, §10, §11, Appendix B; open questions `N1`/`N2`/`N4` re-scoped in §13.*

---

## 1. The one thing that shapes everything else

Before any architecture: the mesh link is *brutally* small. This single fact should drive every other decision, so it goes first.

A Meshtastic packet carries **at most 233 bytes of application payload** (`Data.payload` inside a `MeshPacket`; the full on-air frame is 256 bytes). On the default `LongFast` preset, one full 256-byte packet occupies the channel for **2.16 seconds**.

Airtime below is computed from the Semtech LoRa formula and validated against Meshtastic's published numbers — a 16-byte LongFast packet comes out to exactly 354 ms, matching their docs, and a full packet to 2.16 s vs. their stated "~2 s". So these numbers are trustworthy:

| Preset | Airtime, full 256B packet | Max goodput | Payload/day @ 5% airtime | ≈ forum posts/day* |
|---|---:|---:|---:|---:|
| Short Turbo | 0.102 s | 2285 B/s | 9.9 MB | 33,000 |
| Short Fast | 0.204 s | 1143 B/s | 4.9 MB | 16,500 |
| Short Slow | 0.362 s | 644 B/s | 2.8 MB | 9,300 |
| Medium Fast | 0.652 s | 358 B/s | 1.5 MB | 5,100 |
| Medium Slow | 1.181 s | 197 B/s | 853 KB | 2,800 |
| Long Turbo | 1.656 s | 141 B/s | 608 KB | 2,000 |
| **Long Fast (default)** | **2.157 s** | **108 B/s** | **467 KB** | **1,550** |
| Long Moderate | 7.934 s | 29 B/s | 127 KB | 420 |

\* assuming a ~900-character post compressing to ~300 bytes with a trained dictionary.

### 1.1 The correction that matters: flood multiplication

**The table above is the channel's capacity, not ours.** Meshtastic uses managed flood routing: every node that receives a broadcast rebroadcasts it (with SNR-based delay and duplicate suppression). So one origination costs the *channel* roughly **R × the airtime of a single transmission**, where R is the number of nodes that actually rebroadcast. On a well-connected mesh R is typically **3–5**; suppression keeps it well below the node count, but it is emphatically not 1.

Every airtime number in this document is therefore stated two ways: *local TX airtime* (what our node spends) and *mesh airtime* (what the commons pays, ≈ R ×). The governor budgets the second. `[D2]`

With the network sized at up to ~50 instances `[D2]`, the honest per-node budget is small:

| Instances | R | Local TX/day | Full packets/day | Raw payload/day |
|---:|---:|---:|---:|---:|
| 5 | 4 | 216 s | 108 | 25 KB |
| 20 | 4 | 54 s | 27 | 6.3 KB |
| **50** | **4** | **21.6 s** | **10.8** | **2.5 KB** |

(LongFast, mesh-wide ceiling 5% of wall clock, shared evenly.)

**Three conclusions fall straight out of this:**

1. **Forums and DMs over mesh are viable, but tight — not comfortable.** At 50 instances sharing a 5% mesh ceiling, each node originates ~10 full packets/day ≈ 2.5 KB raw, ≈ 8.8 KB of text after dictionary compression, ≈ 25 short posts/day per node, ~1,200/day network-wide. Adequate for a hobby network; not adequate for anything careless. **Batching (§7.3) and dictionary compression (§7.4) are load-bearing requirements, not optimizations.** Fixed per-packet and per-record overhead is the enemy.
2. **File transfer over mesh is not viable and is not implemented at all.** A single 1 MB file at 5% mesh LongFast takes over a week of channel time. Mesh replicates *file catalogs* only; bytes move over IP or sneakernet. This is a hard block, not a discouraged option. `[D8]`
3. **Every byte on the wire must be justified.** Binary encoding, truncated hashes, derived (not transmitted) record IDs, per-bundle origin tables, a pre-shared compression dictionary, and batching — not JSON, not UUIDs, not per-post packets.

There is also a social constraint that matters as much as the technical one: **a shared Meshtastic channel is a commons.** If a BBS network parks itself on a community mesh and eats 30% of airtime, it will be (rightly) unwelcome. The airtime governor (§7.6) is a first-class feature, not a nice-to-have.

---

## 2. Goals and non-goals

### Goals

- Single self-contained Go binary, **`CGO_ENABLED=0` on every target** `[D13]`; runs on Linux, macOS, Windows (amd64 + arm64)
- SSH as the primary access method, with a modern TUI that still feels like a BBS
- Forums (message bases), direct messages, file areas, door games, multi-node presence
- Federation of forums and DMs between independent BBS instances
- Meshtastic as a *first-class but not exclusive* federation transport
- Off-grid operation: a BBS with no internet at all is a full participant
- **Self-certifying key-derived node IDs** with a local petname layer `[D9]` (§6.1), and an **FTN gateway** bridging echomail/netmail to existing FidoNet-style networks `[D14]` (§7.7)
- Sysop-friendly: one config file, sane defaults, no external database or runtime

### Non-goals

- **File transfer over mesh in any form.** Catalogs only, hard-blocked in code (§7.5). `[D8]`
- **Bluetooth LE transport.** USB serial and TCP cover every realistic deployment, and BLE is the only dependency that would force cgo. Dropped. `[D13]`
- **User-authored ANSI art theme packs** — screen templates, layout manifests, per-menu artwork. A curated set of built-in themes, plus a simple `themes/*.toml` loader for colour and glyph overrides `[D15]` `[N5]` (§5.4)
- **DM metadata privacy.** Content confidentiality is required; hiding who-talks-to-whom is not. `[D7]`
- Real-time inter-BBS chat over mesh (latency is 10s of seconds to minutes; do it over IP)
- **A second, web-shaped UI.** Narrowed by `[D16]`, not withdrawn. There *is* a browser front end (§5.3, [webui.md](webui.md)), but it renders the same `Screen` descriptions the SSH front end does. What stays refused is a separate navigation model with its own screens, its own key bindings and its own place for every future feature to be implemented a second time.
- Legacy DOS door emulation in v1 — nice-to-have, deferred to Phase 7. `[D4]`

---

## 3. Architecture overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                          FRONT ENDS                                  │
│   SSH (primary)     Telnet (off by default)     Web (browser, [D16]) │
└───────────────────────────┬──────────────────────────────────────────┘
                            │  Session API (in-process, transport-agnostic)
┌───────────────────────────▼──────────────────────────────────────────┐
│                      SESSION / UI LAYER                              │
│   Bubble Tea model · menu graph · Screen() → ANSI | HTML renderers    │
│   terminal capability negotiation · multi-node presence               │
└───────────────────────────┬──────────────────────────────────────────┘
                            │  Service interfaces
┌───────────────────────────▼──────────────────────────────────────────┐
│                       DOMAIN SERVICES (core)                          │
│  Identity/Auth │ Forums │ DMs │ Files │ Doors │ Sysop │ Event bus     │
└──────┬──────────────────────────────────────────────┬─────────────────┘
       │                                              │
┌──────▼────────────────────┐          ┌──────────────▼─────────────────┐
│        STORAGE            │          │       FEDERATION ENGINE        │
│  SQLite (pure Go)         │◄────────►│  record log · version vectors  │
│  content-addressed blobs  │          │  anti-entropy · signing        │
└───────────────────────────┘          └──────────────┬─────────────────┘
                                                      │  Link interface
              ┌───────────────────┬───────────────────┼──────────────────┐
              │                   │                   │                  │
     ┌────────▼────────┐ ┌────────▼───────┐ ┌─────────▼──────┐ ┌────────▼──────┐
     │  MESH LINK      │ │   IP LINK      │ │  SNEAKERNET    │ │  FTN GATEWAY  │
     │  (Meshtastic)   │ │ (TCP + TLS1.3) │ │ (file bundle)  │ │ (echomail)    │
     │  MTU 233, ~108B/s│ │ MTU ~1400, MB/s│ │  offline       │ │  IP-side only │
     └────────┬────────┘ └────────────────┘ └────────────────┘ └───────────────┘
              │
       ┌──────┴──────┐
  ┌────▼────┐  ┌─────▼────┐
  │  Serial │  │   TCP    │      (BLE dropped — see §2, [D13])
  │  (USB)  │  │  (WiFi)  │
  └─────────┘  └──────────┘
         Meshtastic node
```

### The two load-bearing abstractions

**(a) `Link` — the federation transport interface.** Meshtastic is treated as *one* link type: a datagram link with a 233-byte MTU, ~108 B/s, high loss, and multi-minute latency. IP is another with a 1400-byte MTU and megabytes/second. Sneakernet (export a bundle to a USB stick) is a third. The FTN gateway (§7.7) is a fourth, with the unusual property of being a trust boundary as well as a transport.

This matters more than it sounds. If the sync protocol is designed for mesh from the start, it works everywhere. If it's designed for IP and mesh is bolted on later, it will never fit in 233 bytes. **Design for the mesh, get IP for free** — not the other way round.

```go
type Link interface {
    Name() string
    MTU() int                      // usable payload bytes per datagram
    Send(ctx context.Context, peer PeerID, b []byte) error
    Recv() <-chan Datagram
    // Rate/airtime accounting the federation engine must respect
    Budget() Budget                // bytes/sec allowance, current backpressure
    Capabilities() LinkCaps        // broadcast?, reliable?, ordered?, addressable?
}
```

**(b) `Transport` — how we talk to the Meshtastic node.** Serial and TCP speak the same protobuf stream API, so this is a thin layer. See §7.1.

---

## 4. Technology choices

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.23+ | Stated requirement; excellent cross-compilation |
| SSH server | `charmbracelet/wish` (over `gliderlabs/ssh`) | Middleware model, native Bubble Tea integration, PTY + window-resize handling |
| TUI | `charmbracelet/bubbletea` + `lipgloss` + `bubbles` | Elm architecture fits a menu-driven BBS well; one `tea.Program` per SSH session |
| Database | **`modernc.org/sqlite`** (pure Go) | **Critical:** avoids cgo. `mattn/go-sqlite3` needs a C toolchain, which destroys clean cross-compilation. Non-negotiable. |
| Serial | `go.bug.st/serial` | Pure Go, cross-platform, well-maintained |
| ~~BLE~~ | **dropped** `[D13]` | `tinygo.org/x/bluetooth` was the only credible option and needs platform SDK bindings (cgo on macOS). Removing it makes *every* release artifact cgo-free with no build tags. |
| Protobuf | `google.golang.org/protobuf` + `meshtastic/protobufs` as a git submodule | Official schema, generate Go bindings at build time `[D3]` |
| Compression | `klauspost/compress/zstd` with a **trained dictionary** | Pure Go. The single highest-leverage optimization on the mesh — see §7.4 |
| Erasure coding | **systematic LT / random-XOR repair symbols**, own implementation | Fountain coding is the chosen reliability strategy `[D1]`. See §7.2 for why an off-the-shelf RaptorQ is the wrong fit at our block sizes. |
| Hashing | BLAKE3 (`lukechampine.com/blake3`) | Fast, pure Go, good truncation properties |
| Signing | Ed25519 (`crypto/ed25519`) | Stdlib |
| DM encryption | X25519 sealed box (`golang.org/x/crypto/nacl/box` or a minimal custom construction) | 48-byte overhead vs. age's ~200; see §8.2 |
| CLI framework | **`spf13/cobra`** | Commands, subcommands, flags, generated help and shell completions for every binary (§11.6). Stated preference; also gives `completion bash\|zsh\|fish` and doc generation for free. |
| Config decoding | `BurntSushi/toml` or `pelletier/go-toml/v2` — **not Viper** | §11.3 requires that an unknown key be a startup *error*. Viper does not surface unknown keys at all and layers its own precedence model over the one §11.2 specifies. Cobra without Viper is a well-trodden combination; use the TOML decoder's `Undecoded()` / `DisallowUnknownFields` to get strict validation. |
| ID word rendering | BIP-39 English wordlist (embedded, ~13 KB) | 2048 phonetically distinct words, 11 bits each → 6 words per 64-bit node ID (§6.1.4.2) |
| Assets | `embed.FS` | Single-binary deployment with ANSI art, themes, migrations, dictionary, wordlist |

### Meshtastic library: roll our own `[D3]`

**Decided: vendor the official `meshtastic/protobufs` as a git submodule and write our own thin transport layer.** None of the existing Go libraries is a comfortable dependency:

- `meshnet-gophers/meshtastic-go` — right shape (`transport/`, `radio/`, `mqtt/`) but 11 stars, last release Feb 2024, and the README says its contracts are *"written with a half-eaten crayon."*
- `lmatte7/gomesh` / `lmatte7/meshtastic-go` — CLI-oriented, serial+TCP only.
- `exepirit/meshtastic-go` — newer (March 2026), MIT, unproven.

The framing is genuinely trivial — a 4-byte header (`0x94 0xC3`, then a 16-bit big-endian length) wrapping a `ToRadio`/`FromRadio` protobuf, identical across serial and TCP. That's maybe 300 lines. Cribbing structure from `meshnet-gophers` is fair game; depending on it is not.

---

## 5. Front ends and the session layer

### 5.1 SSH (primary)

Wish gives a middleware chain per connection. Auth supports:

- **Public key** — a user registers their SSH pubkey; subsequent logins are passwordless. This is the modern path and should be the default we advertise. It is also the hook for client-held DM keys (§8.2).
- **Password** — over the SSH transport, keyboard-interactive. Needed for new-user signup and for people connecting from a client they don't control.
- **Guest/anonymous** — read-only browsing, configurable.

#### How an account comes into existence

This is the one place where SSH is genuinely *worse* than telnet for a BBS, and it needs an explicit answer rather than an assumption. A classic BBS lets you connect and type `NEW` at the login prompt. SSH decides accept-or-reject **before any session exists**, so there is no prompt to type `NEW` at — the client has already committed to a username and a credential. Registration therefore has to be handled at the auth layer, not in the TUI.

**On the web this inverts, and the inversion is instructive.** Signup is trivial there — a page can prompt for anything — but the *credential* is the hard part: SSH hands the server a public key before the session exists, which is exactly why enrolment is free here and the next login is passwordless. Browsers cannot reach `ssh-agent`, and pasting a private key into a web page is not a design. Passkeys are the honest analogue and are what §5.3 uses `[D17]`.

Three entry points, in order of how users will actually arrive:

**1. The reserved `new` account (primary, documented path).**

```bash
ssh new@bbs.example.com
```

`new` is a reserved nick that the `PublicKeyHandler` accepts with *any* key, including one the server has never seen, and that the keyboard-interactive handler accepts with no password. It lands directly in the signup TUI. The elegant part: **SSH already handed us a public key**, so if the user connected with one, enrollment is free and their very next login is passwordless with no key-pasting step. Users on a client with no key fall through to setting a password.

**2. Unknown-nick offer (convenience).** `ssh austin@bbs.example.com` where `austin` doesn't exist → the same signup TUI, pre-filled with that nick. This is the flow people will discover by accident, and it's nicer than an opaque `Permission denied`. Two constraints:
- It must not become a nick-existence oracle beyond what a BBS already leaks (user lists are public on a BBS; this is not a meaningful new leak, but it *is* worth stating rather than discovering later).
- An **existing** nick presenting an unknown key must get a clear "this account exists; you're not authenticated" path — never the registration flow. Offering "register?" to someone whose key merely changed is how you get duplicate accounts and a confused user.

**3. Sysop-side creation (`meshbbs user add`).** Non-interactive, scriptable, and the only path that exists before the SSH server does. This is the testing lever (§6.7) and the sysop's break-glass tool.

Guest access is a fourth reserved nick, `guest`, accepted with no credential when enabled, giving a read-only session with no persistent identity.

Reserved nicks that can never be registered: `new`, `guest`, `sysop`, `admin`, `root`, `bbs`, `all`, `postmaster`, `daemon`.

Notably, SSH gives us **SFTP for free** as the file-transfer mechanism. No ZMODEM, no XMODEM, no serial-protocol emulation nonsense — `sftp bbs.example.com` and you're in the file areas. This is a large simplification over legacy BBS software.

### 5.2 Telnet (legacy, optional) `[D12]`

**Off by default, with a loud warning when enabled.** Exists because (a) some ANSI terminal clients (SyncTERM, NetRunner, MagiTerm) are telnet-only and are what BBS people actually use, and (b) DOS door bridging is easier over a raw socket. When enabled, the login banner and the sysop's startup log must both state that credentials cross the wire in plaintext. Guest-only telnet is a supported middle setting and is what we should recommend.

### 5.3 Web — a semantic terminal `[D16]`

**Reversed in v0.16.** This section previously specified `xterm.js` over a WebSocket, described it as optional and low priority, and justified it by "casual visitors and showing the BBS off". All four parts of that turned out wrong, and the full design is in **[webui.md](webui.md)**; what follows is the summary this document needs.

**The shape.** An `xterm.js` terminal in a browser is a character grid rendered in a page — it inherits every constraint of an 80-column screen while adding a dependency. Instead, the model emits a typed description of what is on screen and two renderers consume it:

```
Model.Screen() → Screen
                  ├─→ ansi.Render(Screen, Styles, w, h) → ANSI bytes   (SSH, telnet)
                  └─→ json.Marshal(Screen)              → WebSocket    (browser)
```

**The rule that makes it better rather than a reskin: the `Screen` carries semantics, the renderer owns geometry.** Truncation, viewport windowing and line wrapping moved out of the model, because the correct answer differs — an 80-column grid wants an area name cut to 26 characters, and a browser wants the whole name and a CSS ellipsis. If they had stayed in the model, the web UI would receive a pre-truncated 80-column snapshot and be a screenshot of a terminal.

**Why this seam and not a JSON API over `bbs.Service`.** That would mean a second navigation model, a second set of screens, and a second place to implement every future feature — which is what §2 declined, and it drifts the moment somebody adds a screen to one and forgets the other. Here a new screen *cannot* exist over SSH and be missing from the web: there is one `Screen()` method, and both renderers consume its output. The ANSI renderer is held to byte-identical output by the existing golden frames (§12.8), so this is a verifiable refactor rather than an aspiration.

**Input is keystrokes.** Clicking the `[M]` row sends `M`; the model never learns it is talking to a browser, so `handleKey` is unchanged and there are no web-only code paths in `Update`. The one exception is text entry, which sends a whole field value — character streaming breaks under autocorrect, predictive text and IME composition, all of which revise multi-character runs.

**Authentication is passkeys, and nothing else** `[D17]`. No password path, no SSH-key path. The credential is a keypair whose private half the server never holds — the same shape as SSH key auth and as the node identity in §6.1 — and discoverable credentials mean no nick is typed. Accounts predating the web bootstrap through a single-use, short-expiry enrolment code minted by an authenticated SSH session `[D18]`; it registers a credential and cannot mint a session.

**Two consequences, stated rather than discovered:**

- **No guest browsing on the web.** A passkey-only front door means an unauthenticated visitor sees a sign-in prompt and nothing else. This removes the "showing the BBS off" rationale this section originally gave, and it is a product decision rather than a technical detail. The SSH `guest` account is unaffected.
- **`web.origin` is required, with no default.** Passkeys bind to an origin, and a mismatch does not degrade — it fails every sign-in with a browser error that says nothing about the cause. Validated at startup like any other config error (§11.3).

**Presence is shared.** A web session calls `Presence.Join` exactly as SSH and telnet do, so it gets a node number and appears in `[W] Who's online` and the sysop status panel. One BBS, three doors — which is what makes the browser feel like part of the BBS rather than an adjacent website.

The static bundle is served from the binary via `embed`, consistent with §10's single-artifact packaging: no CDN, and no asset build in the release path.

### 5.4 Terminal rendering — the fiddly bit

BBS aesthetics mean **CP437 + ANSI art**, but modern terminals are UTF-8. Plan:

- Detect capability: `LANG`/`LC_ALL` env, terminal type, and a first-run user preference toggle.
- Store art as CP437 bytes; render through a translation layer that either passes bytes through (legacy client) or maps CP437 → Unicode box-drawing/block glyphs (modern client).
- Support SAUCE metadata on `.ANS` files (dimensions, font hints) — it's what art packs ship with.
- Handle window size: SSH gives us `pty-req` and `window-change`; Bubble Tea consumes these natively. Fall back to 80×24.
- **Themes: a small curated set, built in and embedded, plus a simple file loader.** `[D15]` `[N5]` Ship perhaps four (classic 16-colour ANSI, a muted 256-colour, high-contrast/accessible, monochrome for serial-ish clients).
  - Colour and glyph choices live behind a `Theme` struct rather than being hardcoded at call sites. This costs nothing now and is the difference between extending themes later being a weekend and being a rewrite.
  - **The loader reads `<datadir>/themes/*.toml` at startup** and merges each over the built-in defaults, so a sysop can retheme by editing a file. Roughly 50 lines, and it turns "I want different colours" from a feature request into a text edit.
  - **The boundary that keeps it small:** these are *style* overrides — colours, box-drawing glyphs, accent characters. They are **not** ANSI art theme packs with screen templates, layout manifests, and per-menu artwork, which is what `[D15]` declined. A malformed theme file is a startup error like any other config (§11.3), never a silent fallback.
  - Hot-reloadable (§11.3), so a sysop can iterate on a theme without dropping sessions.
  - **The same `themes/*.toml` files style the browser** `[D16]`. The eight `Theme` colour fields map onto CSS custom properties and `Border` onto a CSS border style, so web retheming needs no second mechanism and no second file format. This is where the bullet above — colour behind a `Theme` struct rather than hardcoded at call sites — paid for itself: the claim was that it made *extending* themes cheap, and what it actually made cheap was a whole second renderer. See [webui.md §11](webui.md).

---

## 6. Domain model

### 6.1 Identity and addressing `[D9]`

**Reversed in v0.4.** v0.2/v0.3 specified FidoNet-style numeric `zone:net/node` addresses. That is withdrawn in favour of **self-certifying key-derived node IDs**, with human-friendliness supplied by a local petname layer rather than by the address itself. Rationale and the trade being accepted are in §6.1.4; `[D9]` in §14 records the reversal.

#### 6.1.1 Node ID = the key

There is no separate address. **The node ID *is* the node's identity key**, truncated:

```
node_id = BLAKE3(ed25519_pubkey)[:8]          // 8 bytes / 64 bits
```

The property this buys, and the reason it's the right call for a mesh: **the ID is self-certifying.** Anyone holding a record can hash the claimed pubkey and check it against the ID in the header. There is no binding to maintain, no registry to consult, no authority to run, and — critically — **no way to squat an identity you don't hold the key for.** An entire category of machinery from v0.3 evaporates:

| v0.3 needed | v0.4 |
|---|---|
| First-seen address→key binding | *gone* — the binding is arithmetic |
| Anti-squatting conflict detection and alerting | *gone* — nothing to squat |
| Quarantine until a node's `NODE` record arrives | *reduced* — only the pubkey is needed, and it self-verifies |
| An address registry / coordinator (`N1`) | *gone* |
| Zone/net numbering scheme (`N4`) | *gone* |

**Why 8 bytes and not 4.** A shorter ID would save wire bytes, and against *accidental* collision 4 bytes would be fine (50 nodes → ~3 × 10⁻⁷). But the threat is adversarial: an attacker grinds keypairs until one hashes to a target node's ID, then presents forged records to peers that haven't yet learned the real pubkey, or slips into a sysop's allowlist. 32 bits is grindable in minutes on a GPU; 48 bits is hours-to-days. **64 bits (2⁶⁴ ≈ 1.8 × 10¹⁹) puts it out of reach**, and §6.1.3 shows the extra bytes cost nothing in practice.

#### 6.1.2 The `NODE` record still exists, for a different job

Not for binding — for **key distribution and metadata**. A `NODE` record carries `{node_pubkey, display_name, sysop_contact, capabilities}`, self-signed. Receivers verify it by hashing the pubkey and confirming it matches the `origin` ID, then verifying the signature. It is entirely self-validating; there is no trusted-first-sight rule and no conflict case.

- Records from an unknown origin are **quarantined pending the `NODE` record** (which may be minutes behind on a lossy mesh), then verified retroactively.
- Roster size: ~50 nodes × ~100 B = **~5 KB**, trivially replicated, cheap to backfill, and safe to serve to anyone since it's all public keys.
- `NODE` records are **idempotent and replayable** — any node can rebroadcast another's `NODE` record without trust implications, which makes bootstrapping a new instance genuinely easy: ask any peer for the roster.

#### 6.1.3 Wire encoding — hoist the origin, and it gets *cheaper*

The naive concern is that 8-byte IDs cost twice the 4-byte packed address they replace. They don't, because **`origin` hoists out of the record and into a per-bundle origin table** — the same trick §6.2 already applies to `area` and the base timestamp:

```
Bundle header:  … | origin_count u8 | origin_id[0] (8B) | origin_id[1] (8B) | …
Record:         origin_idx u8 | seq | ts_delta | type | …
```

Bundles are usually single-origin (a node batching its own posts), so the 8-byte ID is paid **once per bundle** and each record carries a 1-byte index. Relayed multi-origin bundles list each distinct origin once.

| Records in bundle | v0.3 numeric (4 B/record) | v0.4 key-derived (table + 1 B/record) | Δ |
|---:|---:|---:|---:|
| 1 | 95 B | 101 B | +6 |
| 2 | 184 B | 187 B | +3 |
| **3** | **273 B** | **273 B** | **0** |
| 5 | 451 B | 445 B | −6 |
| 10 | 896 B | 875 B | −21 |
| 20 | 1786 B | 1735 B | −51 |

(Per-bundle overhead excluding bodies: headers + `seq`/`ts`/`type` + `parent` + signature.)

**Break-even is 3 records, and §7.3 mandates 15–30 minute batching precisely so bundles are bigger than that.** In the common case the reversal *saves* wire bytes. It costs 6 bytes only on a single-record bundle — which, per §7.2, is the K=1 fast path used for urgent DMs, where 6 bytes is noise.

#### 6.1.4 The human problem, and the petname layer

This is the real cost of the reversal and it deserves a straight answer rather than a hand-wave. `K7QM4X2PB9TFR` is not memorable and not speakable over voice radio. `42:100/7` was. **Key-derived IDs are strictly worse as things humans type**, and the design has to make up for it somewhere.

This is Zooko's triangle: identifiers can be *globally unique*, *human-meaningful*, and *decentralized* — pick two. Numeric addressing chose unique + human-meaningful and paid with a registry. Key-derived chooses unique + decentralized. The missing leg is supplied by **petnames**, and they have to be a real feature, not an afterthought:

- **Sysop-managed local aliases are the everyday surface.** `[N1]` Each BBS keeps one alias table, exactly like `~/.ssh/config` `Host` entries or `/etc/hosts`. **The sysop owns it; users do not have private alias tables** — one namespace per instance, not one per user. Users type `austin@pnw`, never the ID. Aliases are **local**, so two BBSes may disagree about what `pnw` means and neither is wrong, which is exactly why no registry is needed.
- **Aliases resolve in addressing, not just display.** `[N1]` `austin@pnw` is a valid thing to type anywhere a destination is accepted. See §6.1.4.1 for the resolution rule, which is the part that must not be got wrong.
- **Self-declared display name.** Each node publishes one in its `NODE` record (`pnw-bbs`, `Fog City`). **Not unique, not authoritative, never used for routing** — purely a label. The UI always renders it alongside the short ID so a spoofed name is visibly attached to the wrong ID.
- **One-keystroke accept, never auto-bind.** `[N1]` A `NODE` record's display name is a *suggestion* the sysop may accept as an alias with a single keystroke. Nothing binds without that keystroke, and the acceptance prompt shows the full ID in both renderings (below). This keeps onboarding a new peer fast without reintroducing a namespace to fight over.

##### 6.1.4.1 The resolution rule

**Aliases are resolved locally at compose time; an alias never goes on the wire.** `austin@pnw` becomes `austin@K7QM4X2PB9TFR` before the DM record is constructed, and inbound records are re-rendered through the *local* table on display. This is not a detail — the receiving node's alias table is a different table, and putting `pnw` in a record would make delivery depend on the recipient agreeing with your private naming. The wire format has no alias field and never will.

Consequences to build for:
- **Unresolvable alias is a delivery failure with its own error path**, raised at compose time, not silently at send time: "no node known as `pnw` — ask your sysop to add it, or use the full ID." Because aliases are sysop-only, the user's remedy is a request, not a fix, so the error should offer to send that request.
- An alias that is *retargeted* by the sysop changes where future mail goes. Alias edits are audit-logged (§11.6), and the sysop TUI warns when retargeting an alias that has been used as a DM destination.
- Ambiguity between an alias and a literal ID is resolved by trying the literal first — a valid 13-character Crockford string is never treated as an alias, and aliases are forbidden from *looking* like one.

##### 6.1.4.2 Two renderings of the same 64 bits `[N4]`

**Both encodings ship, and it is important that they are two ways of writing one identifier — not two identifiers, and not two addressing schemes.** There is exactly one node ID; these are display codecs over the same 8 bytes, and either converts losslessly to the other.

| Rendering | Form | Used for |
|---|---|---|
| **Crockford base32** | 13 chars, `K7QM-4X2P-B9TFR` | Typing, config files, allowlists, URLs, logs, everyday display (first 8 chars, git-short-hash style) |
| **Word list** | 6 words, `pilot-ranch-obey-vivid-scout-amber` | Reading aloud — voice radio, phone, in-person fingerprint confirmation |

- Nobody types the words and nobody speaks the base32. Both surfaces exist because they serve different physical channels.
- **Correction to the estimate in the original question:** it proposed "four or five words," which is wrong. 64 bits needs 6 words from a 2048-word list (11 bits each = 66 bits) — 4 words would require a 65,536-word list, and 5 would need 4,096 obscure ones. Use the **BIP-39 English wordlist**: 2048 words, already engineered for exactly this (phonetically distinct, no two words sharing their first four letters, ~13 KB embedded).
- The word rendering appears **only** where a human is verifying an identity out of band — the allowlist-confirmation screen, `meshbbs id`, and the peer-add flow. It is not an input method; there is no "type your words to log in."

Honest summary of the trade: **routing and trust become simpler and safer; typing an unfamiliar node ID becomes worse, and the alias table is what you are relying on to hide that.** If the alias UX is weak, this decision will feel bad in daily use.

#### 6.1.5 User identity

An Ed25519 keypair, generated server-side at signup (with an option for the user to supply their own). A globally-addressable user is `nick@NODEID` — e.g. `austin@K7QM4X2PB9TFR`, or `austin@pnw` through a local alias. The pubkey is what matters for DM encryption; the nick is a display convenience and *is not globally unique*.

#### 6.1.6 Key rotation and succession `[N2]`

Because the ID *is* the key, rotating the key means **becoming a different node**. A `SUCCESSION` record handles the planned case:

```
SUCCESSION {
  origin      [8]byte    // the OLD node ID
  successor   [8]byte    // the NEW node ID
  new_pubkey  [32]byte   // full pubkey of the successor, so it self-verifies
  effective   uint32     // unix seconds
  sig         [64]byte   // signed by the OLD key
}
```

**Peers auto-follow.** On receipt, a peer verifies the old key's signature, confirms `BLAKE3(new_pubkey)[:8] == successor`, then migrates allowlist entry, alias, peer config, and per-area version-vector state from old ID to new. The sysop is not asked first — that was the decision — but four guardrails come with it, because auto-follow means *whoever holds the old key can redirect every peer in the network*:

1. **Always alert, never silent.** Auto-follow still raises a sysop notification and an audit-log entry naming both IDs in both renderings (§6.1.4.2). Auto-follow removes the *blocking prompt*, not the *disclosure*.
2. **The old ID is tombstoned at `effective`.** Records from the old ID with `ts > effective` are rejected. Records already in flight with `ts ≤ effective` remain valid, so a lossy mesh doesn't lose the tail of the predecessor's log.
3. **One succession per predecessor.** A second `SUCCESSION` signed by the same old key is rejected and alerted — an attacker with the old key gets one shot at a visible, logged redirect rather than an untraceable game of pass-the-parcel. Chains (A→B→C) are fine, since each hop is signed by the then-current key.
4. **Rate limit and sanity window.** `effective` more than a short interval in the future or the past is rejected; this prevents pre-dated successions being held and replayed later.

**A *lost* key has no recovery path, and the docs must say so plainly.** With no old key there is nothing to sign a `SUCCESSION` with. The honest answer is "you are a new node; re-establish with your peers out of band" — which, because peer setup is just an ID exchange (§8.4), is a handful of messages rather than a re-registration. This is the price of having no registry, and it is the *reason* there is no registry to petition. Back up `keys/node.ed25519`; the first-run wizard says so and offers to print the key as a recovery phrase.

### 6.2 The record log — the heart of federation

Everything replicable is an **immutable, signed record** in a single append-only log:

```
Record {
  id        [16]byte   // BLAKE3(canonical_body)[:16] — DERIVED, NOT TRANSMITTED
  origin    [8]byte    // BLAKE3(node_pubkey)[:8] — HOISTED to a per-bundle origin table
  seq       varint     // per-origin monotonic sequence (~2B typical)
  ts        varint     // delta from the bundle's base timestamp (~2B typical)
  type      uint8      // POST, DM, PROFILE, NODE, SUCCESSION, AREA, FILE, TOMBSTONE, VOTE, DOOR_EVENT
  area      [4]byte    // truncated hash of area name — HOISTED to bundle header
  parent    [16]byte   // threading: parent record id (optional, flag-gated)
  body      []byte     // type-specific, zstd-compressed with shared dict
  sig       [64]byte   // Ed25519 over everything above, by origin node key
}
```

Design notes:

- **The `id` is never transmitted.** It is `BLAKE3(canonical_body)[:16]`, and the receiver recomputes it from the fields it already received. v0.1 put it on the wire; that was 16 wasted bytes, **7% of a mesh packet, per record.** Content addressing still gives free dedup — the mesh floods packets and the same record arrives via multiple paths — the ID just doesn't need to be sent to do that.
- **Truncated to 16 bytes** where it *is* transmitted (`parent`). A full 32-byte BLAKE3 would cost 7% of a packet per reference; 128 bits is ample collision resistance at this scale. `parent` is flag-gated so top-level posts don't pay for it.
- **`origin`, `area`, and the base `ts` all hoist to the bundle header.** Bundles are per-area and usually single-origin, so the 4-byte area tag and the 8-byte origin ID are each paid once per bundle; records carry a 1-byte origin index and a small timestamp delta. This is what makes 8-byte self-certifying IDs cheaper than 4-byte numeric addresses at any bundle size ≥ 3 (§6.1.3).
- **`(origin, seq)` pairs make reconciliation a version-vector problem**, which is the simplest correct approach. See §7.3.
- **Immutable + tombstones** means forums have *no merge conflicts at all*. An edit is a new record with a `supersedes` pointer; a delete is a signed tombstone. Whether a BBS honors a *remote* delete is local sysop policy — see below.
- **The node signs, not the user, for forum posts.** `[D5]` A 64-byte Ed25519 signature is 27% of a mesh packet, so dual-signing every post is unaffordable. Node-signing means "the instance holding key `K7QM4X2P…` vouches that user `austin` posted this," which matches the FidoNet trust model — you trust the sysop, and the sysop is accountable. **DMs are user-signed** (§8.2), where non-repudiation actually matters and volume is lower.

#### 6.2.1 Encoding discipline — three ways to silently break every signature

Found while auditing for §12. All three are **Phase 0 architectural constraints**, not later hardening: each is cheap to design in and effectively unfixable once a network has history.

1. **Canonical encoding is frozen and versioned independently of the database schema.** Verifying a signature requires reconstructing the signed bytes *exactly*. If records are stored decomposed into columns and re-serialized to verify, then any encoder change — a field reorder, a varint tweak, an added optional field, a migration — invalidates every historical signature at once. **Store the original signed bytes verbatim** alongside the decomposed form and verify against those. The canonical encoder is a frozen artifact with its own version byte and conformance vectors (§12.6), and it is emphatically *not* whatever the ORM happens to emit this month.

2. **Deterministic serialization — which in Go means never ranging over a map.** Go randomizes map iteration order on purpose. An encoder that walks a map to emit fields produces different bytes on different runs, hence a different record `id` and a signature that verifies on the machine that made it and nowhere else. This is the single easiest way to introduce a heisenbug that only appears in federation. Sort keys explicitly, make it a lint rule, and cover it with the determinism property test in §12.2.

3. **`seq` must never regress, including across a restore from backup.** This is the subtle one. Version vectors mean a peer that has seen `seq ≤ 500` from us **will never ask for 450–500 again**. Restore a week-old backup, reissue those numbers with different content, and the divergence is permanent and *silent* — no error, no conflict, peers simply hold different records under identical coordinates forever. Fix: persist the high-water mark with an `fsync` on every advance, and carry an **incarnation counter** in the `NODE` record that increments whenever a regression is detected at startup. Peers seeing a new incarnation know to treat the origin's log as needing re-verification rather than assuming continuity. Sysop docs must also say plainly that restoring `bbs.db` from backup is not a routine operation.

Two smaller bounds in the same family:

- **Maximum record size** is capped (suggest 8 KB pre-compression). Without it, a hostile or merely buggy peer can force unbounded fragment buffering — see the resource-exhaustion tests in §12.5.
- **Clocks are advisory.** `ts` feeds ordering, tombstone precedence, `SUCCESSION` windows (§6.1.6), DM TTL, and retention — but an off-grid Raspberry Pi may have no RTC and no NTP, and will happily boot believing it is 1970. Rules: prefer time from the Meshtastic node where GPS disciplines it; **never make correctness depend on absolute time** (causal ordering comes from `(origin, seq)` and `parent`, not `ts`); and where absolute time is unavoidable, use generous windows that tolerate skew rather than tight ones that reject honest peers.

### 6.3 Forums

- **Areas** (a.k.a. echo areas / conferences) are the unit of replication. Each has a name, description, moderation policy, and a list of peer nodes it federates with.
- Local-only areas exist and never touch the mesh. **Default new areas to local-only** — sysops must opt *in* to burning airtime. At the §1.1 budget this default is doing real work.
- Threading via `parent` pointers; the UI reconstructs trees. Out-of-order arrival is normal on a mesh, so orphaned replies must render gracefully ("parent not yet received") and re-parent when the parent arrives.
- Per-area retention policy (age, count) — old records are prunable; the log is not required to be complete forever.
- **Per-area airtime sub-budget.** Given ~10 originated packets/day/node at 50 instances, one chatty area can starve every other. Each federated area gets a share of the node's governor allocation. This is also the mechanism that makes an FTN-bridged echo safe (§7.7).

### 6.4 Direct messages

- Addressed to `nick@NODEID` (or `nick@alias` locally, §6.1.4). Routed by node ID.
- **End-to-end encrypted, user-signed** (§8.2). Intermediate BBSes and everyone else on the mesh channel store and forward opaque bytes.
- **Recipient addressing is in the clear.** `[D7]` v0.1 proposed routing on an opaque `BLAKE3(recipient_pubkey)[:8]` tag to hide the recipient from intermediate sysops. Since metadata privacy is explicitly not a requirement, we drop the indirection and route on the destination node ID plus a cleartext nick — so intermediate nodes can bounce undeliverable mail immediately instead of holding it blind, and sysops can rate-limit and spam-filter per recipient.
- **Mail has its own well-known area** (`_mail`), derived like the directory's `_directory` (§8.2). It is emphatically NOT the zero area tag — that is the roster (§6.1.2), which is always federated, so mail written there is private mail in the one area every peer syncs, with its cleartext sender and recipient `[D7]` in front of every sysop on the channel (§8.1), spending the roster's sequence numbers as it goes. The implementation did exactly that until this was found, which is why it is written down here rather than left to the code.
- **The mail area does not replicate yet.** Off-node delivery does not exist (there is no store-and-forward, below), so there is nothing to gain from putting mail on the air and a metadata leak to lose. The replication layer excludes it in both directions: never offered in a digest, never served, never accepted from a peer. When store-and-forward lands, that exclusion is the one thing to change — the governor already classifies the mail area as DM traffic, which is what arms §8.3's Part 97 refusal.
- Store-and-forward: if the destination node isn't reachable, hold and retry with exponential backoff, with a TTL (default 7 days) after which we return a bounce to the sender.
- DMs sit above forum posts in the governor's priority order (§7.6) — they are the one class we keep transmitting under backpressure.

### 6.5 Files `[D8]`

- Local file areas with upload/download via SFTP over SSH, plus an in-TUI browser.
- Content-addressed blob store (BLAKE3 → `blobs/ab/cd/abcd...`), so identical files across areas dedup.
- **Over mesh, only the catalog replicates.** `FILE` records carry name, size, hash, description, tags, and holding node — roughly 120–200 bytes compressed. Users see the whole network's file list.
- **Descriptions are set after upload, not during it.** SFTP carries a name and bytes and has no field for anything else, so an upload always arrives with an empty description. It is set afterwards, by the uploader or a sysop, with `meshbbs file describe <area> <name> "<text>"` or by pressing `d` in the file browser. The limit is 80 bytes, and it is the wire's rather than the database's: a description that doubles the size of a catalog entry is one fewer file a node can announce within §1.1's budget. Changing a description detaches the file from its published `FILE` record, so the next publish re-announces it — the record carries the description, so an edited one makes the copy peers hold stale.
- **Mesh file transfer does not exist as a code path.** Not a quota, not a sysop toggle, not a "tiny files only" exception — the mesh link refuses `FILE_DATA` payloads outright. v0.1 proposed an 8 KB trickle option; it is removed. Fetch paths are exactly two:
  1. **Direct IP** from a holding BBS (the TLS 1.3 link of §7.9, or plain HTTPS if the sysop publishes one).
  2. **Sneakernet queue** — the request is recorded, and satisfied at the next bundle exchange.
- Be honest in the UI: a file with no IP-reachable holder shows "available by request only — queued for next exchange," with the requesting user notified when it lands. Not an error, not a spinner that never resolves.

### 6.6 Doors — see §9.

### 6.7 Accounts: registration, lifecycle, and recovery

§5.1 covers the SSH-layer mechanics of *getting to* a signup screen. This covers what registration actually does, what policy governs it, and — the part with a genuine airtime consequence — when a new account becomes visible to the rest of the network.

#### The property that makes this simple: nicks are local

Per §6.1, a nick is not globally unique; `austin@K7QM4X2PB9TFR` and `austin@M2X9F0QLD4H7A` are different people. **Registration therefore needs no network coordination whatsoever** — no name reservation, no distributed lock, no waiting for a mesh round-trip that costs minutes. A brand-new instance with no radio attached and no peers can create accounts immediately. This is worth stating explicitly because the obvious alternative (globally unique nicks) would make signup depend on a link with multi-minute latency and no delivery guarantee, which would be miserable.

Uniqueness is enforced case-insensitively within the instance. Nick rules: 2–16 characters, `[A-Za-z0-9_-]`, must start with a letter, no leading/trailing separators, not in the reserved list (§5.1). Display names are separate and may be richer.

#### What signup collects

| Field | Required | Notes |
|---|---|---|
| Nick | yes | Rules above; local uniqueness |
| Password | only if no pubkey enrolled | Argon2id, never reversible |
| SSH public key | auto | Captured from the connection if present; users may enroll several later, one per client |
| Real name | no | Sysop-configurable whether shown |
| Email | **not collected** `[N8]` | Deliberately absent. An off-grid BBS may have no path to send mail, so email could never be load-bearing for recovery — and not collecting it is one less piece of PII to protect. There is no field, no column, and no optional prompt |
| Terminal prefs | defaults offered | CP437 vs. Unicode, theme, width fallback (§5.4) |
| DM passphrase | yes, with a default | Wraps the X25519 key (§8.2 tier 2). Defaults to "same as login password"; a separate passphrase is offered |
| Federated posting | not asked | Off by default — see below |

**Multiple pubkeys per user is a v1 requirement, not a later nicety.** One key per laptop, phone, and shell box is the normal case, and a schema with a single `pubkey` column is a painful migration later.

#### The DM key, created at signup

Per §8.2, the user's X25519 keypair is generated at signup and stored wrapped by an Argon2id key derived from their DM passphrase. Two consequences that must be surfaced *at signup*, not in a man page:

- **A lost DM passphrase means permanently unreadable DM history.** There is no recovery path and inventing one would defeat the point. The signup screen says so in plain language and requires an acknowledgement.
- If the passphrase is shared with the login password, changing the login password re-wraps the DM key — which works only while the plaintext key is in memory, i.e. during an authenticated session. A sysop-forced password reset therefore **cannot** re-wrap it, so a sysop reset must either preserve the DM passphrase separately or explicitly warn that DM history will be lost. Get this right in Phase 1; it is exactly the kind of thing that becomes unfixable once real users have real mail.

#### Registration policy modes

A config enum (§11), all four needed eventually, `open` shipping first:

| Mode | Behaviour |
|---|---|
| `open` | Anyone may register and use the BBS immediately |
| `approval` | Registration creates a pending account; the user can log in but sees only a "awaiting sysop review" screen. Sysop approves from the TUI or `meshbbs user approve` |
| `invite` | Requires a code from `meshbbs invite new` — single-use, optionally expiring, optionally pre-granting capabilities |
| `closed` | No self-registration; `meshbbs user add` only |

**Decided default: `open`, with federated posting off.** `[N7]` This decomposition matters and is the airtime-aware answer. A new user can immediately register, browse, post to local areas, and play doors — the classic open-BBS feel — but their posts do **not** consume the shared mesh budget until the sysop grants a `post_federated` capability. The door is open; the commons is gated.

This puts a real burden on the UI, which is the cost of the choice: a user who posts and sees nothing federate must never be left guessing. Required affordances — every area displays its scope (**Local to this BBS** / **Federated**) in its header; a user without `post_federated` sees federated areas as read-only with an explicit "you can read this area; ask the sysop for posting access" rather than a silent failure; and the sysop's pending-grants list is surfaced on the status screen so requests don't rot.

Per-capability grants (`post_federated`, `send_dm_offnode`, `upload_files`, `run_doors`) are cheaper to reason about than a role ladder and map directly onto the abuse vectors that actually exist here.

#### Rate limiting

Registration is an abuse vector on a public SSH port. Per-IP caps on registrations/hour and on failed auth attempts, a global cap on pending accounts, and a configurable delay before a new account may post. All in §11.

#### When does the network learn about a new user? (Lazy PROFILE publication)

This is the sharp edge. A `PROFILE` record (nick, node address, X25519 pubkey, signature) is ~100 B compressed and is what makes a user DM-addressable network-wide. Publishing one per account eagerly is unaffordable at the §1.1 budget:

| Local users | PROFILE bytes | Cost in that node's *entire* mesh budget |
|---:|---:|---:|
| 10 | 1.0 KB | 0.4 days |
| 50 | 5.0 KB | **2.0 days** |
| 200 | 20 KB | **7.9 days** |

Fifty users would consume two full days of the node's total mesh allocation just announcing that they exist — before anyone posts anything. Network-wide, 50 nodes × 50 users is ~244 KB of pure directory data.

**Therefore: PROFILE records publish lazily and only on demand.** A profile is emitted when, and only when, the user first does something that requires the network to know them — posts to a federated area, or sends an off-node DM. Registering does not publish anything. Additionally:

- Profiles piggyback on bundles the node is already sending (same mechanism as digests, §7.3), so in the normal case they cost no dedicated packet.
- **Users are listed by default** `[N9]` — a BBS user list has always been public, and discoverability is most of the point of a network directory. A user may set themselves **unlisted** from a visible per-user toggle: they can still send off-node DMs but no PROFILE is published, so they aren't in the directory. Receiving a reply still works because the sender's DM carries what's needed.
- Account deletion emits a `TOMBSTONE` for the profile; local-only accounts that never published need no tombstone.
- **Directory backfill is pull, not push.** A node that wants the full network user directory requests it; nobody broadcasts their whole roster.


#### Lifecycle and recovery

- **Idle/expiry:** configurable inactivity window after which an account is marked dormant (not deleted). Dormant accounts don't count against pending caps and are excluded from directory publication.
- **Deletion:** local records are removed; already-federated *posts* are not retracted by default (they're immutable and other nodes' tombstone policy is advisory, §8.4). The UI must say this before confirming — "your posts on other systems will remain" is the honest version.
- **Lost password:** sysop reset via `meshbbs user passwd <nick>` run locally, with the DM-key caveat above. No email-based reset, because email may not exist.
- **Lost pubkey:** enroll another via password login; if both are gone, sysop reset.
- **Lost sysop credentials:** `meshbbs user grant sysop <nick>` executed locally as the service user. Filesystem access to the datadir is the root of trust, which is the correct and conventional answer for self-hosted software.

#### The testing story — available from Phase 0

Explicitly a Phase 0 deliverable, because federation work in Phase 2 needs populated instances and there is no SSH server until Phase 1:

```bash
meshbbs user add --nick alice --password-stdin --pubkey ~/.ssh/id_ed25519.pub
meshbbs user add --nick bob --no-login          # DM target with no interactive access
meshbbs user grant alice post_federated
meshbbs dev seed --users 20 --areas 3 --posts 200 --seed 42   # deterministic
meshbbs dev login-token alice                    # one-shot token for automated TUI tests
```

`dev seed` being **deterministic on `--seed`** is the requirement that makes the simulated mesh harness (§10) useful: N instances seeded from known seeds produce a known, reproducible divergence to reconcile, which is how you actually test anti-entropy. `dev *` subcommands are compiled in always but refuse to run against a datadir whose config sets `environment = "production"`.

---

## 7. Federation: the mesh sync protocol

Call it **BSMP** (BBS Sync over Mesh Protocol). Five layers, each with one job.

```
┌─ L4  Records ──────── POST / DM / PROFILE / NODE / SUCCESSION / FILE / TOMBSTONE …
├─ L3  Replication ──── version vectors, anti-entropy digests, delta requests
├─ L2  Bundle ────────── zstd(shared dict) + framing of N records
├─ L1  Coding ────────── split bundle → ≤223B symbols, systematic + fountain repair
└─ L0  Datagram ─────── Meshtastic MeshPacket, portnum PRIVATE_APP → registered
```

### 7.1 L0 — the Meshtastic datagram

**Channel.** A dedicated Meshtastic channel (e.g. named `bbsnet`) with its own PSK, distinct from the community's primary channel. Meshtastic supports 8 channel slots; this keeps BBS traffic logically separate and lets non-BBS nodes ignore it. **But note:** channel separation is *not* airtime separation — all channels on the same frequency slot share the same physical airtime. A separate channel does not make us a better neighbour; the governor (§7.6) does. (And in ham mode the channel PSK itself must be off — see §8.3.)

**Port number.** `PRIVATE_APP` (256) during development; **request a registered allocation from the Meshtastic project once the wire format is frozen.** `[D10]` That sequencing is deliberate — registering a portnum is a public commitment to a stable format, so it comes after the format has survived contact with real radios, not before. Concretely this means:

- Every BSMP frame carries a 2-bit version field from day one (§7.2), and the L2 bundle header carries a format revision byte.
- Pre-freeze builds treat *any* version mismatch as "drop and log," with no compatibility promises between dev releases.
- The freeze is a roadmap deliverable (Phase 6) with a written spec document, a conformance test vector set, and only then the portnum request.

**Transports to the local node** — both speak the same protobuf stream:

- **Serial/USB** — `go.bug.st/serial`, 4-byte frame header (`0x94 0xC3 <len_hi> <len_lo>`) + protobuf. Most reliable; recommended default.
- **TCP** — the node's WiFi API on port 4403, same framing. Best when the node is mounted somewhere with better RF than the server closet.
- BLE is not supported. `[D13]`

The adapter auto-detects: scan serial ports for Meshtastic VID/PIDs, then try the configured TCP host. **Corrected in v0.12 — VID/PID scanning is not available everywhere.** Reading USB identifiers is pure Go on Linux (sysfs) and Windows (the registry), but on macOS it requires IOKit through cgo, and §4 makes cgo-free builds non-negotiable. macOS therefore matches device names (`cu.usbmodem*`, `cu.usbserial*`, `cu.wchusbserial*`) instead, which is weaker evidence — so detection *ranks* candidate ports and explains each guess rather than silently picking one, and the config file can always name the port outright. Two details that are not cosmetic: `/dev/tty.*` is excluded in favour of `/dev/cu.*`, because opening the tty twin blocks until carrier detect and presents as the BBS hanging at startup; and most Meshtastic boards use generic USB-UART bridges, so even a VID/PID match identifies a *chip*, not a radio.

#### 7.1.1 What the local wire actually looks like

Written after building it. The framing is as simple as §4 `[D3]` claimed, but three properties of the stream are load-bearing and none is in the protobuf definitions:

- **The serial line is not a clean frame stream.** Firmware debug output shares the same UART, so plain text arrives interleaved between frames. The reader therefore resynchronises: anything that is not a well-formed header is skipped one byte at a time. Skipped bytes are counted, because a port producing bytes and no frames is the signature of a wrong baud rate — a diagnostic worth surfacing rather than a silent hang. Our resync advances a **single** byte on a mismatch, where the reference client drops the whole two-byte candidate and so loses a frame preceded by a stray `0x94` — exactly what a truncated previous frame leaves behind.
- **Connecting begins with a config exchange.** The client sends `want_config_id`; the node answers with a burst of its own state, terminated by a matching `config_complete_id`, and will not accept packets to send until that completes. The ID is what distinguishes our dump from the tail of a previous client's. This is also where the governor gets its inputs — region, modem preset, hop limit — and where §8.3's ham-mode check can see which channel slots carry a PSK.
- **Two different maxima.** The local frame limit is 512 bytes (`MAX_TO_FROM_RADIO_SIZE`); the mesh MTU is 233. They are unrelated numbers and conflating them would either truncate a `NodeInfo` or, far worse, let something oversized for the air look acceptable locally.

Waking a sleeping or desynchronised node is 32 bytes of `0xC3` — deliberately never `0x94`, so a node left mid-frame by a client that vanished cannot mistake the wake sequence for a header.

#### 7.1.2 Addressing — binding node IDs to radios `[N12]`

**A gap this document had.** The federation addresses peers by node ID: 8 bytes, `BLAKE3(pubkey)`, self-certifying, no registry (§6.1.1). Meshtastic addresses radios by a 4-byte node number assigned by firmware. Nothing here said how one maps to the other, and the `nodes` table had no column for it. Three answers were considered:

| | Per-packet cost | Per bundle at K=15 | Resolves an unknown peer? |
|---|---|---|---|
| Full 8-byte sender ID in every packet | 3.4% of MTU | 120 B | Yes |
| 4-byte ID prefix, resolved against the roster | 1.7% of MTU | 60 B | **No** — a prefix means nothing without the roster |
| **Learned binding from a signed `ANNOUNCE`** | **0** | **8 B, once** | Yes, on demand |

**Decided: learn it.** Carrying identity in every packet re-inflates precisely what §6.1.3 hoisted out of records, and the 4-byte compromise pays airtime forever without even fixing the bootstrap case it appears to address. So no packet carries identity bytes at all, and the binding comes from an announcement that is self-certifying in the same way node IDs are:

```
ANNOUNCE:  type(1) | pubkey(32) | radio_num(4) | unix_secs(4) | signature(64)   = 105 B
```

- **The radio number is inside the signed body.** Without that, an attacker could rebroadcast a peer's announcement from their own radio and inherit its unicast traffic. Signing the claim makes a captured frame useless anywhere but where it came from, and the receiver checks it against the Meshtastic header.
- **The timestamp is for ordering, not dating.** A node's radio is not part of its identity — sysops replace hardware — so a later announcement rebinds. The timestamp is what stops that being a free redirection primitive: bindings only move *forward* in a peer's own time, so replaying an old announcement cannot drag a node back to a radio it has left. Announcements more than a day ahead of local time are refused, since a node with a broken clock could otherwise pin its binding permanently.
- **Discovery is demand-driven, not broadcast-driven.** A node announces on connect and every 12 hours, but the connect-time announcement only reaches peers that are already listening — the first node on a mesh announces to nobody. Having every peer answer a newcomer would rebuild §7.3's reply storm at the link layer, so instead an unattributable sender is asked directly: a 1-byte unicast `WHO_IS`, rate-limited to once per radio per 15 minutes, answered with a unicast `ANNOUNCE`. Two packets, only when there is something to learn. Announcing often enough to avoid the question would cost about **1% of the whole channel** at fifty instances — a fifth of the entire federation budget spent saying hello.
- **An unattributed datagram is dropped, not guessed at.** L1 keys decoder state on the sender and derives repair masks from it (§7.2), so a datagram filed under the wrong node ID would not merely be misplaced — it would corrupt a decode.

#### 7.1.2.1 What the first two-radio bring-up established

Two Heltec V3s on 2.7.15, both on a `bbsnet` secondary channel, running two instances over LoRa. Discovery, broadcast in both directions and attribution all work. Two things had to be fixed or understood first, and neither is visible without hardware.

**Packet IDs are part of the wire contract.** Meshtastic suppresses duplicates by `(sender, packet_id)` for several minutes. Anything that replays an ID sequence — a node restarting with a deterministically seeded RNG, which §12.1 otherwise encourages everywhere — has its traffic **silently dropped by every peer that heard the previous run**. No error, no NAK, just an instance nobody can reach. It presented as two links discovering each other on the very first run and never again. Packet IDs therefore come from a separate, entropy-seeded source; determinism is right for the simulator and wrong for the air.

**Unicast is not simply "broadcast with an address".** Firmware 2.5+ encrypts direct packets with the destination's public key rather than the channel PSK. A unicast to a peer whose Meshtastic-level key we have not exchanged (or whose key has changed since it was cached) arrives as an undecryptable payload on channel 0 — the receiving *firmware* acknowledges it, the receiving *application* never sees it. Broadcast, which uses the channel PSK, is unaffected.

That is survivable because the sync protocol already prefers broadcast everywhere it matters — §7.2's whole argument for fountain coding is that one transmission serves every peer, and §7.3 broadcasts responses so the first asker answers everyone. But it means the unicast paths (`WHO_IS`, delta requests, `want-repair`) depend on Meshtastic key exchange having happened between the two radios, and a link that finds unicast failing should fall back to broadcast rather than retrying into silence. **The governor must price that fallback**, since a broadcast reply costs the commons R times what it costs us.

**A trace hook, not a guess.** "Federation is not working" has too many identical-looking causes — wrong channel, wrong portnum, an unattributable sender, our own echo, a firmware duplicate drop — so the link can report every packet it is handed and what became of it. That is what turned a week's worth of plausible theories into two facts.

**The frame type byte is shared between L0 and L1.** The layer above already spends byte 0 on a frame type, so the link reads that byte rather than prepending one of its own: `1` control, `2` symbol, `3` `ANNOUNCE`, `4` `WHO_IS`. A second header byte would cost one byte per *symbol* — about 15 per bundle — to buy layering purity, which §12.7's byte budget does not have room for. The MTU available to the sync protocol therefore stays the full 233 bytes.

**Reliability.** Meshtastic offers `want_ack` with limited firmware-level retries, but it is not a reliable transport and shouldn't be treated as one. Use `want_ack` only for small unicast control packets (delta requests) and let L1 handle bulk reliability. Also respect the hop limit (0–7, default 3) — **set it explicitly and as low as the topology allows**, since hop limit is a direct multiplier on R (§1.1) and therefore on the airtime cost of everything we send.

### 7.2 L1 — fountain coding `[D1]`

**Decided: erasure coding from the start, not ARQ.** The reasoning is the broadcast asymmetry: a mesh broadcast reaches every listening BBS at once and each one misses a *different* random subset. Under ARQ, N receivers send N different NACK sets and the sender retransmits the union, so cost grows with peer count — at up to 50 peers `[D2]` that is exactly the wrong scaling. Under a fountain code the sender emits encoded symbols and each receiver decodes as soon as it has collected any K+ε; one transmission serves everyone and there is no NACK traffic at all. On a link where a retransmit round-trip costs minutes, removing the round-trip entirely is worth more than the coding overhead.

**But an off-the-shelf RaptorQ is the wrong tool here, and this needs saying plainly.** Our block sizes are tiny — a typical bundle is 1–10 symbols, rarely more than 30. Classic LT/Raptor overhead figures (~5%) assume K in the hundreds to thousands; at K < 20 the degree distributions those codes rely on behave poorly and overhead can exceed 20%. Additionally, RFC 6330 RaptorQ carries Qualcomm IPR declarations, which is worth a licensing check before it ends up in an MIT-licensed binary. `github.com/google/gofountain` is a reasonable reference but should be validated at our K, not assumed.

**The design that actually fits:**

```
byte 0     : version(2b) | type(3b) | flags(3b)
bytes 1-4  : bundle_id (uint32, random per bundle; scoped by Meshtastic sender)
byte 5     : symbol_id      (0..K-1 = systematic original; ≥K = repair symbol)
byte 6     : K (symbol count of the source block)
byte 7     : symbol_size_hint / extended id high bits
            → 8-byte header, 225 bytes of payload per symbol
```

1. **Systematic prefix.** Symbols `0..K-1` are the original fragments, sent in order. A receiver with no loss decodes at **zero coding overhead** — the common case on a good link costs us nothing, which is the property pure fountain schemes give up.
2. **Repair symbols on demand.** Symbols `K, K+1, …` are XOR combinations of the source symbols. The combination for symbol `i` is derived by seeding a PRNG with `(sender, bundle_id, i)`, so **the mask is never transmitted** — both ends compute it. Note the `sender` in that tuple: two nodes can collide on a `bundle_id`, so decoder state must be keyed by the Meshtastic source address as well, or their decodes silently corrupt each other.

   > **`bundle_id` is derived from the bundle's content, not drawn at random.** An earlier draft specified "a random uint32 chosen independently by each node". That livelocks under an airtime governor, and the simulator caught it: when the governor interrupts a transmission partway, a random ID makes the retry a *different block*, so every symbol receivers already hold becomes worthless. Measured at a 0.1% duty cycle — **30 simulated days, 43 minutes of airtime, zero records delivered**, while four peers sent ~700 vector requests each and got nothing. Taking `bundle_id = BLAKE3(packed_bundle)[:4]` makes an interrupted transmission **resumable**: the same records are the same block, symbols accumulate across attempts, and the sender continues from a cursor rather than restarting. The same run then converged in 58 seconds of channel time. This is not an optimisation — without it, a node whose budget is tighter than one bundle never converges at all. Degree is drawn from a **uniform-half** distribution: each source symbol is included with probability ½, giving expected degree K/2.

   > **Corrected from "heavy on degree 2–3".** That intuition is correct for *belief-propagation* decoding, which needs sparse rows to find a degree-1 equation to peel. We decode by Gaussian elimination (item 3), which wants the opposite — dense rows are far more likely to be linearly independent of what the decoder already holds. Measured at K=40: the sparse distribution needed **14.41** symbols beyond K on average; uniform-half needs **1.72**. At mesh airtime that difference is minutes per bundle.
3. **Decoding** is belief propagation with Gaussian-elimination fallback over GF(2). At K ≤ 64 that is a 64×64 bit matrix — microseconds, and a few dozen lines.
4. **How many repair symbols to send** is a governor decision, not a protocol constant. α starts at the observed mesh loss rate and adapts from the digest cycle (peers' high-water marks reveal whether bundles are landing). No NACKs in the steady state; a peer still stuck after a full digest cycle can unicast a `want-repair(bundle_id, count)`.

   > **`want-repair` is a common path, not a last resort** — an earlier draft of this section called it the latter, and measurement says otherwise. The formula below sizes for *one* receiver's decode probability (~2% failure). The chance that **every** receiver decodes is that raised to the audience size, and it falls off fast — measured at K=10, 20% loss: **97%** for a single receiver, **79%** across ten, **29%** across fifty. At the 50-instance scale of `[D2]`, roughly two thirds of broadcasts will leave *somebody* short.
   >
   > This is the right trade rather than a defect. Sizing so that fifty receivers all decode would inflate every transmission for every peer on the channel, to spare one straggler a unicast. But it means `want-repair` must be engineered as a routine, rate-limited, airtime-budgeted path — not an exception handler.

   The count itself is **not** `ceil(αK) + 1`. That formula under-provisions badly, because it accounts for neither the repair symbols being lost at the same rate as everything else, nor the fact that loss is random and half of all receivers do worse than the mean. Measured at K=10 and 20% loss it sent 13 symbols and **5 of 12 receivers decoded**.

   What we send instead is the smallest `N` satisfying

   ```
   N(1-α) - z·sqrt(N·α·(1-α))  ≥  K + ε        with ε = 3.4, z = 1.8
   ```

   solved by search (N appears on both sides). ε is the codec's own overhead — the symbols needed beyond K because some arrivals are linearly dependent — and it is **flat in K only for K ≥ 5**:

   | K | 1 | 2 | 3 | 4 | ≥5 |
   |---|---|---|---|---|---|
   | ε | 0 | 0.9 | 2.0 | 2.5 | 3.4 |

   **K = 1 is computed exactly rather than approximated.** Every symbol of a one-symbol block decodes it, so the question is only how many copies make it improbable that all are lost: the smallest `n` with `α^n ≤ 0.02`. The Gaussian machinery above is a normal approximation to a binomial and overshoots badly at tiny `n`. Treating K=1 like K=40 charged **16 symbols for a one-symbol payload at 50% loss** where 6 suffice — a 3× airtime bill on precisely the bundles item 5 says dominate a BBS. At fifty nodes the simulator showed this alone putting the federation at 5.4% of the channel, over the §1.1 budget; fixing it brought the same run to **2.3%**. It was first set to 1.6, the measured *mean*; that was the wrong statistic, because the overhead is long-tailed (p90 = 4, p95 = 5, p99 = 7) and sizing to the mean leaves half the receivers short of it. The constants are fitted to a measured failure curve (K ∈ {5,10,15,20,40} × α ∈ {5…50}%, 3000 trials each, smallest count holding failure under 2%); they clear all thirty cells while overshooting by 14 symbols in total.

   Note that **z came down from 2.0 as ε went up**. The margin was never missing, it was charged to the wrong term — and widening the binomial interval to cover a shortfall that was really codec overhead costs far more airtime, because that term scales with N while ε does not.

   Cost, at R=4 on LongFast for a 3 KB bundle (K=15), in seconds of channel time:

   | Loss | Symbols | Channel time | vs. payload alone |
   |---|---|---|---|
   | 0% | 16 | 1m 54s | 1.11× |
   | 10% | 24 | 2m 52s | 1.66× |
   | 30% | 34 | 4m 03s | 2.36× |
   | 50% | 50 | 5m 58s | 3.47× |

   This table is why α must adapt rather than be set pessimistically: assuming 50% loss on a link that is actually clean triples the airtime bill for every peer on the channel.

   **Against ARQ.** Modelling ARQ properly — transmit `K`, then retransmit the union of what anyone still lacks, with retransmissions subject to the same loss, repeating until everyone is whole — at K=10 and 15% loss:

   | Receivers | ARQ | Fountain | Ratio |
   |---|---|---|---|
   | 1 | 12 | 20 | **0.60×** |
   | 5 | 21 | 20 | 1.05× |
   | 20 | 24 | 20 | 1.20× |
   | 50 | 29 | 20 | 1.45× |

   The fountain cost is flat in audience size — one transmission serves everyone — while ARQ grows with it. Note the crossover: **at a single receiver ARQ genuinely wins**, and unicast repair to one peer is the better strategy. `[D1]` is a claim about broadcast to many, and holds from about five receivers up.
5. **K = 1 is a special case** — a single-packet bundle needs no coding, just optional blind repeats. Most DMs and most small post batches land here, and the code path should be trivially short.

This is an LT code with a small-K-tuned distribution and a systematic prefix. Writing it ourselves is a few hundred lines, avoids the IPR question, and lets us tune the distribution against the simulated mesh harness (§10) — which is exactly why the codec is built in **Phase 2**, where the harness lives, not in Phase 3 alongside the radios.

### 7.3 L3 — replication and anti-entropy

**Version vectors.** For each area, a node tracks `{origin_node → highest_contiguous_seq}`: 8 bytes of node ID + ~2 bytes of varint seq = **10 bytes per known origin per area**. At 50 instances `[D2]` that is **500 bytes per area** — three mesh packets — and across 10 federated areas, 5 KB. (Key-derived IDs cost 4 bytes per entry more than v0.3's packed addresses here; unlike the record header, a version vector has no repeated-origin structure to hoist. It changes nothing, because full vectors never go on the mesh routinely — see the digest design below — but it is the one place the reversal genuinely costs bytes.)

For 50 peers, version vectors remain the right answer. Merkle trees, IBLTs, and range-based reconciliation pay off at thousands of peers with heavily diverged state; at 50 they add machinery without reducing bytes. **But 3 KB of full vectors cannot go on the mesh routinely**, which drives the digest design below.

#### The digest storm — a problem the 50-node answer creates

v0.1 proposed a digest broadcast every 15–30 minutes. At 50 instances this is fatal. A 100-byte digest costs 1.01 s of local TX; with R = 4 that is ~4 s of mesh airtime; 50 nodes doing that every 30 minutes is:

| Instances | Digest interval | Mesh airtime consumed |
|---:|---:|---:|
| 5 | 30 min | 1.1 % |
| 20 | 30 min | 4.5 % |
| **50** | **30 min** | **11.2 %** |
| 50 | 120 min | 2.8 % |

**11% of the channel for control traffic that carries no content** — more than the entire 5% budget, before a single post is sent. Four mitigations, all required:

1. **Digests never carry full version vectors.** A digest carries, per federated area, `{area_tag(4) | rolling_hash(4) | count(2)}` = 10 bytes. Ten areas = 100 bytes, one packet. Full vectors are exchanged **unicast, on demand**, only when a rolling-hash mismatch proves divergence.
2. **Interval scales with peer count.** `interval = base × max(1, N/5)`, clamped so control traffic stays under a configured share of the mesh budget (default 1% of the 5% ceiling, i.e. 20% of our allocation). Take whichever is larger.

   At 50 nodes that lands at **about 5 hours**, not the 2–3 an earlier draft claimed — this section's own table contradicted that text, since 50 nodes at a 120-minute interval consume 2.8% of the channel, nearly triple the 1% control budget set a paragraph earlier. Measured intervals: 5 peers → 30 min, 20 → 2.0 h, 50 → 5.0 h, 100 → 10.0 h.

   > **The two rules are nearly the same function**, which is worth knowing before tuning either. Both are linear in N: the heuristic contributes `base/5` = 6m 0s per peer, and the clamp requires `digest_airtime/share` = 5m 54s per peer for a 103-byte digest at R=4. They differ by 2%, so the interval is effectively set by the clamp alone and small changes in digest size decide which wins. Only the clamp has physical meaning — it is derived from bytes, airtime and the flood multiplier rather than chosen — so it is the one to reason about.

   A multi-hour heartbeat sounds alarming and is not: anti-entropy is a *safety net*, not the delivery path. Content propagates by opportunistic push, digests piggyback on any bundle already in flight, and the standalone digest is only the idle-node heartbeat. What the interval bounds is how long a node that has been silent **and** has heard nothing waits before announcing itself.
3. **Piggyback.** Any bundle we're already sending carries the digest in its header. A node with normal traffic almost never needs a standalone digest packet — which means the standalone digest is genuinely just the idle-node heartbeat.
4. **Suppression.** If we hear a digest from a peer whose rolling hashes match ours across all shared areas within the last interval, skip our own — it would carry no information. On a converged mesh this collapses digest traffic to near zero.

5. **Reply suppression (added in v0.9).** The four mitigations above control the *digest*, and leave the reply path unguarded: one broadcast digest heard by fifty peers who are all behind produces fifty simultaneous unicast requests to one node. That is the digest storm wearing a different hat, and the reply is bigger than the digest that triggered it.

   So a peer waits a random fraction of a request window before asking, and **drops its request entirely if the answer arrives first**. Because responses are broadcast (cycle step 4), the first peer to ask answers everyone and the rest fall silent without ever transmitting. This is classic multicast repair suppression, and the reason the reply storm never forms.

6. **Control messages fit one packet, by construction.** `MaxAreas` and the delta-request size are *derived* from the MTU rather than chosen. A control message spanning packets must be fragmented, so it can arrive partially and need its own repair — a request that itself requires reliable delivery, which is the recursion this whole no-session design exists to avoid.

   Note that a delta request must be bounded in **bytes, not ranges**: this section budgets "~10 bytes per range", which holds only while sequence numbers are small. The origin is 8 fixed bytes but the two varints grow — 14 bytes per range at 2²⁰ records, 16 at 2⁴⁸. A count-based limit fits one packet in a young area and silently overflows in a mature one, which is a fragmentation bug that surfaces only after months of deployment.

**The gossip cycle, revised:**

1. **Opportunistic push (the primary path).** New local posts are batched (default 15–30 min, governor-gated) and broadcast as a bundle with a piggybacked digest. This is how content actually propagates.
2. **Digest heartbeat (the safety net).** Scaled interval, jittered, suppressed when redundant, skipped when piggybacked.
3. **Delta request (unicast).** A peer noticing a rolling-hash mismatch requests full vectors, then `(area, origin, from_seq, to_seq)` ranges. ~10 bytes per range, `want_ack` set.
4. **Bundle push (broadcast).** The holder packages requested records and broadcasts — other lagging peers benefit for free, which is the same broadcast-economy argument as the fountain code.

Batching is not optional at these budgets. One packet per post wastes the fixed header on every post and burns 2 s of airtime for a 40-character message. Accumulating 15–30 minutes of posts into one bundle amortizes framing across records and lets zstd find cross-message redundancy.

**Measured, at the `[D2]` scale.** Fifty instances on a simulated LongFast mesh at 15% loss, ten of them publishing, R = 4:

| | |
|---|---|
| Time to full convergence | 3 h 20 m |
| Channel utilisation | **2.3%** (budget: 5%) |
| Standalone digests, whole mesh | 13 |
| Digest beats suppressed | 224 — **95%** |
| Vector requests needed | 1 |

The suppression figure is the headline: on a mesh that is mostly converged, 95% of scheduled digest beats carry no information and are never sent. Opportunistic push reaches nearly everyone, so the reconciliation path — vector exchange, delta request, bundle push — is almost never used. That is the intended shape: the safety net should be idle.

**Measured publish ceiling.** Six nodes, LongFast, R = 4, 15% loss, against §1.1's 5% federation budget measured over a rolling hour:

| Records/hour, federation-wide | Peak channel utilisation |
|---:|---:|
| 1 | 1.39% |
| 2 | 2.04% |
| 4 | 3.38% |
| **6** | **4.72%** |
| 8 | 6.04% — over budget |
| 12 | 8.71% — over budget |

**About six records per hour, for the entire federation** — roughly 144 a day, shared across every instance and every area. That is the number to design the BBS around, and it is small enough to be worth stating plainly rather than discovering after deployment. Note the 1.39% floor at one record per hour: anti-entropy and per-record framing cost something even when almost nothing is being said.

It also explains why batching (below) is not optional. The figures above are for one record per bundle; amortising framing and letting zstd work across a batch is what moves the ceiling.

**Failure behaviour:** a node offline for a week comes back, broadcasts a digest showing stale rolling hashes, and peers backfill it. There's no session, no handshake, no state machine that can wedge. This is the property that makes anti-entropy the right choice over a FidoNet-style polling session — mesh links are too flaky for sessions. (The FTN gateway does use sessions, but only on its IP side; §7.7.)

### 7.4 L2 — compression with a trained dictionary

This deserves its own section because it is the highest-leverage optimization available — and at the §1.1 budget it is closer to mandatory than optional.

Generic zstd on a 400-byte forum post gets maybe 1.3–1.5× — there isn't enough data for the compressor to build a model. **zstd with a pre-trained dictionary** on the same post typically gets 3–5×, because the dictionary already contains common English fragments, BBS-specific vocabulary, quoting conventions (`> `), signature patterns, and structural boilerplate.

At ~10 originated packets/day/node, tripling effective throughput is the difference between 25 posts/day and 8. It is free range and we should take all of it.

Implementation:
- Ship a versioned dictionary (`dict_v1`, a few tens of KB) embedded in the binary.
- Dictionary ID goes in the bundle header (1 byte). Nodes announce which dictionaries they hold in their digest.
- Train it (`zstd --train`) on a corpus of real BBS/Usenet/forum text. **If the FTN gateway ships** `[D14]`, train on FTN echomail too — kludge lines (`SEEN-BY`, `PATH`, `MSGID`) are extremely repetitive and dictionary-compress beautifully.
- Refresh in later versions; old dictionaries stay supported.
- Compress the *bundle*, not individual records, so cross-record redundancy is captured.
- **Hard-cap the decompressed output.** A 233-byte packet can expand to gigabytes; zstd decompression bombs are a trivial attack for anyone holding the channel PSK, and §8.1 says to assume everyone does. Set an absolute output limit (bundle size ceiling × a small factor), enforce it in the streaming decoder rather than after the fact, and treat a breach as a peer-quota violation worth alerting on. This is fuzz-tested in §12.5.

### 7.5 File catalogs, not files `[D8]`

Restating the §1 conclusion as an enforced rule, because it will be tempting to violate:

- `FILE` records replicate metadata only (~120–200 B compressed).
- The UI shows network-wide file listings with a "held by" indicator.
- **The mesh `Link` implementation rejects file payloads.** This is a type-level constraint, not a config option: `FILE_DATA` is not in the set of record types the mesh link will serialize. There is no sysop flag to turn it on and no size threshold below which it's allowed.
- Fetch paths: (1) direct IP from a holding BBS, (2) queued for the next sneakernet exchange. That's it.
- Be honest in the UI: "held by `pnw` (`K7QM4X2P…`) — no IP route from here; queued for next exchange" is a feature, not a failure.

### 7.6 The airtime governor

The most important piece of civic infrastructure in the system, and the 50-instance answer `[D2]` makes it more important, not less.

- **Token bucket sized in *mesh* airtime-seconds.** Compute per-packet airtime from the active preset using the Semtech formula (Appendix A), then **multiply by the estimated flood multiplier R** to get the cost charged against the budget. Bytes are a bad proxy and local TX time is also a bad proxy (it ignores rebroadcast, §1.1). **Corrected in v0.15:** earlier drafts said bytes fail "because airtime is superlinear in payload at high SF". Appendix A's own numbers say the opposite, and the direction changes what the governor must do. Airtime is *affine* in payload — a large fixed cost plus a linear term — so per byte it is strongly SUBlinear: 436 ms for a one-byte payload against 9.3 ms/byte for a full one. A byte-denominated budget therefore does not under-price large packets, it wildly under-prices **small** ones: 233 bytes in a single packet costs 2.16 s, and the same 233 bytes sent one byte at a time costs 101 s. **Cost is charged per packet**, or a chatty protocol that stays inside a byte budget can still take the channel apart.
  - Estimate R from observed traffic: count distinct rebroadcasts of our own packets seen coming back, plus the node's neighbour table. Default to 4 before enough data exists. Expose it in the sysop status screen — a sysop watching R climb is a sysop who understands their mesh.
- **Configurable ceiling, expressed as a mesh-wide share, default 5%**, hard max enforced in code at 15%. The ceiling is what *the BBS network as a whole* should consume; each node's own allocation is `ceiling / expected_instance_count`, which the node learns from the `NODE` roster (§6.1). At 50 instances that is 0.1% each, ~21 s of local TX/day.
  - **Sysops must not have to compute this.** The first-run wizard and the status screen both show the derived figure in human terms: "your share: about 11 full packets/day, or 25 short posts."
- **Read the node's own telemetry.** Meshtastic reports `channel_utilization` and `air_util_tx`. Above ~25% channel utilization, back off exponentially; above ~40%, transmit nothing but already-queued DM traffic.
- **Respect regional duty cycle.** EU 433/868 is limited to 10% duty over a rolling hour, enforced by firmware. Don't fight it — track it locally so we queue rather than getting rejected.
- **Quiet hours** — optional sysop-configured windows of zero transmission.
- **Priority classes:** control (digests, delta requests, `NODE` and `SUCCESSION` records) > DM > forum posts > file catalog. Under backpressure, drop from the bottom. There is no mesh-file class because there are no mesh files `[D8]`.
- **Per-area sub-budgets** (§6.3) so one chatty area — or one FTN-bridged echo (§7.7) — cannot consume the node's entire allocation.
- **Per-peer inbound quotas** so a rogue or malfunctioning BBS can't flood us — and log/alert the sysop when a peer exceeds them.

### 7.7 The FTN gateway `[D14]`

FidoNet/FTN compatibility moves from non-goal to goal. The record log maps cleanly onto echomail and there is a live FTN scene to connect to. It is also the single most dangerous feature in the document for the airtime budget, so the constraints come first.

**The `[D9]` reversal costs this feature something, and it should be stated plainly.** v0.3's numeric addressing shared an address space with FTN, so the mapping was the identity function. With key-derived IDs it is not: the gateway must maintain an **explicit two-way mapping table** between FTN `zone:net/node[.point]` addresses and MeshBBS node IDs. In exchange, the gateway's own FTN address becomes honestly what it is — an address assigned by a FidoNet coordinator to *the gateway*, not a claim about the whole MeshBBS network's numbering. That is arguably the more truthful arrangement: we are a foreign network with a gateway into FTN, not a pretend FTN zone. The mapping table is a Phase 6 deliverable and lives in the database with the rest of the peer state.

**Constraints, non-negotiable:**

- **The gateway is IP-side by default.** A gateway instance carries FTN echoes into its *local* message base and to *IP-linked* peers. Bridging an echo onto the mesh is a separate, explicit, per-area opt-in.
- **Any mesh-bridged echo gets a hard per-area airtime sub-budget** (§6.3) and a message-rate cap. For scale: the whole network originates ~1,200 short posts/day (§1.1). A single busy FTN echo can exceed that by itself. Over-budget messages are dropped at the gateway with a logged counter, not queued indefinitely.
- **The wizard must refuse to bridge an echo to mesh without showing the projected airtime** based on the echo's observed message rate over the preceding week.

**Mapping:**

| FTN | MeshBBS |
|---|---|
| Echomail message (FTS-0001 packed, `.PKT`) | `POST` record |
| Netmail | `DM` record |
| `MSGID` / `REPLY` kludges | record `id` / `parent` |
| `SEEN-BY` / `PATH` lines | dedup is by content-addressed `id`; SEEN-BY is generated outbound, consumed inbound for loop prevention |
| Echo tag | area name (and its 4-byte `area` tag) |
| `zone:net/node.point` | **explicit mapping table at the gateway** — see below |

**The gateway is a trust boundary, and this is the part to get right.** FTN echomail is plaintext, unsigned, and carries no cryptographic origin. Records entering from FTN cannot be signed by their true author, so:

- The **gateway node signs them with its own key**, and they are marked `via_ftn` with the original FTN origin address and `MSGID` preserved in the body.
- The UI must display them as gateway-attested, not author-attested: "from `joe@1:234/5` via gateway `fido-gw` (`M2X9F0QL…`)". Users need to know the trust chain is different.
- Loop prevention needs both mechanisms: content-addressed dedup handles the mesh side, `SEEN-BY`/`PATH` handles the FTN side. A message that round-trips FTN → mesh → FTN must not re-enter as new. Test this explicitly in the harness with a deliberately cyclic topology.
- Outbound, only areas the sysop has explicitly marked as FTN-exportable go out, and node-signed provenance is necessarily lost on the way — flag it in the export config so nobody is surprised.

Phase 6, after the wire format freeze. Gatewaying into a format that is still changing is how you generate angry FidoNet sysops.

### 7.8 Measuring R, the flood multiplier `[N6]`

R is currently a guess (4), and the entire airtime budget scales linearly with it — if the real value is 8, every budget in this document is twice as generous as it should be. It is also the one parameter in the design that **cannot be derived, simulated, or reasoned about** — it is a property of a specific mesh's RF topology at a specific moment. It has to be measured on real hardware.

#### 7.8.1 The observable that makes this easy

The measurement looks hard (how do you count transmissions you can't see?) but there is a clean trick. Meshtastic device telemetry reports two figures from the same node:

- **`air_util_tx`** — percentage of time *this node* spent transmitting.
- **`channel_utilization`** — percentage of time *the channel* was busy, as observed by this node.

If our node is the only originator during a measurement window, then everything on the channel that isn't our own transmission is a rebroadcast of our transmissions. So:

```
R ≈ (channel_utilization_load − channel_utilization_baseline) / air_util_tx_load
```

The baseline subtraction is what makes this work on a live community mesh rather than requiring an empty band: measure the channel's ambient busy-ness while transmitting nothing, then measure again while transmitting a known load, and attribute the difference to your own packets and their echoes. Note the result is R as observed *from your node's position* — nodes at the edge of a mesh see a different R than nodes in the middle, which is the correct thing to measure, since it is your node's traffic you are budgeting.

#### 7.8.2 Manual procedure, no software required

This can be done today with the stock Meshtastic CLI and app, before any MeshBBS code exists — and it is worth doing early, because the answer may change the roadmap's budget assumptions.

1. **Pick a quiet window** — a weekday small hours, ideally. Note the preset and confirm `hop_limit`.
2. **Baseline, 30 minutes.** Transmit nothing. Sample device metrics every minute (`meshtastic --info`, or the app's telemetry screen) and record `channel_utilization`. Take the mean and the spread — the spread matters, since a noisy baseline means a noisy R.
3. **Load, 30 minutes.** Generate a known, steady load. The **Range Test module** is purpose-built for this and is the least effort; a scripted `meshtastic --sendtext` loop with a fixed-size payload works too. Aim for roughly 1–2% `air_util_tx` — enough to rise above baseline noise, low enough to stay a good neighbour. Record `channel_utilization` and `air_util_tx` per sample.
4. **Compute** R from the formula above. Expect a range, not a number.
5. **Sweep `hop_limit`** — repeat at 1, 3, and 5. This is the most actionable output: R should climb steeply with hop limit, and it tells you directly what `hop_limit` costs the commons (§7.1 already says to set it as low as topology allows; this is how you find out what "as low as" means).
6. **Cross-check, optional.** Enable serial debug logging on the node and count duplicate receptions of your own `packet_id`s. Firmware dedups before the client API, so the debug log is where duplicates are visible. Where firmware exposes `hop_start` (2.2+), `hop_start − hop_limit` on a received packet gives hops travelled, which distinguishes a direct hearing from a relayed one.

**Two ways to get better data if you have the hardware:** a second node you control, parked elsewhere and logging everything it hears, turns the estimate into a partial census of who actually rebroadcast. And Meshtastic's discrete-event simulator (`meshtasticator`) can do sensitivity analysis — feed it a plausible topology, vary node density and hop limit, and see how hard R moves. The simulator cannot tell you *your* R, but it can tell you how much R varies, which is what determines how conservative the default should be.

#### 7.8.3 What `meshbbs mesh-survey` does

The subcommand automates the above and is worth building because it turns a fiddly hour into one command, and because it produces comparable numbers across sysops.

- **Runs without a BBS.** It needs only the node and the Phase 3 transport layer — no database, no SSH server, no config beyond the device connection. Someone curious about hosting can run it before committing to anything, which makes it a genuine adoption on-ramp as well as a diagnostic.
- **Automates baseline → load → compute**, with the load phase self-governed: it obeys the airtime governor's own ceiling, defaults to a conservative ~1% duty, refuses to run if ambient `channel_utilization` is already high, and aborts if it rises mid-test (someone else needs the channel more than we need the measurement).
- **Sweeps `hop_limit`** across 1/3/5 and reports the curve.
- **Builds a rebroadcast census** by logging every received packet with `packet_id`, `hop_start`/`hop_limit`, `rx_snr`, `rx_rssi`, and `from`, so repeat receptions and relay identities are visible where firmware allows.
- **Reports in the units the sysop actually needs**, not just a number: estimated R with a confidence interval, direct-neighbour count, channel utilization by hour-of-day, and — the important one — **the derived budget**: "at R = 5.2 and a 5% mesh ceiling shared 50 ways, your instance can originate about 8 full packets/day, roughly 20 short posts." That is the same human-terms figure §7.6 requires the wizard to show, computed from measurement instead of a default.
- **Writes a shareable report file.** Aggregating these across sysops is how the project eventually replaces the guessed default of 4 with a distribution grounded in real deployments, and how a new sysop gets a sane starting value for their region before running their own survey.

**The governor consumes the result.** `mesh-survey` provides the initial calibration written into config; §7.6's live estimation refines it continuously from observed traffic. Neither replaces the other — the survey gives a good starting value and a hop-limit decision, live estimation tracks drift as the mesh grows.

---

### 7.9 The IP link — TCP with mutually-authenticated TLS 1.3

*(Corrected in v0.11. Earlier drafts said "QUIC/Noise".)*

**The original specification was not implementable as written.** QUIC mandates TLS 1.3 as its handshake; there is no standard way to run a Noise handshake inside it. The two were named together because the *intent* was "no PKI" — no certificate authorities, no chain of trust to bootstrap, just two static keys authenticating each other.

TLS 1.3 delivers exactly that here, because this design already has the piece that usually forces PKI: **node IDs are self-certifying**. `ID = BLAKE3(ed25519_pubkey)[:8]` (§6.1.1), so a self-signed certificate whose key hashes to the expected node ID authenticates that node completely. There is no authority to consult and — the decisive part — **no extra key material to bind to the identity**. Noise would need a separate X25519 static key, and the binding from that key to the Ed25519 node ID would be new cryptographic design to write, review and get right. Here there is nothing to bind.

So the IP link is **TCP with mutually-authenticated TLS 1.3, self-signed Ed25519 certificates pinned to node IDs, and no third-party dependency at all**. QUIC's multiplexing, 0-RTT and connection migration would buy almost nothing over a protocol that sends a batch every 15–30 minutes (§7.3), and the standard library alone keeps cgo-free cross-compilation to all five targets trivially safe (§10).

Three properties worth stating explicitly, because each inverts something that is true on the mesh:

- **The certificate is a container, not a credential.** Nothing checks validity dates, issuer or subject. The key *is* the identity; expiry would imply a renewal process that cannot exist without an authority, and the recovery path for a compromised key is a `SUCCESSION` record (§6.1.6), not a CRL.
- **Broadcast costs N here, not 1.** On the mesh one transmission reaches everyone, which is the entire reason §7.2 chooses a fountain code over ARQ. Over IP a "broadcast" is N separate sends. `Caps().Broadcast` is therefore **false** — meaning *not free*, not *unavailable* — so a governor reading it knows the fountain economics do not apply and that unicast repair to one lagging peer is the cheaper move.
- **`Caps().Reliable` is true**, so L1's repair symbols are redundant over IP and a caller can skip them entirely.

**Inbound is default-closed.** A link with no configured allow list accepts *nobody*; a listener on a public port that accepts any node dialling it is an open relay for the federation. Outbound dials are not gated by the list, because naming a peer to dial is itself the authorization decision — the caller asked for that exact node and the handshake proved it got it.

## 8. Security and trust

### 8.1 Threat model

The mesh channel PSK is shared by every BBS on the network (and Meshtastic's channel encryption has known limitations). **Treat the mesh as a public broadcast medium.** Anyone with the channel key — which includes every participating sysop — sees all traffic. Design accordingly:

- Everything is **signed**, so content can't be forged or tampered with in transit.
- Forum posts are **public by definition** — no confidentiality expectation, and the UI should say so.
- DMs are **end-to-end encrypted** so the shared channel key is irrelevant to them.
- **DM metadata is not protected.** `[D7]` Other sysops can see that `austin@K7QM4X2P…` sent a DM to `joe@M2X9F0QL…`, and when. This is a deliberate, documented trade — it buys immediate bounces and per-recipient spam filtering. The user-facing docs must state it plainly rather than letting people assume otherwise from the phrase "end-to-end encrypted."

### 8.2 DM encryption and key custody `[D6]` `[D7]`

**Encryption.** Per-user X25519 keys (paired with their Ed25519 identity). A DM body is sealed to the recipient's public key:

```
ephemeral_pubkey (32B) || ChaCha20-Poly1305(body) || tag (16B)   = 48B overhead
```

48 bytes is **21% of a mesh packet** — real, but acceptable, and it's the floor for anything with forward-secrecy properties. Do *not* use `age`'s file format here (header, armor, and stanza overhead run to ~200 bytes); use `nacl/box` or an equivalent minimal construction directly. DMs are **user-signed** `[D5]`, unlike forum posts — volume is low and non-repudiation between individuals is worth the bytes.

**Key custody — client-held keys are the goal, and there is a real tension to resolve.** `[D6]` The stated preference is client-held keys. The difficulty is that the product is an SSH terminal session: if the server never holds the key, the server cannot render a DM, and "read your mail" stops being something the BBS can do. Three tiers, shipped in this order:

| Tier | Who can read DMs at rest | UX cost | Phase |
|---|---|---|---|
| **1. Server-held** | Sysop (technically) | none | 1 |
| **2. Passphrase-wrapped server-held** | Nobody at rest; server sees the key only in RAM during a session | one passphrase prompt per session | 1 |
| **3. Client-held** | Only the user | needs a local helper | 5 |

- **Tier 2 is the v1 default**, and it is a much better default than v0.1's tier 1. The user's X25519 private key is stored wrapped by an Argon2id-derived key from a passphrase they type at login (separate from, or reused from, their login password by choice). At rest the sysop has ciphertext. During a session the plaintext key is in server memory — unavoidable if the server renders the message, and it must be stated honestly in the docs and at signup.
- **Tier 3 is a standalone local helper**, `meshbbs-key`. `[N3]` The user pipes DM ciphertext through it; the private key never touches the server. The `ssh-agent`-derived alternative (deriving X25519 from a deterministic Ed25519 signature over a domain-separation string) was considered and **rejected**: it is a cleverer construction with better UX, but it depends on agent forwarding being available and on a homebrew derivation that would need real cryptographic review before it protects anyone's mail. The helper is boring, obviously correct, and reviewable in an afternoon — the right trade for the one component whose failure mode is silent.
- **Design now so tier 3 stays possible:** the server must never *require* the private key for anything except decrypting for display. Key discovery, DM addressing, signature verification, and delivery must all work from public keys alone. Getting this boundary wrong in Phase 1 is what would make tier 3 a rewrite rather than an addition.

**Key discovery:** `PROFILE` records replicate nick + node address + X25519 pubkey + signature (~100 B). A network-wide user directory that fits comfortably in the airtime budget. First-contact verification is trust-on-first-use, with an optional short fingerprint users can compare out of band.

### 8.3 ⚠️ Encryption and amateur radio `[D11]`

If a sysop runs their node in Meshtastic's **ham mode** (`is_licensed=true`, which unlocks higher power on amateur allocations), they operate under **FCC Part 97**, which **prohibits messages encoded for the purpose of obscuring their meaning**.

**Decided: hard-block, with a documented override flag.** Specifically:

- On detecting `is_licensed=true` from the node config, the BBS **refuses to send or relay encrypted DMs** and says so clearly in the sysop log, the status screen, and to any user who tries to compose one.
- Override is a config key with a self-documenting name (`i_accept_part97_responsibility = true`) plus a startup banner that repeats the warning every launch. Not a quiet flag.
- **The consequence v0.1 missed: the channel PSK is also encryption.** In ham mode the `bbsnet` channel itself must run **unencrypted** (PSK disabled), not merely with DMs disabled. The startup check must validate the channel config, not just the DM path, and refuse to start with an encrypted channel in ham mode.
- **Signing is fine.** Ed25519 signatures authenticate without obscuring meaning, so the record log, `NODE` records, and forum federation all operate normally under Part 97. Only confidentiality is the problem. Say this in the docs — sysops otherwise assume the whole system is unusable in ham mode.
- On the default ISM allocations (US 902–928 MHz under Part 15, EU 868 MHz), encryption is fine and this whole section is inert.

Sysops should not stumble into an FCC violation because our defaults were convenient.

### 8.4 Inter-BBS trust

- **Peer allowlist.** A sysop adds peer node IDs. Because an ID *is* the key (§6.1.1), allowlisting an ID is allowlisting a key — there is no pairing to get wrong and no way for a different key to answer to that ID. Unknown-origin records are quarantined for review by default, which is friendlier to network growth than dropping.
- **Display-name spoofing** replaces address squatting as the residual naming attack: nothing stops a node calling itself `Fog City`, so the UI must always render a display name next to its short ID, and local aliases must never auto-bind from a remote suggestion (§6.1.4). The ID itself cannot be spoofed.
- **Per-peer quotas** on records/hour and bytes/hour.
- **Local moderation always wins.** Remote tombstones are **advisory and sysop-configurable** `[D6]` — auto-honouring is what users expect, so it is the default for areas where the tombstone's origin matches the post's origin, but a tombstone from a *different* node than the original author is never auto-honoured. That combination gives users the behaviour they expect while preventing a compromised node from nuking network-wide content.
- **No transitive trust.** If A trusts B and B trusts C, A does *not* automatically accept C's records — though A will happily *relay* them if it carries a shared area. Records are signed end-to-end, so relaying is safe regardless of trust.
- **The FTN gateway is an explicit exception** and must be visibly labelled as such (§7.7).

### 8.5 Local security

- Door sandboxing is the biggest risk surface — see §9.4.
- Rate-limit SSH auth; log and optionally auto-ban.
- DM key custody: see §8.2. The honest version of "the sysop can read your DMs" belongs at signup, not buried in a man page.

---

## 9. Door games `[D4]`

**Decided: legacy DOS doors are nice-to-have.** The modern door API is the product; DOS emulation is deferred to Phase 7 and may never ship. That is a large scope reduction and it changes §9.2 from a build item to a research note.

### 9.1 Tier 1 — modern doors (the primary and, for now, only path)

Define a clean contract and make it the first-class citizen:

- Door is any executable, any language
- Communicates over **stdin/stdout** (the BBS bridges the user's PTY)
- Session context via **environment variables** + a **JSON session descriptor** on a passed fd or temp file: user handle, real name, node number, time remaining, terminal size, ANSI capability, and a callback token
- Optional **local API socket** so doors can read session context, keep persistent state, and — subject to a sysop-granted capability — post to forums or send DMs, which is what makes inter-BBS door leagues possible (§9.5). Authority model in §9.1.1

Works identically on all three OSes. Encourages new doors in Go/Python/Node. This is where the project's leverage is, and with DOS deferred it gets the whole of Phase 4.

Since this is now the only door path, two things that were optional become important: **ship two or three reference doors** with the binary so the API has proof-of-life, and **document the contract as a spec** rather than leaving it implicit in the implementation.

#### 9.1.1 Door API authority `[N11]`

Doors are third-party binaries running on the sysop's machine on behalf of a remote user (§9.4), and this socket is the one channel through which a door reaches the rest of the BBS. Four capability levels; **the first three are always available, the fourth is granted per door by the sysop and is off by default.**

| Level | Capability | Grant |
|---|---|---|
| 1 | **`session`** — read-only context: handle, node number, time remaining, terminal size, ANSI capability, theme hints. No writes | always |
| 2 | **`state`** — a private key/value namespace scoped to `(door, user)` and `(door, global)`, quota'd. No cross-door or cross-user reach | always |
| 3 | **`announce`** — post to a designated area **as the door's own identity**, never as the user. Rendered as `TRADEWARS (door@K7QM4X2P…)`, so it is attributable and rate-limitable, and a sysop can point it at a local-only area so it never touches airtime | always, rate-limited |
| 4 | **`act_as_user`** — post or DM as the logged-in user. What makes inter-BBS leagues feel native, and also a door impersonating a person | **sysop grant, per door** |

Level 4 mirrors the `post_federated` gating from `[N7]` — same shape, same reasoning: the useful thing is available, the thing with blast radius is opt-in. Three rules attach to it:

- **Capabilities intersect, never escalate.** A door running `act_as_user` for a user who lacks `post_federated` cannot federate on their behalf. The effective permission is always the intersection of the door's grant and the user's own capabilities — otherwise a door becomes a privilege-escalation path around §6.7's gating.
- **Every action under level 4 is audit-logged** with the door name, the user, and the record ID produced.
- **The user is told.** A one-time notice the first time a given door posts or sends as them, not buried in a log the user will never read.

**Token mechanics** (the sub-questions `N11` raised, decided):

- **Issued per door *invocation***, not per session — launching a door twice yields two tokens.
- **Scoped to `(user, door, session, invocation)`** and invalidated when the door process exits or the session ends, whichever comes first.
- **Does not survive a door restart.** A restarted door gets a fresh token. The BBS launches the door, so this costs nothing and removes a whole class of stale-credential bugs.
- **Passed via the JSON session descriptor** on a passed fd or a 0600 temp file — **never in argv or the environment of a shared process**, both of which are readable by any other process on the box via `ps` or `/proc`.
- Transport is a Unix domain socket (named pipe on Windows) with filesystem permissions restricting it to the door's user account (§9.4).

### 9.2 Tier 2 — legacy DOS doors (Phase 7, deferred)

Recorded for future reference, not planned work. The classics (LORD, TradeWars 2002, Barren Realms Elite, Usurper) are 16-bit DOS binaries that talk to a COM port and read a dropfile.

| Approach | Platforms | Notes |
|---|---|---|
| **DOSBox-X subprocess** | Linux, macOS, Windows | Bridge `serial1=nullmodem server:127.0.0.1:PORT` to the session. The only genuinely cross-platform option. External dependency. Would be the choice if this is ever built. |
| **dosemu2** | Linux only | Better performance; `$_com1 = "virtual"`. FOSSIL drivers (BNU, X00) *interfere* and must not be loaded. |
| **Embedded x86 emulator (v86 via WASM)** | All | What ENiGMA½ does. In Go: embed `wazero` (pure Go, no cgo) and run v86. Preserves the single-binary story but is a large effort. |

**Dropfile generation** (`DOOR.SYS`, `DOOR32.SYS`, `DORINFO1.DEF`) is cheap and mostly independent of the emulator, so it can land opportunistically in Phase 4 if the modern-door work touches the same code.

### 9.3 Windows-specific plumbing

Windows has no `fork`/`exec` with a PTY in the Unix sense. Use **ConPTY** (Windows 10 1809+) via `github.com/UserExistsError/conpty` or similar to give doors a real console. Budget real time for this; it's where cross-platform door support usually breaks — and with DOS deferred, it's the *only* remaining hard part of door support.

### 9.4 Sandboxing

We're executing arbitrary binaries on the sysop's machine on behalf of remote users. Minimum bar:

- Dedicated low-privilege user account (documented, not enforced by us)
- Working directory confined per-door
- Resource limits: CPU time, memory, wall-clock, max concurrent instances
- Node locking — many doors assume single-instance-per-node
- Never pass user-supplied strings into a shell

### 9.5 Inter-BBS doors — the payoff

Classic BBS culture had InterBBS leagues (LORD inter-BBS wars, TradeWars leagues) that exchanged score/battle files over FidoNet. **The same federation bus works here.** A `DOOR_EVENT` record type carries game events between instances, and you get inter-BBS door leagues over LoRa mesh with no internet — a genuinely novel thing, nearly free once §7 is built.

**`DOOR_EVENT` records are node-signed**, consistent with `[D5]`. The question was whether crossing a trust boundary and driving game state on someone else's machine warrants something stronger — and it does not, for a reason worth writing down: the threat in an inter-BBS league is a **cheating sysop**, not a cheating user, and user-signing provides zero additional protection against that. Under tier-2 key custody (§8.2) the sysop's server holds the user's key material during a session and can produce a valid user signature at will. Node-signing is therefore both cheaper and *more honest about who is actually being trusted* — leagues rest on sysop-to-sysop trust, exactly as they did in the FidoNet era. A league that wants integrity guarantees against its own members needs game-level design (commit-reveal, cross-checked state), not a signature layer.

One caveat from §1.1: `DOOR_EVENT` sits at the bottom of the priority order with forum posts, and a chatty game will notice. Design door events to be small and batched, and give them their own sub-budget like an area.

---

## 10. Cross-platform and packaging

- **Single static binary per platform, `CGO_ENABLED=0` everywhere.** With BLE dropped `[D13]` there are **no build tags, no cgo variants, and no split release artifacts** — one build matrix, one set of binaries. This is a real simplification the BLE decision bought.
- **Targets:** linux/amd64, linux/arm64 (Raspberry Pi — likely the most common deployment), darwin/arm64, darwin/amd64, windows/amd64.
- **Data layout:**
  ```
  <datadir>/
    config.toml
    bbs.db              # SQLite
    keys/               # node + host keys (0600)
    blobs/ab/cd/...     # content-addressed files
    themes/             # built-in themes, extracted for reference only
    doors/              # door installs
    logs/
  ```
  Default datadir follows OS convention (`~/.local/share/meshbbs`, `~/Library/Application Support/MeshBBS`, `%APPDATA%\MeshBBS`), overridable.
- **Service integration:** systemd unit, launchd plist, Windows Service wrapper. Generate them with a `meshbbs install-service` subcommand.
- **First-run wizard:** generate the node key (**the ID falls out of it — nothing to choose, request, or register**), pick a display name, scan for a Meshtastic device, configure the channel, show the derived airtime allocation in human terms (§7.6), pick a registration mode (§6.7), create the sysop account. Writes a *minimal* `config.toml` (§11.2), not a commented dump. Should take under two minutes.
- **Testing — see §12**, which covers the strategy in full. The headline is the **deterministic simulation harness**: the `Link` abstraction lets us run N in-process BBS instances on a virtual clock over a fake link with configurable MTU, latency, loss, and flood multiplier, with every source of nondeterminism driven by one seed. You cannot iterate on a sync protocol by physically deploying radios. Three things get validated there specifically:
  - the fountain code's overhead at small K (§7.2),
  - digest suppression and scaling at N = 50 (§7.3),
  - FTN loop prevention on a deliberately cyclic topology (§7.7).

  Meshtastic's own discrete-event simulator (`meshtasticator`) can validate RF-level assumptions and calibrate R later.

---

## 11. Configuration and administration

### 11.1 The split: file vs. database

The most consequential decision here, and the one that's expensive to change later. **Not everything configurable belongs in `config.toml`.**

> **`config.toml` holds what must exist before the process can serve a single connection.**
> **The database holds content and operational state that sysops edit while it's running.**

| In `config.toml` | In the database |
|---|---|
| Node address, node name, sysop identity | Users, capabilities, bans |
| Listener addresses, ports, host key paths | Forum areas and their policies |
| Meshtastic transport and channel | Federation peers and their keys |
| Airtime governor limits | Door registrations |
| Policy *defaults* (registration mode, new-area defaults) | MOTD, bulletins, ANSI art |
| Logging, datadir, environment | Per-user preferences |

The test is simple: if a sysop would plausibly change it from inside the TUI while users are online, it's database. If getting it wrong means the process shouldn't start, it's file. Putting the area list in TOML would mean a restart to create a forum, which no sysop will accept; putting the listen port in the database means a corrupt database is unrecoverable.

**But sysops want their config in git**, and that's legitimate. So: `meshbbs config export --all > backup.toml` dumps *both* layers into one annotated file, and `meshbbs config import` applies it. That gives version-controllable, diffable, reviewable configuration and reproducible harness setups without forcing the runtime to read areas from a text file.

### 11.2 Precedence and secrets

Resolution order, lowest to highest:

```
built-in defaults  →  config.toml  →  MESHBBS_* env vars  →  command-line flags
```

Every setting has a default that works. **The wizard writes a minimal file** — a dozen lines, not a 400-line commented dump. `meshbbs config reference` prints the full annotated schema (generated from struct tags, so it cannot drift from the code), and the same generator produces `docs/config.md`.

**Secrets never sit in `config.toml` as literals.** The Meshtastic channel PSK in particular is a shared secret. Any secret-valued key accepts three forms:

```toml
psk = "env:MESHBBS_MESH_PSK"      # from environment
psk = "file:keys/mesh.psk"        # from a 0600 file
psk = "base64:..."                # literal — permitted, warned about at startup
```

Private keys are never in config at all; they live in `keys/` at 0600, and the process refuses to start if permissions are looser than that.

### 11.3 Validation and reload

- **`meshbbs config check` is a first-class command**, exits non-zero with precise messages, and the generated service units run it as a pre-start step. Better to fail to start than to start wrong.
- **Startup refuses invalid config** rather than silently falling back to defaults. A typo'd key is an error, not a shrug — silently ignoring `airtime_ceiling_pct` because someone wrote `airtime_ceiling_percent` is how a mesh gets flooded.
- **Cross-field validation belongs here**, not scattered through the code. Specifically: the ham-mode checks from §8.3 (if `is_licensed` on the node, then DM encryption must be off *and* the channel PSK must be unset), governor ceiling ≤ the code-enforced hard max, per-area sub-budgets summing to ≤ the node's allocation, and telnet enabled without `guest_only` producing a loud warning.
- **Hot-reloadable** on `SIGHUP` / `meshbbs reload`: MOTD, theme selection, registration mode, rate limits, governor ceiling and quiet hours, area policies, log level.
- **Requires restart:** listener addresses and ports, datadir, node address, Meshtastic transport selection, database path. The reload command names exactly which changed keys it could not apply, rather than pretending it applied them.

### 11.4 The rule that protects the commons

**A knob whose wrong value harms other people on the mesh does not get to be a plain config value.** The governor's ceiling is configurable up to a hard maximum compiled into the binary (§7.6); the flood multiplier can be overridden only for testing and logs a warning every startup when it is; mesh file transfer is not configurable at all because it does not exist as a code path (§7.5). This is a deliberate asymmetry: sysops get wide latitude over their own instance and narrow latitude over shared airtime.

### 11.5 What is configurable

Grouped as the reference doc will be. Phase markers indicate when the group first appears.

**Instance identity — Phase 0**
`node_display_name`, `sysop_name`, `sysop_contact`, `datadir`, `timezone`, `environment` (`development` | `production` — gates `dev` subcommands, §6.7). **There is no `node_id` key** — it is derived from `keys/node.ed25519` and is read-only in every surface; `meshbbs id` prints it. `aliases` (database): local petname → node ID map (§6.1.4)

**Listeners — Phase 1**
- SSH: `bind`, `port` (default 2222 unprivileged, 22 documented), `host_key_path`, `auth_methods`, `max_sessions`, `max_sessions_per_user`, `idle_timeout`, `login_grace`
- Telnet: `enabled` (default **false**), `bind`, `port`, `guest_only` (default true when enabled), warning acknowledgement flag `[D12]`
- Web `[D16]`: `enabled` (default **false** — it needs a public origin and a TLS certificate, neither of which has a sensible default), `bind`, `port` (default 8443), **`origin` (REQUIRED when enabled, no default — passkeys bind to it and a mismatch fails totally, §5.3)**, `tls_cert`, `tls_key`, `max_sessions`, `max_sessions_per_user`, `idle_timeout_mins`, `unlocked_idle_timeout_mins` (shorter than `idle_timeout_mins` on purpose — such a session holds the mail passphrase in memory, and a closing browser tab is a far less reliable goodbye than an SSH disconnect), `session_ttl_hours`, `enrolment_code_ttl_mins`, `enrol_attempts_per_hour`, `auth_attempts_per_hour`

**Users and registration — Phase 1** (§6.7)
`registration_mode` (`open` | `approval` | `invite` | `closed`, default `open` `[N7]`), `guest_enabled`, `guest_areas`, `nick_min_len`, `nick_max_len`, `extra_reserved_nicks`, `collect_real_name`, `show_real_name`, `default_capabilities` (note: **excludes `post_federated`** `[N7]`), `new_user_post_delay`, `session_time_limit`, `dormant_after`, `dm_key_custody` (`server` | `wrapped` | `client`, default `wrapped`), `default_directory_listed` (default **true** `[N9]`), `password_min_len`, `argon2_params`. **No email key exists** `[N8]`

**Rate limits and abuse — Phase 1**
`auth_attempts_per_ip_per_hour`, `registrations_per_ip_per_day`, `max_pending_accounts`, `auto_ban_threshold`, `auto_ban_duration`, `ban_list` (database)

**Forums — Phase 1**
Per-area (database): `name`, `description`, `moderators`, `read_acl`, `post_acl`, `retention_days`, `retention_max_records`, `federated` (default **false** `[D8]`-adjacent — sysops opt in to airtime), `peer_nodes`, `airtime_share`, `ftn_export`
Defaults for new areas (file): `new_area_federated = false`, `new_area_retention_days`

**Files — Phase 1**
`sftp_enabled`, `blob_path`, `max_upload_size`, `per_user_quota`, `area_paths`, `catalog_federated`

**Themes — Phase 1** `[D15]` `[N5]`
`default_theme`, `theme_dir` (default `<datadir>/themes`, scanned for `*.toml` style overrides), `allow_user_theme_override`, `default_encoding` (`cp437` | `utf8` | `auto`)

**Federation — Phase 2**
`enabled_links`, `peers` (database: node ID — which *is* the key, §6.1.1 — plus local alias, allowed areas, quotas, `trust` = `accept` | `quarantine` | `reject`), `succession_policy` (default **auto-follow** `[N2]`; `confirm` available for sysops who want the prompt), `batch_window`, `digest_base_interval`, `quarantine_policy`, `tombstone_policy` (§8.4), `dictionary_version`, `ip_link` (bind/port/peer allow list)

**Mesh and the governor — Phase 3**
- Transport: `mode` (`serial` | `tcp` | `auto`), `serial_device`, `serial_baud`, `tcp_host`, `tcp_port`
- Channel: `channel_name`, `channel_index`, `psk` (secret-valued, §11.2), `port_num`, `hop_limit`
- Governor: `airtime_ceiling_pct` (default 5, hard max 15 in code), `expected_instance_count` (else derived from the `NODE` roster), `flood_multiplier` (default 4; set from a `mesh-survey` report, §7.8, then refined live), `flood_multiplier_override` (testing only), `quiet_hours`, `duty_cycle_region`, `backoff_thresholds`, `priority_order`
- Safety: `ham_mode_override` (`i_accept_part97_responsibility`) `[D11]`

**Doors — Phase 4**
Per-door (database): `name`, `path`, `args`, `cwd`, `env_passthrough`, `dropfile_type`, `max_concurrent`, `node_lock`, `cpu_limit`, `mem_limit`, `wall_clock_limit`, `required_capability` (what a *user* needs to run it), `api_level` (1–3 by default; `act_as_user` is level 4 and must be set explicitly, §9.1.1), `announce_area`, `announce_rate_limit`

**FTN gateway — Phase 6** `[D14]`
`uplink` (address, packet paths, session credentials), `echo_map` (FTN tag ↔ local area), `mesh_bridged_echoes` (explicit opt-in list), `per_echo_daily_cap`, `export_areas`, `origin_line`

**Logging and observability — Phase 0**
`level`, `format` (`text` | `json`), `file`, `rotate_size`, `rotate_keep`, `metrics_bind` (Prometheus, off by default), `audit_log` (auth events, sysop actions, peer quota violations)

### 11.6 Administration surfaces

Three, deliberately overlapping:

1. **CLI** — built on **`spf13/cobra`** (§4), which fixes the shape of every binary in the project: nested commands, consistent flag parsing, generated help, and `meshbbs completion bash|zsh|fish` for free. Works with the server stopped, which is what makes it the recovery path. Non-interactive and scriptable.

```
meshbbs
├── serve                      run the BBS (default when invoked bare)
├── id                         print this node's ID — base32 and words (§6.1.4.2)
├── init                       first-run wizard
├── config  check | export | import | reference
├── user    add | list | grant | revoke | approve | passwd | delete
├── invite  new | list | revoke
├── area    add | list | set | federate
├── peer    add | list | remove | alias
├── door    add | list | test
├── mesh    survey | status | send-test          (§7.8)
├── install-service
├── reload
└── dev     seed | login-token                   (refuses when environment = production)
```

Cobra is used for command and flag structure only — **not Viper for configuration**, for the reason in §4: §11.3 requires an unknown config key to be a hard startup error, which Viper cannot express and which the TOML decoder gives directly.
2. **Sysop TUI** — reachable from an authenticated session with the sysop capability. Covers the database layer: users, areas, peers, bans, MOTD, and a **live status screen** showing sessions, mesh link state, peer high-water marks, governor budget consumed, and the observed flood multiplier R (§7.6). A sysop watching R and their airtime share is a sysop who understands their mesh.
3. **Config file** — the file layer, plus `config export/import` for the database layer.

Anything destructive (delete user, purge area, remove peer) requires confirmation and writes to the audit log regardless of surface.

---

## 12. Testing and quality

Unit tests are assumed and not discussed. This section is about the rest — and the reason it needs its own section is that **MeshBBS is a distributed system whose transport is slow, lossy, broadcast, and physically inaccessible.** You cannot debug a sync protocol by deploying radios: a single failed reconciliation takes minutes to observe, the failing condition is a specific interleaving of loss and timing, and reproducing it means recreating RF conditions you do not control.

The strategy that follows is ordered by how much bug-finding it does per unit of effort.

### 12.1 Deterministic simulation is the centrepiece

Everything else is supporting cast. The `Link` abstraction (§3) and seeded `dev seed` (§6.7) exist to enable this: **run the whole network in one process, on a virtual clock, with every source of nondeterminism driven by a single seed.**

- N instances in-process, connected by a simulated link with configurable MTU, latency, loss, reordering, duplication, and flood multiplier.
- **Virtual clock.** No `time.Sleep`, no wall-clock waits. A 30-day soak runs in seconds, and a 3-hour digest interval (§7.3) is testable at all — which it is not against a real clock.
- **One seed controls everything**: delivery order, which fragments drop, when nodes crash, clock skew per node, message scheduling. A failure prints its seed; rerunning that seed reproduces it exactly.
- Failures shrink automatically: on a red run, re-run with the same seed and fewer nodes/events until it stops failing.

**This imposes four constraints that must be adopted in Phase 0**, because none is retrofittable once the codebase has grown:

| Constraint | Why |
|---|---|
| Clock is injected everywhere (`Clock` interface), never `time.Now()` in domain code | Virtual time is the whole point; one stray `time.Now()` makes a test flaky forever |
| Randomness is injected, never package-level `math/rand` or `crypto/rand` in logic paths | Same seed must produce the same run |
| The sync engine is a **single-threaded event-driven state machine**, not a web of goroutines | Goroutine scheduling is the one nondeterminism Go will not let you seed. Concurrency belongs at the edges (transports, sessions), never in the reconciliation core |
| No map iteration where order is observable (§6.2.1) | Same reason, and it also corrupts signatures |

This is the FoundationDB/TigerBeetle approach. It is more discipline up front than most projects want, and for this project it is the difference between "we found the reconciliation bug" and "sometimes messages go missing and we don't know why."

### 12.2 Property-based tests

Several components have properties that are far more valuable than examples. Use `pgregory.net/rapid` or `testing/quick`.

| Component | Property |
|---|---|
| Fountain codec (§7.2) | For random K and random loss patterns, **any** K+ε distinct symbols reconstruct the bundle exactly |
| Fountain codec | Overhead at small K stays under a stated bound — this is the assumption `[D1]` rests on, so assert it rather than hoping |
| Version vectors (§7.3) | Merge is commutative, associative, and idempotent — the CRDT laws. If these hold, convergence follows; if they don't, no amount of integration testing will save you |
| Canonical encoding (§6.2.1) | Round-trip fidelity, **and** byte-identical output across 1000 encodes of the same record, across platforms and architectures |
| Compression (§7.4) | `decompress(compress(x)) == x` for arbitrary bytes, including adversarial ones |
| ID rendering (§6.1.4.2) | base32 ↔ bytes ↔ words all round-trip losslessly |
| Record IDs | Distinct content never collides in 16 bytes across large random corpora (sanity, not proof) |

### 12.3 Invariant checking under simulation

The simulator's real power is asserting global properties after every simulated event — things no single node can check about itself:

- **Convergence.** Given a connected network and a quiescent period, every node subscribed to area A holds an identical record set. This is *the* correctness property of the whole federation design.
- **Monotonicity.** No node's version vector ever decreases; no `seq` is ever reused (§6.2.1).
- **Immutability.** No record ID ever maps to two different bodies, anywhere, ever.
- **Airtime.** No node exceeds its governor budget over any sliding window — the commons guarantee from §7.6, checked continuously rather than trusted.
- **Signature integrity.** Every stored record still verifies after any migration, restart, or restore.
- **No orphan leaks.** Threading reparents correctly when a late parent arrives (§6.3); orphan buffers are bounded.

### 12.4 Fault injection

Driven by the same seed, layered onto simulation runs:

- Network partition and heal, including partitions that straddle a digest interval
- Node crash mid-bundle, mid-write, mid-succession
- Restore-from-backup — specifically the `seq` regression scenario in §6.2.1, which should be caught by the incarnation counter and must have a test proving it
- Clock jumps forwards and backwards; a node stuck at epoch 0
- Duplicate, reordered, and truncated delivery
- Disk full, database corruption, permissions changed underneath a running server
- A peer that goes silent for a month and returns

### 12.5 Adversarial testing and fuzzing

§8.1 says to treat the mesh as a public broadcast medium where every listener holds the channel key. That makes **every parser an attack surface reachable by an unauthenticated stranger**, which raises fuzzing from good practice to a requirement.

**Native `go test -fuzz` targets**, all seeded from corpora captured in simulation:

- Meshtastic protobuf framing and the `0x94 0xC3` stream reassembler
- L1 symbol header and fountain decoder — malformed K, absurd symbol IDs, symbols claiming a bundle that doesn't exist
- **L2 bundle decompression** — decompression bombs specifically (§7.4), plus truncated and corrupt zstd streams and bogus dictionary IDs
- Record deserialization and signature verification paths
- SAUCE metadata on `.ANS` files (§5.4) — binary metadata from art packs of unknown provenance
- TOML config and theme files (§5.4, §11.3)
- Dropfiles and door API JSON (Phase 4)
- **FTN packet parsing (Phase 6)** — parsing hostile mail from a network we do not control, arriving at a node that signs on its behalf. Highest-risk parser in the system

**A malicious-peer fixture**, as a first-class test double rather than an afterthought: a node that replays records, forges signatures, claims a bogus `SUCCESSION`, sends oversized and never-terminating bundles, floods beyond quota, tombstones other people's content, and squats display names. Every §8.4 defence should have a test in which this fixture attacks it and loses.

### 12.6 Wire-format conformance vectors

`[D10]` commits us to freezing the wire format publicly in Phase 6. That commitment needs teeth well before then:

- A checked-in corpus of `(input → canonical bytes → id → signature)` vectors, generated once and thereafter immutable.
- CI fails if the encoder produces different bytes for any vector. This turns "we accidentally changed the wire format" from a discovery made by an angry sysop into a red build.
- Cross-version tests once there is more than one format version, in both directions.
- The same corpus is what a future third-party implementation would test against, which is the actual point of registering a portnum.

### 12.7 The byte budget is a test, not a document

Appendix B is a budget, and budgets that live only in prose get exceeded. **Assert it in CI**: a canonical 10-record bundle must encode to ≤ N bytes; a single-record DM bundle to ≤ M. A change that quietly adds 8 bytes per record cuts network capacity by ~10% (§1.1) and would otherwise be invisible until deployment.

Related benchmark-regression gates, with thresholds rather than eyeballs:
- Compression ratio against a fixed corpus, so a dictionary change that helps one case and hurts overall gets caught
- Fountain overhead at K = 2, 5, 10, 30
- Decode time and memory at max bundle size

### 12.8 Terminal and TUI testing

BBS output is ANSI, and ANSI regressions are invisible to ordinary assertions.

- **Golden-frame tests** via `charmbracelet/x/exp/teatest`: render each screen at 80×24, 132×50, and a pathological 40×10, and diff against committed golden files.
- CP437 ↔ Unicode translation (§5.4) covered both directions, including the box-drawing and block glyphs that make or break BBS art.
- Theme loading (§5.4) — every built-in and every sample TOML renders without escape-sequence corruption.
- SAUCE-annotated art files render at their declared dimensions.

### 12.9 End-to-end and cross-platform

- **Real SSH client against the real server** (`golang.org/x/crypto/ssh` as the client): the `new@` signup flow, pubkey enrollment, multi-key login, capability gating, session limits, SFTP file operations. These are the flows §5.1 and §6.7 describe, and they are worth exercising through an actual protocol rather than an in-process shortcut.
- **Multi-instance integration** over the real IP link, not the simulator — catching everything the simulated `Link` abstracts away.
- **CI matrix across all 5 targets**, and the binaries must *run*, not merely compile. `CGO_ENABLED=0` is asserted, not assumed.
- **A real Windows runner is non-negotiable** for Phase 4: ConPTY (§9.3) is where cross-platform door support breaks, and it cannot be tested anywhere else.

### 12.10 Hardware in the loop

The residue that simulation cannot reach, because it is physics and firmware rather than logic. Nightly or pre-release, on a bench with two or more Meshtastic devices:

- Framing against **real firmware**, including a version-compatibility matrix — the protobuf schema moves, and we vendor it (`[D3]`)
- Serial disconnect/reconnect, device reboot mid-bundle, USB re-enumeration
- Measured airtime vs. Appendix A's computed figures — if these disagree, the governor is wrong
- `mesh survey` (§7.8) validated against a known two-node topology before it is trusted on a live mesh
- Regional duty-cycle enforcement behaving as §7.6 assumes

### 12.11 What each phase adds

| Phase | Testing added |
|---|---|
| **0** | The four determinism constraints (§12.1); config validation tests; CI matrix with runnable binaries |
| **1** | Golden-frame TUI tests; real-SSH end-to-end; registration and capability-gating flows |
| **2** | **The simulator itself**, plus property tests, invariant checking, fault injection, conformance vectors, byte-budget gates. The largest single investment, and it pays for Phase 3 |
| **3** | Hardware-in-the-loop; airtime validation; `mesh survey` ground truth |
| **4** | Windows/ConPTY runner; door sandbox escape attempts; dropfile fuzzing |
| **5–6** | FTN parser fuzzing and loop-prevention tests on a cyclic topology; cross-version conformance |

**The one thing to get right early:** §12.1's four constraints. Everything else in this section can be added later at normal cost. Those four cannot.

---

## 13. Roadmap

Revised per the decisions. The largest changes: the fountain codec moves **up** into Phase 2 (it needs the harness), DOS doors move **down** to Phase 7, BLE and theme packs are **gone**, and a new Phase 6 covers wire-format freeze plus the FTN gateway. **Account creation and the config loader are Phase 0**, before the SSH server exists, so there is something to test against from the first week.

| Phase | Scope | Why this order |
|---|---|---|
| **0 — Skeleton** | Go module, SQLite schema + migrations, **config loader + `config check` + generated reference**, logging, node key generation, **key-derived node ID + `NODE` record + sysop alias table**, **cobra command tree**, **`user add` / `user grant` / `dev seed` CLI**, **injected clock + seeded RNG + canonical-encoding discipline (§12.1, §6.2.1)**, CI cross-compiling all 5 targets cgo-free | Prove the cgo-free build works on day one. Identity lands here because everything signs against it; the alias table lands with it because `[D9]` makes petnames the only human-facing surface. Account CLI lands here because Phase 1 and 2 both need populated instances and neither can wait for a signup TUI. |
| **1 — Single-node BBS** | SSH server, **`new@`/unknown-nick signup TUI, open-plus-gated registration, capability grants, multi-pubkey enrollment**, Bubble Tea UI, menus, ANSI/CP437 rendering, **built-in themes + `themes/*.toml` loader**, **alias resolution in addressing**, **dual base32/word ID rendering**, local forums, local DMs with **passphrase-wrapped keys (tier 2)**, file areas via SFTP, presence/node chat, telnet off-by-default, **sysop TUI + status screen** | A genuinely usable BBS with zero federation. Ship this; get people on it. Tier-2 key custody is here because retrofitting key wrapping means re-keying every user, and the DM-key/password-reset interaction (§6.7) becomes unfixable once real mail exists. |
| **2 — Federation over IP + the harness** | Record log, version vectors, anti-entropy with **digest suppression/scaling**, bundle format, zstd dictionary, `Link` abstraction, **simulated mesh harness (seeded by `dev seed`)**, **fountain codec (L1)**, **lazy `PROFILE` publication**, **`SUCCESSION` records with auto-follow**, **the deterministic simulator + property/invariant/fuzz suites (§12)**, QUIC/Noise IP link | Build and debug the sync protocol where iteration is fast — but design every byte for the mesh MTU. The codec is here, not Phase 3, because tuning small-K overhead needs the harness's controllable loss. |
| **3 — Meshtastic link** | Serial + TCP transports, protobuf framing, **airtime governor with flood-multiplier accounting**, ham-mode safety checks (DMs *and* channel PSK), file catalog replication, **`mesh survey` (§7.8) + live R estimation** | The protocol already fits 233 bytes because Phase 2 was designed that way |
| **4 — Doors** | Modern door API + spec **incl. the §9.1.1 capability model**, PTY/ConPTY bridging, sandboxing, 2–3 reference doors, dropfile generation, **Windows CI runner (§12.9)** | Independent of federation; parallelizable with 2–3. Now the whole of door scope. |
| **5 — Reach** | ~~Web terminal~~ (**shipped early — see below**), inter-BBS `DOOR_EVENT`, sneakernet bundles, **`meshbbs-key` helper for client-held DM keys (tier 3)** | Genuinely optional, except tier-3 keys which are a stated preference |
| **6 — Interop & stabilization** | **Freeze BSMP wire format v1** + written spec + conformance vectors, **request Meshtastic portnum**, **FTN gateway** (echomail/netmail, SEEN-BY/PATH, per-echo airtime caps) | Both deliverables are public commitments and both need a format that has survived real radios. `[D10]` sequences the portnum after the freeze; §7.7 sequences the gateway after it too. |
| **7 — Legacy DOS doors** | DOSBox-X bridge, node locking, COM-port bridging | Nice-to-have `[D4]`. May never ship, and nothing depends on it. |

Phases 2 and 4 are parallelizable if there's more than one person working on it. Phase 6 is the first point at which the project makes promises to strangers, and should not be rushed toward.

**The web front end shipped out of order, during Phase 3.** It is recorded here rather than renumbered, because *why* it jumped is the useful part: as `xterm.js` it was a genuine Phase 5 luxury, but `[D16]` turned it into a refactor of the session layer — splitting `Screen` from its rendering — and that refactor is cheaper the earlier it happens and touches every screen written after it. It also arrived in four independently shippable steps, the first of which (screen extraction) carried all the structural risk and landed alone, with no web server anywhere near it and no user-visible change. Federation work was not displaced: Phase 3's remaining scope is unchanged and the mesh link is still the critical path.

---

## 14. Open questions

**All design questions are now answered** and recorded in §15.2 — `N1`–`N9` in v0.5, `N11` in v0.7. `N4` is fully closed (both renderings over the same 64 bits, 6 BIP-39 words and 13 Crockford characters, no sub-question outstanding). One item remains, and it is not a decision to make but a number to go and measure.

### Resolves by measurement, not discussion

**`N10` — What is R on a real mesh?** `[N6]` settled the *method*, not the *number*. §7.8 specifies how to measure the flood multiplier by hand with stock Meshtastic tooling, and what `mesh survey` automates. The design still runs on a guessed default of 4, and every airtime figure in this document scales linearly with it. Ideally measured before Phase 3 finalizes the governor's defaults. **If R turns out to be 8**, the per-node budget halves to ~5 full packets/day, the batching windows in §7.3 want lengthening, and the default `hop_limit` deserves a hard look — §7.8.2's hop-limit sweep is the part that tells you what to change.

### Answered — see §15.2

**`N12` — How does a node ID map to a radio address?** Raised and resolved during Phase 3: a signed `ANNOUNCE` binds them, `WHO_IS` discovers on demand, and no packet carries identity bytes. Full reasoning and the two rejected alternatives in §7.1.2.

**`N11` — How much authority does a door get?** Resolved: capability levels 1–3 always available, `act_as_user` a per-door sysop grant, capabilities intersect rather than escalate, tokens scoped per invocation, `DOOR_EVENT` node-signed. Full model in §9.1.1.

### Standing note

Nothing else blocks work. The remaining unknowns are ones implementation answers, not review: the fountain code's measured overhead at small K (Phase 2, §7.2, and §12.2 makes it an assertion rather than a hope), whether digest suppression actually converges at N = 50 in the simulator (Phase 2, §7.3), and whether the alias UX carries the weight `[D9]` put on it (Phase 1, §6.1.4). None can be settled on paper.

---

## 15. Decisions

### 15.1 Resolved v0.1 questions

| # | Question | Decision | Main sections affected |
|---|---|---|---|
| **D1** | ARQ or fountain codes? (was `Q1`) | **Fountain coding from the start.** Refined to a *systematic* LT code with small-K-tuned degrees and derived (untransmitted) XOR masks — off-the-shelf RaptorQ is a poor fit at K < 20 and carries IPR questions. | §7.2, §4, Phase 2 |
| **D2** | How many instances? (was `Q8`) | **~50 at most.** Version vectors stay; but this broke the digest cycle (11% of channel) and forced digest scaling/suppression, and it shrinks each node's share to ~10 packets/day. | §1.1, §7.3, §7.6 |
| **D3** | Own Meshtastic transport or a library? (was `Q3`) | **Vendor `meshtastic/protobufs`, roll our own transport.** | §4 |
| **D4** | Legacy DOS doors? (was `Q15`) | **Nice-to-have.** Modern door API gets all of Phase 4; DOS deferred to Phase 7. | §9, Phase 4/7 |
| **D5** | Node- or user-signed posts? (was `Q9`) | **Node-signed forums, user-signed DMs** (the v0.1 recommendation). | §6.2, §8.2 |
| **D6** | Honour remote deletes? (was `Q10`) | **Advisory, sysop-configurable.** Refined: auto-honour when the tombstone's origin matches the post's origin; never auto-honour a third-party tombstone. | §8.4 |
| **D7** | DM metadata privacy? (was `Q12`, part 1) | **Content confidentiality only.** Drops the hashed-recipient routing tag — DMs address `nick@NODEID` in the clear, enabling immediate bounces and spam filtering. Must be documented plainly. | §6.4, §8.1 |
| **D8** | Mesh file transfer? (was `Q11`) | **None at all.** Not a quota or a toggle — the mesh link refuses file payloads by type. Catalogs replicate; bytes move over IP or sneakernet. | §1, §6.5, §7.5 |
| **D9** | Node addressing? (was `Q7`) | **REVERSED in v0.4 — self-certifying key-derived IDs**, `BLAKE3(node_pubkey)[:8]`, with human-friendliness from a local petname/alias layer. Was numeric `zone:net/node` in v0.2–v0.3. Deletes the registry, the address authority, first-seen binding, and the whole squatting attack surface; costs typeability, which the alias table must carry. Net *cheaper* on the wire at bundle sizes ≥ 3 once `origin` hoists to a per-bundle table. Dissolves old `N1`/`N4`, re-scopes `N2`. | §6.1, §6.2, §8.4, §7.7 |
| **D10** | Register a portnum? (was `Q2`) | **Yes, but after the wire format is frozen and tested.** Versioning from day one; freeze + spec + conformance vectors + portnum request all in Phase 6. | §7.1, Phase 6 |
| **D11** | Hard-block encrypted DMs in ham mode? (was `Q14`) | **Hard-block with a documented override flag.** Extended: the channel PSK must also be disabled in ham mode, and signing remains legal and enabled. | §8.3 |
| **D12** | Telnet? (was `Q5`) | **Off by default, loud warning when enabled.** Guest-only telnet is the recommended middle setting. | §5.2 |
| **D13** | BLE? (was `Q4`) | **Dropped, future/never.** Makes every artifact cgo-free with no build tags — a real packaging simplification. | §2, §3, §4, §7.1, §10 |
| **D14** | FTN gateway? (was `Q13`) | **Yes.** Moves from non-goal to goal, Phase 6. The `D9` reversal means FTN↔MeshBBS addressing needs an explicit mapping table rather than being the identity function (§7.7). Constrained hard: IP-side by default, per-echo airtime caps, and explicitly labelled as a trust boundary since FTN mail is unsigned. | §2, §7.7, Phase 6 |
| **D15** | Theme customization? (was `Q6`) | **Small set of built-in themes**, colours behind a `Theme` struct. Extended by `[N5]`: a `themes/*.toml` style-override loader ships too; full ANSI art packs still declined. | §5.4, §15.2 `N5` |

---

### 15.2 Resolved v0.4 review questions

| # | Question | Decision | Main sections affected |
|---|---|---|---|
| **N1** | Alias machinery — scope and ownership | **Sysop-owned aliases only** (no per-user tables — too complex and confusing), **one-keystroke accept** of a peer's suggested name, and **aliases resolve in addressing**, not just display. Adds the resolution rule: aliases are expanded locally at compose time and **never appear on the wire**. | §6.1.4, §6.1.4.1, §11.5 |
| **N2** | Node key rotation | **Accept the `SUCCESSION` proposal with auto-follow.** Signed by the old key, self-verifying on the new pubkey. Guardrails added because auto-follow is powerful: always alert, tombstone the old ID at `effective`, one succession per predecessor, and a sanity window on `effective`. Lost keys still have no recovery path, stated plainly. | §6.1.6, §6.2, §7.6, §11.5 |
| **N3** | Client-held DM key mechanism | **Standalone `meshbbs-key` helper.** The `ssh-agent`-derived X25519 alternative is rejected — nicer UX, but a homebrew derivation guarding mail is the wrong place for cleverness. | §8.2 |
| **N4** | Node ID display | **Both renderings ship**, over the same 64 bits: Crockford base32 (13 chars) for typing, BIP-39 words for reading aloud. **Corrected from the question:** 6 words, not 4–5 — 64 bits needs 6 × 11 bits. | §6.1.4.2, §4 |
| **N5** | Theme loader without theme packs | **Yes** — read `<datadir>/themes/*.toml` and merge over built-ins. Scope boundary held: *style* overrides (colours, glyphs), not ANSI art template packs. Hot-reloadable. | §5.4, §11.5 |
| **N6** | How to measure R | **Method specified** (§7.8): baseline vs. load `channel_utilization` differenced against `air_util_tx`, swept across hop limits, doable by hand today with stock Meshtastic tooling. `mesh-survey` automates it, self-governs its own load, and reports the derived budget in human terms. The *number* remains open as `N10`. | §7.8, §7.6, §11.6, §12 |
| **N7** | Default registration mode | **`open`, with `post_federated` withheld** — open front door, gated commons. Accepted with the UI burden made explicit: area scope badges, read-only rendering of federated areas for ungranted users, and pending grants surfaced to the sysop. | §6.7, §11.5 |
| **N8** | Collect email? | **No.** No field, no column, no optional prompt. | §6.7, §11.5 |
| **N9** | Directory listing default | **Listed**, with a visible per-user toggle. Publication stays lazy — listed governs what happens on first federated activity, not at signup. | §6.7, §11.5 |
| **N11** | Door API authority | **Levels 1–3 (`session`, `state`, `announce`) always available; `act_as_user` a per-door sysop grant, off by default** — same shape as `[N7]`'s `post_federated` gating. Capabilities **intersect, never escalate**, so a door cannot federate on behalf of a user who lacks that right. Tokens are per-invocation, die with the process, and travel in the session descriptor rather than argv. `DOOR_EVENT` stays node-signed: the threat is a cheating sysop, and user-signing buys nothing against that under tier-2 key custody. | §9.1.1, §9.5, §11.5 |
| **N-tech** | CLI framework | **`spf13/cobra`** for all binaries — commands, flags, help, completions. **Viper explicitly not used**: §11.3 requires unknown config keys to be a hard startup error, which Viper cannot express. | §4, §11.6 |

### 15.3 Web front end — recorded in `webui.md`

`[D16]`–`[D18]` are decided and written up in **[webui.md §15](webui.md)**, which owns them. They are indexed here because they are referenced inline in this document and because §5.3 is a reversal, and the summary below is deliberately short: duplicating the reasoning in two files is how the two drift, which is the same argument `[D16]` itself makes about navigation models.

| # | Question | Decision | Sections affected |
|---|---|---|---|
| **D16** | What shape is the web UI? | **A semantic terminal, reversing §5.3's `xterm.js`.** The model emits a typed `Screen`; ANSI and HTML renderers both consume it. Same screens, same keys, same wording, rendered twice. Geometry moves from the model into the renderers, which is what makes the browser version better rather than a screenshot of a terminal. §2's "no web forum UI" non-goal is **narrowed, not reversed** — there is still no second navigation model. | §2, §5.3, §13 |
| **D17** | How do people authenticate on the web? | **Passkeys / WebAuthn only.** No password path, no SSH-key path, no guest browsing. Discoverable credentials, so no nick is typed. Two consequences accepted rather than discovered: the web has **no unauthenticated front door**, and **`web.origin` becomes a required key** because an RP ID mismatch fails totally rather than degrading. | §5.1, §5.3, §11.5 |
| **D18** | How does an account predating the web get its first passkey? | **A single-use, short-expiry, enrolment-only code minted by an authenticated SSH session.** It registers a credential and cannot mint a session. Note `webui.md` §8.1's correction: this does **not** make a stolen code harmless — whoever holds one can enrol *their own* passkey — so the properties carrying the weight are the ten-minute window and the authenticated issuance, not the endpoint shape. | §5.3, §6.7 |

---

## Appendix A — Airtime math

Semtech LoRa airtime, validated against Meshtastic's published figures:

```
Ts       = 2^SF / BW
DE       = 1 if Ts > 16.38 ms else 0                      # low data-rate optimization
Tpre     = (n_preamble + 4.25) * Ts                       # Meshtastic uses n_preamble = 16
n_pay    = 8 + max(ceil((8*PL - 4*SF + 28 + 16*CRC - 20*IH) / (4*(SF - 2*DE))) * (CR+4), 0)
Tair     = Tpre + n_pay * Ts
```

Validation: SF11 / BW250 kHz / CR 4/5 (LongFast), PL=16 → **354 ms**, exactly matching Meshtastic's documented figure. PL=256 → **2.157 s** vs. their stated "~2 s". PL=100 (a digest) → **1.010 s**; PL=233 (full app payload) → **1.993 s**.

Two things the governor must do with this:
- Budget in **airtime-seconds**, not bytes, and charge **per packet** — airtime is affine in payload, so the fixed preamble and header dominate small packets and a byte budget under-prices them by an order of magnitude (§7.6).
- **Multiply by R**, the flood multiplier (§1.1), to get the cost to the commons rather than the cost to us.

## Appendix B — Payload budget for a typical forum post

Revised for the derived-`id` change (§6.2) and the key-derived origin of `[D9]`. **Single-record bundle** — the worst case for v0.4, because the origin table's fixed cost has nothing to amortize against:

| Component | v0.1 | v0.3 (numeric) | **v0.4 (key-derived)** |
|---|---:|---:|---:|
| Meshtastic `Data` payload ceiling | 233 | 233 | 233 |
| L1 symbol header | −8 | −8 | −8 |
| L2 bundle header (dict id, area tag, base ts, count, flags) | −4 | −6 | −6 |
| Bundle origin table (1 origin) | — | — | **−9** (count + one 8 B ID) |
| Record `id` | −16 | 0 (derived) | 0 (derived) |
| `origin` per record | −8 | −4 (packed address) | **−1** (index into table) |
| `seq`, `ts` delta, `type` | −13 | −5 | −5 |
| `area` | −4 | 0 (hoisted) | 0 (hoisted) |
| `parent` (threaded replies only) | −16 | −16 | −16 |
| Ed25519 signature | −64 | −64 | −64 |
| **Remaining for compressed body** | **106** | **130** | **124** |
| → decompressed at 3.5× with trained dictionary | ≈ 370 chars | ≈ 455 chars | **≈ 434 chars** |

A top-level post (no `parent`) has 140 bytes for the body, ≈ 490 characters.

**Batching reverses the comparison**, which is the whole argument for `[D9]`'s wire encoding. Across a 10-record single-origin bundle the origin ID is paid once and per-record cost falls to `sig(64) + origin_idx(1) + seq/ts/type(5) = 70` bytes plus body — against 73 under numeric addressing. Total non-body overhead for 10 records: **875 B (v0.4) vs 896 B (v0.3)**. Break-even is 3 records; §7.3's 15–30 minute batching windows put normal traffic well past that.

**The signature is now ~90% of the per-record overhead.** That is precisely why `D5` (node-signed, not dual-signed) matters, and it is the only remaining target of any size: aggregate or bundle-level signatures would recover ~64 bytes per record, at the cost of losing per-record verifiability when records are relayed individually. Worth revisiting only if the budget gets tight — everything else has been squeezed.

---

## References

- [Meshtastic — Radio Settings (presets, data rates, link budgets)](https://meshtastic.org/docs/overview/radio-settings/)
- [Meshtastic — Client API (Serial/TCP), framing](https://meshtastic.org/docs/development/device/client-api/)
- [Meshtastic — LoRa Configuration](https://meshtastic.org/docs/configuration/radio/lora/)
- [Meshtastic — Mesh Broadcast Algorithm (managed flood routing / rebroadcast)](https://meshtastic.org/docs/overview/mesh-algo/)
- [Meshtastic — Encryption limitations](https://meshtastic.org/docs/about/overview/encryption/limitations/)
- [Meshtastic — Why your mesh should switch from LongFast](https://meshtastic.org/blog/why-your-mesh-should-switch-from-longfast/)
- [meshtastic/protobufs — mesh.proto](https://github.com/meshtastic/protobufs/blob/master/meshtastic/mesh.proto)
- [Port Numbers — PRIVATE_APP and the 256–511 range](https://deepwiki.com/meshtastic/protobufs/2.2-port-numbers)
- [Message Architecture — 233-byte payload limit](https://deepwiki.com/meshtastic/protobufs/2-message-architecture)
- [meshnet-gophers/meshtastic-go](https://github.com/meshnet-gophers/meshtastic-go)
- [lmatte7/goMesh](https://github.com/lmatte7/goMesh)
- [charmbracelet/wish — SSH apps](https://github.com/charmbracelet/wish)
- [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea)
- [google/gofountain — LT and Raptor codes in Go](https://github.com/google/gofountain)
- [RFC 6330 — RaptorQ (note the IPR declarations)](https://www.rfc-editor.org/rfc/rfc6330)
- [ENiGMA½ BBS — Features](https://enigma-bbs.github.io/features/)
- [ENiGMA½ — FidoNet-Style Networks (FTN)](https://nuskooler.github.io/enigma-bbs/messageareas/ftn.html)
- [tsali/dosemu2-bbs-doors](https://github.com/tsali/dosemu2-bbs-doors)
- [Synchronet wiki — DOS doors with dosemu](http://wiki.synchro.net/howto:dosemu)
- [FTSC — FidoNet technical standards](http://ftsc.org/docs/fsc-0070.002)
- [FTS-0001 — FidoNet packed message format](http://ftsc.org/docs/fts-0001.016)
