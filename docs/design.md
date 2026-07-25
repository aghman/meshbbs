# MeshBBS — High-Level Design

*A modern, cross-platform BBS in Go with SSH access, door games, file areas, forums, and DMs — federated between independent BBS instances over Meshtastic LoRa.*

**Status:** Draft v0.4 — addressing reversed to key-derived IDs
**Date:** 2026-07-24
**All 15 open questions from v0.1 are answered. Decisions are recorded in §14 and referenced inline as `[D#]`. New questions raised *by* those decisions are in §13.**

*v0.3 added account creation and registration (§5.1, §6.7) and configuration and administration (§11).*
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
- **User-authored ANSI theme packs.** A curated set of built-in themes only. `[D15]` — see §13 `N5`
- **DM metadata privacy.** Content confidentiality is required; hiding who-talks-to-whom is not. `[D7]`
- Real-time inter-BBS chat over mesh (latency is 10s of seconds to minutes; do it over IP)
- Web forum UI. A read-only web view might come later; SSH is the product.
- Legacy DOS door emulation in v1 — nice-to-have, deferred to Phase 7. `[D4]`

---

## 3. Architecture overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                          FRONT ENDS                                  │
│   SSH (primary)   Telnet (off by default)   WebSocket+xterm.js (opt) │
└───────────────────────────┬──────────────────────────────────────────┘
                            │  Session API (in-process, transport-agnostic)
┌───────────────────────────▼──────────────────────────────────────────┐
│                      SESSION / UI LAYER                              │
│   Bubble Tea models · menu graph · ANSI/CP437 renderer · themes       │
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
     │  (Meshtastic)   │ │ (QUIC+Noise)   │ │ (file bundle)  │ │ (echomail)    │
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
| Assets | `embed.FS` | Single-binary deployment with ANSI art, themes, migrations |

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

### 5.3 Web terminal (optional, later)

`xterm.js` over a WebSocket, same session API. Good for casual visitors and for showing the BBS off. Low priority.

### 5.4 Terminal rendering — the fiddly bit

BBS aesthetics mean **CP437 + ANSI art**, but modern terminals are UTF-8. Plan:

- Detect capability: `LANG`/`LC_ALL` env, terminal type, and a first-run user preference toggle.
- Store art as CP437 bytes; render through a translation layer that either passes bytes through (legacy client) or maps CP437 → Unicode box-drawing/block glyphs (modern client).
- Support SAUCE metadata on `.ANS` files (dimensions, font hints) — it's what art packs ship with.
- Handle window size: SSH gives us `pty-req` and `window-change`; Bubble Tea consumes these natively. Fall back to 80×24.
- **Themes: a small curated set, built in and embedded.** `[D15]` Ship perhaps four (classic 16-colour ANSI, a muted 256-colour, high-contrast/accessible, monochrome for serial-ish clients). No sysop-authored theme packs, no manifest format, no loader.
  - Design constraint to honour anyway: keep colour and glyph choices behind a `Theme` struct rather than hardcoding escape sequences at call sites. That costs nothing now and is the difference between "add theme packs later" being a weekend and being a rewrite. See §13 `N5`.

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

- **Display form.** Crockford base32 (no `I`, `L`, `O`, `U`, so no ambiguity and no accidental profanity): 8 bytes → 13 characters, shown grouped as `K7QM-4X2P-B9TFR`. Security-relevant surfaces (allowlist confirmation, fingerprint comparison) always show all 13. Everywhere else shows the first 8 characters, git-short-hash style, expanding on collision.
- **Self-declared display name.** Each node publishes one in its `NODE` record (`pnw-bbs`, `Fog City`). **Not unique, not authoritative, never used for routing** — purely a label. The UI always renders it alongside the short ID so a spoofed name is visibly attached to the wrong ID.
- **Local aliases are the everyday surface.** Each BBS keeps its own alias table, exactly like `~/.ssh/config` `Host` entries or `/etc/hosts`. The sysop (and optionally each user) maps `pnw` → `K7QM4X2PB9TFR`. Users then type `austin@pnw`, never the ID. Aliases are **local**, so two BBSes may disagree about what `pnw` means and neither is wrong — which is exactly why no registry is needed.
- **Alias suggestions propagate, bindings don't.** A `NODE` record's display name is a *suggestion* the local sysop may accept as an alias with one keystroke. Nothing auto-binds. This keeps the convenience of names without reintroducing a namespace to fight over.

Honest summary of the trade: **routing and trust become simpler and safer; typing an unfamiliar node ID becomes worse, and the petname table is what you're relying on to hide that.** If the alias UX is weak, this decision will feel bad in daily use. §13 `N1` and `N4` are re-scoped accordingly.

#### 6.1.5 User identity

An Ed25519 keypair, generated server-side at signup (with an option for the user to supply their own). A globally-addressable user is `nick@NODEID` — e.g. `austin@K7QM4X2PB9TFR`, or `austin@pnw` through a local alias. The pubkey is what matters for DM encryption; the nick is a display convenience and *is not globally unique*.

### 6.2 The record log — the heart of federation

Everything replicable is an **immutable, signed record** in a single append-only log:

```
Record {
  id        [16]byte   // BLAKE3(canonical_body)[:16] — DERIVED, NOT TRANSMITTED
  origin    [8]byte    // BLAKE3(node_pubkey)[:8] — HOISTED to a per-bundle origin table
  seq       varint     // per-origin monotonic sequence (~2B typical)
  ts        varint     // delta from the bundle's base timestamp (~2B typical)
  type      uint8      // POST, DM, PROFILE, NODE, AREA, FILE, TOMBSTONE, VOTE, DOOR_EVENT
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
- Store-and-forward: if the destination node isn't reachable, hold and retry with exponential backoff, with a TTL (default 7 days) after which we return a bounce to the sender.
- DMs sit above forum posts in the governor's priority order (§7.6) — they are the one class we keep transmitting under backpressure.

### 6.5 Files `[D8]`

- Local file areas with upload/download via SFTP over SSH, plus an in-TUI browser.
- Content-addressed blob store (BLAKE3 → `blobs/ab/cd/abcd...`), so identical files across areas dedup.
- **Over mesh, only the catalog replicates.** `FILE` records carry name, size, hash, description, tags, and holding node — roughly 120–200 bytes compressed. Users see the whole network's file list.
- **Mesh file transfer does not exist as a code path.** Not a quota, not a sysop toggle, not a "tiny files only" exception — the mesh link refuses `FILE_DATA` payloads outright. v0.1 proposed an 8 KB trickle option; it is removed. Fetch paths are exactly two:
  1. **Direct IP** from a holding BBS (QUIC/Noise link, or plain HTTPS if the sysop publishes one).
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
| Email | **no, and unverified** | See §13 `N8`. An off-grid BBS may have no path to send mail, so email must never be load-bearing for recovery |
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

**Recommended default: `open`, with federated posting off.** This decomposition matters and is the airtime-aware answer. A new user can immediately register, browse, post to local areas, and play doors — the classic open-BBS feel — but their posts do **not** consume the shared mesh budget until the sysop grants a `post_federated` capability. The door is open; the commons is gated. See §13 `N7` if you'd rather default to `approval`.

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
- A user may be **unlisted** (config-default and per-user): they can send off-node DMs but no PROFILE is published, so they aren't in the network directory. Receiving a reply still works because the sender's DM carries what's needed.
- Account deletion emits a `TOMBSTONE` for the profile; local-only accounts that never published need no tombstone.
- **Directory backfill is pull, not push.** A node that wants the full network user directory requests it; nobody broadcasts their whole roster.

See §13 `N9` on whether unlisted should be the default.

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
┌─ L4  Records ──────── POST / DM / PROFILE / NODE / FILE / TOMBSTONE …
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

The adapter auto-detects: scan serial ports for Meshtastic VID/PIDs, then try the configured TCP host.

**Reliability.** Meshtastic offers `want_ack` with limited firmware-level retries, but it is not a reliable transport and shouldn't be treated as one. Use `want_ack` only for small unicast control packets (delta requests) and let L1 handle bulk reliability. Also respect the hop limit (0–7, default 3) — **set it explicitly and as low as the topology allows**, since hop limit is a direct multiplier on R (§1.1) and therefore on the airtime cost of everything we send.

### 7.2 L1 — fountain coding `[D1]`

**Decided: erasure coding from the start, not ARQ.** The reasoning is the broadcast asymmetry: a mesh broadcast reaches every listening BBS at once and each one misses a *different* random subset. Under ARQ, N receivers send N different NACK sets and the sender retransmits the union, so cost grows with peer count — at up to 50 peers `[D2]` that is exactly the wrong scaling. Under a fountain code the sender emits encoded symbols and each receiver decodes as soon as it has collected any K+ε; one transmission serves everyone and there is no NACK traffic at all. On a link where a retransmit round-trip costs minutes, removing the round-trip entirely is worth more than the coding overhead.

**But an off-the-shelf RaptorQ is the wrong tool here, and this needs saying plainly.** Our block sizes are tiny — a typical bundle is 1–10 symbols, rarely more than 30. Classic LT/Raptor overhead figures (~5%) assume K in the hundreds to thousands; at K < 20 the degree distributions those codes rely on behave poorly and overhead can exceed 20%. Additionally, RFC 6330 RaptorQ carries Qualcomm IPR declarations, which is worth a licensing check before it ends up in an MIT-licensed binary. `github.com/google/gofountain` is a reasonable reference but should be validated at our K, not assumed.

**The design that actually fits:**

```
byte 0     : version(2b) | type(3b) | flags(3b)
bytes 1-4  : bundle_id (uint32, random per bundle)
byte 5     : symbol_id      (0..K-1 = systematic original; ≥K = repair symbol)
byte 6     : K (symbol count of the source block)
byte 7     : symbol_size_hint / extended id high bits
            → 8-byte header, 225 bytes of payload per symbol
```

1. **Systematic prefix.** Symbols `0..K-1` are the original fragments, sent in order. A receiver with no loss decodes at **zero coding overhead** — the common case on a good link costs us nothing, which is the property pure fountain schemes give up.
2. **Repair symbols on demand.** Symbols `K, K+1, …` are XOR combinations of the source symbols. The combination for symbol `i` is derived by seeding a PRNG with `(bundle_id, i)`, so **the mask is never transmitted** — both ends compute it. Degree is drawn from a distribution tuned for small K (heavy on degree 2–3, which is where small-K decoding actually succeeds).
3. **Decoding** is belief propagation with Gaussian-elimination fallback over GF(2). At K ≤ 64 that is a 64×64 bit matrix — microseconds, and a few dozen lines.
4. **How many repair symbols to send** is a governor decision, not a protocol constant: send `K` systematic symbols plus `ceil(αK) + 1` repair symbols, where α starts at the observed mesh loss rate and adapts from the digest cycle (peers' high-water marks reveal whether bundles are landing). No NACKs in the steady state; a peer still stuck after a full digest cycle can unicast a `want-repair(bundle_id, count)` as a last resort.
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
2. **Interval scales with peer count.** `interval = base × max(1, N/5)`, clamped so control traffic stays under a configured share of the mesh budget (default 1% of the 5% ceiling, i.e. 20% of our allocation). At 50 nodes that lands around 2–3 hours. This is fine: anti-entropy is a *safety net*, not the delivery path.
3. **Piggyback.** Any bundle we're already sending carries the digest in its header. A node with normal traffic almost never needs a standalone digest packet — which means the standalone digest is genuinely just the idle-node heartbeat.
4. **Suppression.** If we hear a digest from a peer whose rolling hashes match ours across all shared areas within the last interval, skip our own — it would carry no information. On a converged mesh this collapses digest traffic to near zero.

**The gossip cycle, revised:**

1. **Opportunistic push (the primary path).** New local posts are batched (default 15–30 min, governor-gated) and broadcast as a bundle with a piggybacked digest. This is how content actually propagates.
2. **Digest heartbeat (the safety net).** Scaled interval, jittered, suppressed when redundant, skipped when piggybacked.
3. **Delta request (unicast).** A peer noticing a rolling-hash mismatch requests full vectors, then `(area, origin, from_seq, to_seq)` ranges. ~10 bytes per range, `want_ack` set.
4. **Bundle push (broadcast).** The holder packages requested records and broadcasts — other lagging peers benefit for free, which is the same broadcast-economy argument as the fountain code.

Batching is not optional at these budgets. One packet per post wastes the fixed header on every post and burns 2 s of airtime for a 40-character message. Accumulating 15–30 minutes of posts into one bundle amortizes framing across records and lets zstd find cross-message redundancy.

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

### 7.5 File catalogs, not files `[D8]`

Restating the §1 conclusion as an enforced rule, because it will be tempting to violate:

- `FILE` records replicate metadata only (~120–200 B compressed).
- The UI shows network-wide file listings with a "held by" indicator.
- **The mesh `Link` implementation rejects file payloads.** This is a type-level constraint, not a config option: `FILE_DATA` is not in the set of record types the mesh link will serialize. There is no sysop flag to turn it on and no size threshold below which it's allowed.
- Fetch paths: (1) direct IP from a holding BBS, (2) queued for the next sneakernet exchange. That's it.
- Be honest in the UI: "held by `pnw` (`K7QM4X2P…`) — no IP route from here; queued for next exchange" is a feature, not a failure.

### 7.6 The airtime governor

The most important piece of civic infrastructure in the system, and the 50-instance answer `[D2]` makes it more important, not less.

- **Token bucket sized in *mesh* airtime-seconds.** Compute per-packet airtime from the active preset using the Semtech formula (Appendix A), then **multiply by the estimated flood multiplier R** to get the cost charged against the budget. Bytes are a bad proxy (airtime is superlinear in payload at high SF) and local TX time is also a bad proxy (it ignores rebroadcast, §1.1).
  - Estimate R from observed traffic: count distinct rebroadcasts of our own packets seen coming back, plus the node's neighbour table. Default to 4 before enough data exists. Expose it in the sysop status screen — a sysop watching R climb is a sysop who understands their mesh.
- **Configurable ceiling, expressed as a mesh-wide share, default 5%**, hard max enforced in code at 15%. The ceiling is what *the BBS network as a whole* should consume; each node's own allocation is `ceiling / expected_instance_count`, which the node learns from the `NODE` roster (§6.1). At 50 instances that is 0.1% each, ~21 s of local TX/day.
  - **Sysops must not have to compute this.** The first-run wizard and the status screen both show the derived figure in human terms: "your share: about 11 full packets/day, or 25 short posts."
- **Read the node's own telemetry.** Meshtastic reports `channel_utilization` and `air_util_tx`. Above ~25% channel utilization, back off exponentially; above ~40%, transmit nothing but already-queued DM traffic.
- **Respect regional duty cycle.** EU 433/868 is limited to 10% duty over a rolling hour, enforced by firmware. Don't fight it — track it locally so we queue rather than getting rejected.
- **Quiet hours** — optional sysop-configured windows of zero transmission.
- **Priority classes:** control (digests, delta requests, `NODE` records) > DM > forum posts > file catalog. Under backpressure, drop from the bottom. There is no mesh-file class because there are no mesh files `[D8]`.
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

---

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
- **Tier 3** ships a small `meshbbs-key` helper the user runs locally. Two viable mechanisms, to be chosen during Phase 5 (§13 `N3`): a local helper that the user pipes ciphertext through, or deriving the X25519 key from a deterministic Ed25519 signature over a fixed domain-separation string made by the user's forwarded `ssh-agent` — Ed25519 signatures are deterministic, so this yields a stable keypair the agent's holder alone can reproduce, without the private key ever leaving the agent.
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
- Optional **local API socket** so doors can post to forums, send DMs, and read/write persistent per-user state — which is what makes inter-BBS door leagues possible (§9.5)

Works identically on all three OSes. Encourages new doors in Go/Python/Node. This is where the project's leverage is, and with DOS deferred it gets the whole of Phase 4.

Since this is now the only door path, two things that were optional become important: **ship two or three reference doors** with the binary so the API has proof-of-life, and **document the contract as a spec** rather than leaving it implicit in the implementation.

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
- **Testing — the simulated mesh harness.** The `Link` abstraction lets us run N in-process BBS instances over a fake link with configurable MTU, latency, loss, **and flood multiplier**. This is essential; you cannot iterate on a sync protocol by physically deploying radios. It is also where three specific things get validated:
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
- Web terminal: `enabled` (default false), Phase 5

**Users and registration — Phase 1** (§6.7)
`registration_mode` (`open` | `approval` | `invite` | `closed`, default `open`), `guest_enabled`, `guest_areas`, `nick_min_len`, `nick_max_len`, `extra_reserved_nicks`, `collect_real_name`, `show_real_name`, `default_capabilities` (note: **excludes `post_federated`**), `new_user_post_delay`, `session_time_limit`, `dormant_after`, `dm_key_custody` (`server` | `wrapped` | `client`, default `wrapped`), `password_min_len`, `argon2_params`

**Rate limits and abuse — Phase 1**
`auth_attempts_per_ip_per_hour`, `registrations_per_ip_per_day`, `max_pending_accounts`, `auto_ban_threshold`, `auto_ban_duration`, `ban_list` (database)

**Forums — Phase 1**
Per-area (database): `name`, `description`, `moderators`, `read_acl`, `post_acl`, `retention_days`, `retention_max_records`, `federated` (default **false** `[D8]`-adjacent — sysops opt in to airtime), `peer_nodes`, `airtime_share`, `ftn_export`
Defaults for new areas (file): `new_area_federated = false`, `new_area_retention_days`

**Files — Phase 1**
`sftp_enabled`, `blob_path`, `max_upload_size`, `per_user_quota`, `area_paths`, `catalog_federated`

**Themes — Phase 1** `[D15]`
`default_theme`, `allow_user_theme_override`, `default_encoding` (`cp437` | `utf8` | `auto`)

**Federation — Phase 2**
`enabled_links`, `peers` (database: node ID — which *is* the key, §6.1.1 — plus local alias, allowed areas, quotas, `trust` = `accept` | `quarantine` | `reject`), `batch_window`, `digest_base_interval`, `quarantine_policy`, `tombstone_policy` (§8.4), `dictionary_version`, `ip_link` (bind/port/Noise static key path)

**Mesh and the governor — Phase 3**
- Transport: `mode` (`serial` | `tcp` | `auto`), `serial_device`, `serial_baud`, `tcp_host`, `tcp_port`
- Channel: `channel_name`, `channel_index`, `psk` (secret-valued, §11.2), `port_num`, `hop_limit`
- Governor: `airtime_ceiling_pct` (default 5, hard max 15 in code), `expected_instance_count` (else derived from the `NODE` roster), `flood_multiplier_override` (testing only), `quiet_hours`, `duty_cycle_region`, `backoff_thresholds`, `priority_order`
- Safety: `ham_mode_override` (`i_accept_part97_responsibility`) `[D11]`

**Doors — Phase 4**
Per-door (database): `name`, `path`, `args`, `cwd`, `env_passthrough`, `dropfile_type`, `max_concurrent`, `node_lock`, `cpu_limit`, `mem_limit`, `wall_clock_limit`, `required_capability`

**FTN gateway — Phase 6** `[D14]`
`uplink` (address, packet paths, session credentials), `echo_map` (FTN tag ↔ local area), `mesh_bridged_echoes` (explicit opt-in list), `per_echo_daily_cap`, `export_areas`, `origin_line`

**Logging and observability — Phase 0**
`level`, `format` (`text` | `json`), `file`, `rotate_size`, `rotate_keep`, `metrics_bind` (Prometheus, off by default), `audit_log` (auth events, sysop actions, peer quota violations)

### 11.6 Administration surfaces

Three, deliberately overlapping:

1. **CLI** — `user`, `invite`, `area`, `peer`, `door`, `config`, `install-service`, `dev`. Works with the server stopped, which is what makes it the recovery path. Non-interactive and scriptable.
2. **Sysop TUI** — reachable from an authenticated session with the sysop capability. Covers the database layer: users, areas, peers, bans, MOTD, and a **live status screen** showing sessions, mesh link state, peer high-water marks, governor budget consumed, and the observed flood multiplier R (§7.6). A sysop watching R and their airtime share is a sysop who understands their mesh.
3. **Config file** — the file layer, plus `config export/import` for the database layer.

Anything destructive (delete user, purge area, remove peer) requires confirmation and writes to the audit log regardless of surface.

---

## 12. Roadmap

Revised per the decisions. The largest changes: the fountain codec moves **up** into Phase 2 (it needs the harness), DOS doors move **down** to Phase 7, BLE and theme packs are **gone**, and a new Phase 6 covers wire-format freeze plus the FTN gateway. **Account creation and the config loader are Phase 0**, before the SSH server exists, so there is something to test against from the first week.

| Phase | Scope | Why this order |
|---|---|---|
| **0 — Skeleton** | Go module, SQLite schema + migrations, **config loader + `config check` + generated reference**, logging, node key generation, **key-derived node ID + `NODE` record + local alias table**, **`user add` / `user grant` / `dev seed` CLI**, CI cross-compiling all 5 targets cgo-free | Prove the cgo-free build works on day one. Identity lands here because everything signs against it; the alias table lands with it because `[D9]` makes petnames the only human-facing surface. Account CLI lands here because Phase 1 and 2 both need populated instances and neither can wait for a signup TUI. |
| **1 — Single-node BBS** | SSH server, **`new@`/unknown-nick signup TUI, registration modes, capability grants, multi-pubkey enrollment**, Bubble Tea UI, menus, ANSI/CP437 rendering, **built-in themes behind a `Theme` struct**, local forums, local DMs with **passphrase-wrapped keys (tier 2)**, file areas via SFTP, presence/node chat, telnet off-by-default, **sysop TUI + status screen** | A genuinely usable BBS with zero federation. Ship this; get people on it. Tier-2 key custody is here because retrofitting key wrapping means re-keying every user, and the DM-key/password-reset interaction (§6.7) becomes unfixable once real mail exists. |
| **2 — Federation over IP + the harness** | Record log, version vectors, anti-entropy with **digest suppression/scaling**, bundle format, zstd dictionary, `Link` abstraction, **simulated mesh harness (seeded by `dev seed`)**, **fountain codec (L1)**, **lazy `PROFILE` publication**, QUIC/Noise IP link | Build and debug the sync protocol where iteration is fast — but design every byte for the mesh MTU. The codec is here, not Phase 3, because tuning small-K overhead needs the harness's controllable loss. |
| **3 — Meshtastic link** | Serial + TCP transports, protobuf framing, **airtime governor with flood-multiplier accounting**, ham-mode safety checks (DMs *and* channel PSK), file catalog replication, R estimation | The protocol already fits 233 bytes because Phase 2 was designed that way |
| **4 — Doors** | Modern door API + spec, PTY/ConPTY bridging, sandboxing, 2–3 reference doors, dropfile generation | Independent of federation; parallelizable with 2–3. Now the whole of door scope. |
| **5 — Reach** | Web terminal, inter-BBS `DOOR_EVENT`, sneakernet bundles, **client-held DM keys (tier 3)** | Genuinely optional, except tier-3 keys which are a stated preference |
| **6 — Interop & stabilization** | **Freeze BSMP wire format v1** + written spec + conformance vectors, **request Meshtastic portnum**, **FTN gateway** (echomail/netmail, SEEN-BY/PATH, per-echo airtime caps) | Both deliverables are public commitments and both need a format that has survived real radios. `[D10]` sequences the portnum after the freeze; §7.7 sequences the gateway after it too. |
| **7 — Legacy DOS doors** | DOSBox-X bridge, node locking, COM-port bridging | Nice-to-have `[D4]`. May never ship, and nothing depends on it. |

Phases 2 and 4 are parallelizable if there's more than one person working on it. Phase 6 is the first point at which the project makes promises to strangers, and should not be rushed toward.

---

## 13. Open questions raised by the decisions

The v0.1 questions are all answered (§14). These are new, and are consequences of those answers rather than leftovers. None block starting Phase 0.

### Dissolved by the `[D9]` reversal

- **~~`N1` — Who assigns numeric addresses, and what is the default zone?~~** No addresses to assign, no zone to pick, no coordinator to recruit. This was the single biggest unresolved dependency in v0.3 — it needed a volunteer human before the network could grow — and it is simply gone. The `N1` slot is reused below for the alias question that replaces it.
- **~~`N4` — Geographic zone/net structure?~~** No `net` field exists. Topology-aware routing, if ever wanted, would have to come from observed mesh neighbours rather than from the address, which is the more honest source anyway. Slot reused below.

### Needs a decision before Phase 1

**`N1` (re-scoped) — How much alias machinery ships in v1, and who controls it?** `[D9]` makes the petname layer (§6.1.4) the *only* human-facing addressing surface, so its quality is now load-bearing in a way it wasn't when addresses were typeable. Open sub-questions:
- **Sysop-only aliases, or per-user aliases too?** Sysop-only is one table and covers the common case (`pnw`, `fogcity`). Per-user means every user can name nodes for themselves — nicer, but it's a second table, a UI, and a merge story when the sysop's alias and the user's disagree.
- **One-keystroke accept of a remote `NODE` record's suggested display name?** Convenient, and the thing that makes onboarding a new peer feel instant. Also the exact mechanism by which a hostile node gets its preferred name into your table. Suggested: offer it, never auto-apply, and show the full 13-character ID at the moment of acceptance.
- **Do aliases resolve in DM addressing (`austin@pnw`) or only in the UI?** Resolving them in addressing is obviously what users want and means an unresolvable alias is now a delivery failure mode with its own error path.

**`N2` (re-scoped) — Node key rotation and succession.** Simpler than v0.3's version but not gone. Since the ID *is* the key, rotating the key means **becoming a different node**: new ID, and peers' allowlists, aliases, and version vectors all still point at the old one. Proposed: a signed `SUCCESSION` record, signed by the *old* key, naming the new ID, which peers may auto-follow (carrying the alias over) or hold for sysop confirmation. That covers planned rotation cleanly. It does **not** cover a *lost* key — with no old key there is nothing to sign with, so the honest answer is "you are a new node, re-establish out of band." Decide whether auto-follow or confirm-first is the default, and whether the old node ID is tombstoned.

### Needs a decision before Phase 5

**`N3` — Client-held key mechanism.** `ssh-agent`-derived X25519 (deterministic Ed25519 signature as a seed) or a standalone local helper? The agent route is much nicer UX — no extra software, works from any client with agent forwarding — but relies on agent forwarding being available and on a construction that deserves careful review before it protects anyone's mail. The helper route is boring and obviously correct. Prototype both against the tier boundary defined in §8.2.

### Lower stakes

**`N4` (re-scoped) — Node ID display length.** §6.1.4 proposes 13 Crockford base32 characters in full, abbreviated to 8 everywhere except security-relevant surfaces. Worth sanity-checking against real use: is 8 characters enough to be unambiguous in a 50-node network's UI (yes, overwhelmingly), and is 13 short enough that a sysop will actually read it aloud to confirm a fingerprint (borderline)? An alternative is a **word-based encoding** — 64 bits as four or five words from a fixed list, PGP-wordlist style — which is dramatically better for voice confirmation over radio and worse for typing. Both could ship: base32 for typing, words for verification.

**`N5` — Do we want the theme *loader* even without theme packs?** `[D15]` says built-in themes only, and §5.4 keeps colours behind a `Theme` struct so packs remain possible. Question is whether to also read `<datadir>/themes/*.toml` at startup — roughly 50 lines, and it converts "sysop wants a custom theme" from a feature request into a file they edit. Cheap enough that it may not be worth deferring.

**`N6` — What is R, actually?** §7.6 defaults the flood multiplier to 4 with no measurement behind it, and the entire airtime budget scales linearly with it. Worth measuring on a real Pacific Northwest mesh early in Phase 3, and worth a `meshbbs mesh-survey` subcommand that measures it without needing a full BBS deployment.

### Raised by §6.7 / §11 (registration and config)

**`N7` — Default registration mode, and is capability-gating the right shape?** §6.7 recommends `open` registration with `post_federated` **withheld** until the sysop grants it — open front door, gated commons. The alternative is `approval` mode, where nobody gets in without review. Open-plus-gating is friendlier and still protects airtime, but it means new users can post locally and then discover their posts aren't federating, which needs clear UI ("this area is local to this BBS" / "your posts here are pending federation access"). If that ambiguity bothers you, `approval` is the simpler mental model. **Needs deciding before Phase 1.**

**`N8` — Collect email at all?** §6.7 says optional and unverified, because an off-grid BBS may have no way to send mail, so email can never be load-bearing for account recovery. Options: don't collect it (cleanest, and one less PII field to protect), collect as free-text sysop contact info, or collect and use it for recovery *when an SMTP relay is configured* (which reintroduces two recovery paths that behave differently depending on deployment — the thing worth avoiding). Recommendation: don't collect it in v1.

**`N9` — Should new users be listed or unlisted by default?** §6.7 publishes `PROFILE` records lazily, on first federated activity, because eager publication costs ~2 days of a node's entire mesh budget for 50 users. Remaining question is the per-user default once they *do* federate: listed (discoverable in the network directory, the sociable BBS default) or unlisted (DM-reachable but not indexed). Recommendation: listed, since a BBS user list has always been public, with a visible per-user toggle.

---

## 14. Decisions (resolved v0.1 questions)

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
| **D15** | Theme customization? (was `Q6`) | **Small set of built-in themes.** Colours stay behind a `Theme` struct so packs remain a later addition rather than a rewrite. | §5.4, §13 `N5` |

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
- Budget in **airtime-seconds**, not bytes — airtime is superlinear in payload at high SF.
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
