# MeshBBS — High-Level Design

*A modern, cross-platform BBS in Go with SSH access, door games, file areas, forums, and DMs — federated between independent BBS instances over Meshtastic LoRa.*

**Status:** Draft v0.2 — design decisions resolved
**Date:** 2026-07-24
**All 15 open questions from v0.1 are answered. Decisions are recorded in §13 and referenced inline as `[D#]`. New questions raised *by* those decisions are in §12.**

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
3. **Every byte on the wire must be justified.** Binary encoding, truncated hashes, derived (not transmitted) record IDs, packed numeric addresses, a pre-shared compression dictionary, and batching — not JSON, not UUIDs, not per-post packets.

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
- **FidoNet/FTN-compatible addressing and an FTN gateway** — numeric `zone:net/node` addresses `[D9]` and echomail/netmail bridging `[D14]` (§7.7)
- Sysop-friendly: one config file, sane defaults, no external database or runtime

### Non-goals

- **File transfer over mesh in any form.** Catalogs only, hard-blocked in code (§7.5). `[D8]`
- **Bluetooth LE transport.** USB serial and TCP cover every realistic deployment, and BLE is the only dependency that would force cgo. Dropped. `[D13]`
- **User-authored ANSI theme packs.** A curated set of built-in themes only. `[D15]` — see §12 `N5`
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
  - Design constraint to honour anyway: keep colour and glyph choices behind a `Theme` struct rather than hardcoding escape sequences at call sites. That costs nothing now and is the difference between "add theme packs later" being a weekend and being a rewrite. See §12 `N5`.

---

## 6. Domain model

### 6.1 Identity and addressing `[D9]`

Three distinct concepts. Conflating the first two is a classic mistake; conflating the second and third is the specific mistake that numeric addressing invites.

**1. Node key = cryptographic authority.** Ed25519 keypair generated at first run. This is what signs records. It is never a display string and never appears in the UI except as a verification fingerprint.

**2. Node address = human-friendly identity.** A **FidoNet-style 3D/4D numeric address**: `zone:net/node`, optionally `.point`. This is what users type, what appears in the UI, and what travels in record headers.

```
42:100/7        zone 42, net 100, node 7
42:100/7.1      a point (a satellite/personal instance hanging off node 7)
```

Rationale for numeric over key-derived: the user requirement is that addressing be *easy to understand and easy to type*. `42:100/7` is memorable and speakable over voice radio; `K7QM4X2P` is neither. It also lines up exactly with the FTN gateway `[D14]`, which uses the same address space — one addressing scheme covers both networks instead of a translation table. And packed, it is *cheaper on the wire* than the 8-byte key prefix v0.1 proposed (see below).

**Binding address → key.** This is the piece key-derived IDs got for free and we now have to build. A `NODE` record carries `{address, node_pubkey, display_name, sysop_contact, capabilities}`, self-signed by the node key. Rules:

- A node's records are only accepted once its `NODE` record is held (otherwise: quarantine, not drop — the `NODE` record may simply be a few minutes behind on a lossy mesh).
- **First-seen binding wins.** A second `NODE` record claiming an already-bound address with a different key is *rejected and logged as an alert*, never silently accepted. This is the anti-squatting rule and it must be in the first implementation, not retrofitted.
- Address changes are a signed `NODE` record superseding the previous one **under the same key**. Key rotation is a separate, deliberately awkward operation requiring sysop-to-sysop confirmation (see §12 `N2`).
- The full roster is ~50 nodes × ~100 B = **~5 KB**, trivially replicated and cheap to backfill.

**Wire encoding.** Packed into **4 bytes**: `zone` u8, `net` u12, `node` u12. That covers 255 zones, 4095 nets, 4095 nodes per net — ample for a 50-instance network with room for growth, and 4 bytes cheaper per record than a truncated key. Addresses outside that range (which real FidoNet addresses often are — nets and nodes go to 65535) are **gateway-only**: they appear in the *body* of gateway-originated records as full 4×u16, never in the mesh record header. `[D14]`

**3. User identity = a person.** An Ed25519 keypair, generated server-side at signup (with an option for the user to supply their own). A globally-addressable user is `nick@zone:net/node` — e.g. `austin@42:100/7`. The pubkey is what matters for DM encryption; the nick is a display convenience and *is not globally unique*.

### 6.2 The record log — the heart of federation

Everything replicable is an **immutable, signed record** in a single append-only log:

```
Record {
  id        [16]byte   // BLAKE3(canonical_body)[:16] — DERIVED, NOT TRANSMITTED
  origin    [4]byte    // packed zone:net/node of the authoring instance
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
- **`area` and the base `ts` hoist to the bundle header.** Bundles are per-area by construction, so the 4-byte area tag is paid once per bundle rather than once per record, and timestamps become small deltas.
- **`(origin, seq)` pairs make reconciliation a version-vector problem**, which is the simplest correct approach. See §7.3.
- **Immutable + tombstones** means forums have *no merge conflicts at all*. An edit is a new record with a `supersedes` pointer; a delete is a signed tombstone. Whether a BBS honors a *remote* delete is local sysop policy — see below.
- **The node signs, not the user, for forum posts.** `[D5]` A 64-byte Ed25519 signature is 27% of a mesh packet, so dual-signing every post is unaffordable. Node-signing means "instance 42:100/7 vouches that user `austin` posted this," which matches the FidoNet trust model — you trust the sysop, and the sysop is accountable. **DMs are user-signed** (§8.2), where non-repudiation actually matters and volume is lower.

### 6.3 Forums

- **Areas** (a.k.a. echo areas / conferences) are the unit of replication. Each has a name, description, moderation policy, and a list of peer nodes it federates with.
- Local-only areas exist and never touch the mesh. **Default new areas to local-only** — sysops must opt *in* to burning airtime. At the §1.1 budget this default is doing real work.
- Threading via `parent` pointers; the UI reconstructs trees. Out-of-order arrival is normal on a mesh, so orphaned replies must render gracefully ("parent not yet received") and re-parent when the parent arrives.
- Per-area retention policy (age, count) — old records are prunable; the log is not required to be complete forever.
- **Per-area airtime sub-budget.** Given ~10 originated packets/day/node at 50 instances, one chatty area can starve every other. Each federated area gets a share of the node's governor allocation. This is also the mechanism that makes an FTN-bridged echo safe (§7.7).

### 6.4 Direct messages

- Addressed to `nick@zone:net/node`. Routed by node address.
- **End-to-end encrypted, user-signed** (§8.2). Intermediate BBSes and everyone else on the mesh channel store and forward opaque bytes.
- **Recipient addressing is in the clear.** `[D7]` v0.1 proposed routing on `BLAKE3(recipient_pubkey)[:8]` to hide the recipient from intermediate sysops. Since metadata privacy is explicitly not a requirement, we drop it — and get real benefits: intermediate nodes can bounce undeliverable mail immediately instead of holding it blind, sysops can rate-limit and spam-filter per recipient, and the address is 4 bytes instead of 8.
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

**Version vectors.** For each area, a node tracks `{origin_node → highest_contiguous_seq}`: 4 bytes of packed address + ~2 bytes of varint seq = **6 bytes per known origin per area** (down from 10 in v0.1, thanks to numeric addressing). At 50 instances `[D2]` that is **300 bytes per area** — two mesh packets — and across 10 federated areas, 3 KB.

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
- Be honest in the UI: "held by 42:100/3 — no IP route from here; queued for next exchange" is a feature, not a failure.

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

FidoNet/FTN compatibility moves from non-goal to goal. It is a strong fit: `[D9]` already put us in FTN's address space, the record log maps cleanly onto echomail, and there is a live FTN scene to connect to. It is also the single most dangerous feature in the document for the airtime budget, so the constraints come first.

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
| `zone:net/node.point` | node address, full 4×u16 in the record body (§6.1) |

**The gateway is a trust boundary, and this is the part to get right.** FTN echomail is plaintext, unsigned, and carries no cryptographic origin. Records entering from FTN cannot be signed by their true author, so:

- The **gateway node signs them with its own key**, and they are marked `via_ftn` with the original FTN origin address and `MSGID` preserved in the body.
- The UI must display them as gateway-attested, not author-attested: "from `joe@1:234/5` via gateway 42:100/1". Users need to know the trust chain is different.
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
- **DM metadata is not protected.** `[D7]` Other sysops can see that `austin@42:100/7` sent a DM to `joe@42:100/3`, and when. This is a deliberate, documented trade — it buys immediate bounces, per-recipient spam filtering, and 4 bytes per DM. The user-facing docs must state it plainly rather than letting people assume otherwise from the phrase "end-to-end encrypted."

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
- **Tier 3** ships a small `meshbbs-key` helper the user runs locally. Two viable mechanisms, to be chosen during Phase 5 (§12 `N3`): a local helper that the user pipes ciphertext through, or deriving the X25519 key from a deterministic Ed25519 signature over a fixed domain-separation string made by the user's forwarded `ssh-agent` — Ed25519 signatures are deterministic, so this yields a stable keypair the agent's holder alone can reproduce, without the private key ever leaving the agent.
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

- **Peer allowlist.** A sysop explicitly adds peer node addresses *and their keys* (the pairing from §6.1 — allowlisting an address alone would be meaningless). Unknown-origin records are quarantined for review by default, which is friendlier to network growth than dropping.
- **Address squatting** is the new attack surface numeric addressing introduces: first-seen binding wins, conflicts are alerted, never silently resolved (§6.1).
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
- **First-run wizard:** generate keys, **choose or request a numeric address**, pick a node name, scan for a Meshtastic device, configure the channel, show the derived airtime allocation in human terms (§7.6), create the sysop account. Should take under two minutes.
- **Testing — the simulated mesh harness.** The `Link` abstraction lets us run N in-process BBS instances over a fake link with configurable MTU, latency, loss, **and flood multiplier**. This is essential; you cannot iterate on a sync protocol by physically deploying radios. It is also where three specific things get validated:
  - the fountain code's overhead at small K (§7.2),
  - digest suppression and scaling at N = 50 (§7.3),
  - FTN loop prevention on a deliberately cyclic topology (§7.7).

  Meshtastic's own discrete-event simulator (`meshtasticator`) can validate RF-level assumptions and calibrate R later.

---

## 11. Roadmap

Revised per the decisions. The largest changes: the fountain codec moves **up** into Phase 2 (it needs the harness), DOS doors move **down** to Phase 7, BLE and theme packs are **gone**, and a new Phase 6 covers wire-format freeze plus the FTN gateway.

| Phase | Scope | Why this order |
|---|---|---|
| **0 — Skeleton** | Go module, SQLite schema + migrations, config, logging, node key generation, **numeric address + `NODE` record model**, CI cross-compiling all 5 targets cgo-free | Prove the cgo-free build works on day one. Addressing lands here because §6.1's first-seen binding rule is hard to retrofit. |
| **1 — Single-node BBS** | SSH server, Bubble Tea UI, menus, ANSI/CP437 rendering, **built-in themes behind a `Theme` struct**, users/auth, local forums, local DMs with **passphrase-wrapped keys (tier 2)**, file areas via SFTP, presence/node chat, telnet off-by-default | A genuinely usable BBS with zero federation. Ship this; get people on it. Tier-2 key custody is here because retrofitting key wrapping means re-keying every user. |
| **2 — Federation over IP + the harness** | Record log, version vectors, anti-entropy with **digest suppression/scaling**, bundle format, zstd dictionary, `Link` abstraction, **simulated mesh harness**, **fountain codec (L1)**, QUIC/Noise IP link | Build and debug the sync protocol where iteration is fast — but design every byte for the mesh MTU. The codec is here, not Phase 3, because tuning small-K overhead needs the harness's controllable loss. |
| **3 — Meshtastic link** | Serial + TCP transports, protobuf framing, **airtime governor with flood-multiplier accounting**, ham-mode safety checks (DMs *and* channel PSK), file catalog replication, R estimation | The protocol already fits 233 bytes because Phase 2 was designed that way |
| **4 — Doors** | Modern door API + spec, PTY/ConPTY bridging, sandboxing, 2–3 reference doors, dropfile generation | Independent of federation; parallelizable with 2–3. Now the whole of door scope. |
| **5 — Reach** | Web terminal, inter-BBS `DOOR_EVENT`, sneakernet bundles, **client-held DM keys (tier 3)** | Genuinely optional, except tier-3 keys which are a stated preference |
| **6 — Interop & stabilization** | **Freeze BSMP wire format v1** + written spec + conformance vectors, **request Meshtastic portnum**, **FTN gateway** (echomail/netmail, SEEN-BY/PATH, per-echo airtime caps) | Both deliverables are public commitments and both need a format that has survived real radios. `[D10]` sequences the portnum after the freeze; §7.7 sequences the gateway after it too. |
| **7 — Legacy DOS doors** | DOSBox-X bridge, node locking, COM-port bridging | Nice-to-have `[D4]`. May never ship, and nothing depends on it. |

Phases 2 and 4 are parallelizable if there's more than one person working on it. Phase 6 is the first point at which the project makes promises to strangers, and should not be rushed toward.

---

## 12. Open questions raised by the decisions

The v0.1 questions are all answered (§13). These are new, and are consequences of those answers rather than leftovers. None block starting Phase 0.

### Needs a decision before Phase 2

**`N1` — Who assigns numeric addresses, and what is the default zone?** `[D9]` gives us easy-to-type addresses but also, unavoidably, an address authority. Options:
- **Coordinator model (FidoNet-like):** a per-zone coordinator publishes a signed roster; new sysops request an address. Familiar to BBS people, matches the FTN gateway, needs a volunteer.
- **Self-assign + collision detect:** pick your own, first-seen binding wins, conflicts alerted. No authority, no gatekeeping, but two sysops who both pick `42:100/1` while partitioned have a genuine mess when the partition heals.
- **Hybrid (suggested):** self-assign within a reserved "unregistered" net (e.g. `42:999/*`) for isolated meshes and testing; coordinator-assigned nets for the connected network. Also: what zone number? It must not collide with FidoNet zones 1–6 or other live FTN networks.

**`N2` — Node key rotation.** First-seen address→key binding (§6.1) means a sysop who loses their node key loses their address. What's the recovery path? A sysop-to-sysop attested rotation (M-of-N peers sign an override), a coordinator-signed override if `N1` lands on a coordinator, or "generate a new address and move on"? The last is honest but painful once an address is on business cards.

### Needs a decision before Phase 5

**`N3` — Client-held key mechanism.** `ssh-agent`-derived X25519 (deterministic Ed25519 signature as a seed) or a standalone local helper? The agent route is much nicer UX — no extra software, works from any client with agent forwarding — but relies on agent forwarding being available and on a construction that deserves careful review before it protects anyone's mail. The helper route is boring and obviously correct. Prototype both against the tier boundary defined in §8.2.

### Lower stakes

**`N4` — Which zone/net structure for the mesh network itself?** Related to `N1` but narrower: does `net` correspond to a geographic mesh (so `42:100/*` is the Pacific Northwest mesh and routing can be topology-aware), or is it flat? Geographic nets make hierarchical routing possible later; flat is simpler now.

**`N5` — Do we want the theme *loader* even without theme packs?** `[D15]` says built-in themes only, and §5.4 keeps colours behind a `Theme` struct so packs remain possible. Question is whether to also read `<datadir>/themes/*.toml` at startup — roughly 50 lines, and it converts "sysop wants a custom theme" from a feature request into a file they edit. Cheap enough that it may not be worth deferring.

**`N6` — What is R, actually?** §7.6 defaults the flood multiplier to 4 with no measurement behind it, and the entire airtime budget scales linearly with it. Worth measuring on a real Pacific Northwest mesh early in Phase 3, and worth a `meshbbs mesh-survey` subcommand that measures it without needing a full BBS deployment.

---

## 13. Decisions (resolved v0.1 questions)

| # | Question | Decision | Main sections affected |
|---|---|---|---|
| **D1** | ARQ or fountain codes? (was `Q1`) | **Fountain coding from the start.** Refined to a *systematic* LT code with small-K-tuned degrees and derived (untransmitted) XOR masks — off-the-shelf RaptorQ is a poor fit at K < 20 and carries IPR questions. | §7.2, §4, Phase 2 |
| **D2** | How many instances? (was `Q8`) | **~50 at most.** Version vectors stay; but this broke the digest cycle (11% of channel) and forced digest scaling/suppression, and it shrinks each node's share to ~10 packets/day. | §1.1, §7.3, §7.6 |
| **D3** | Own Meshtastic transport or a library? (was `Q3`) | **Vendor `meshtastic/protobufs`, roll our own transport.** | §4 |
| **D4** | Legacy DOS doors? (was `Q15`) | **Nice-to-have.** Modern door API gets all of Phase 4; DOS deferred to Phase 7. | §9, Phase 4/7 |
| **D5** | Node- or user-signed posts? (was `Q9`) | **Node-signed forums, user-signed DMs** (the v0.1 recommendation). | §6.2, §8.2 |
| **D6** | Honour remote deletes? (was `Q10`) | **Advisory, sysop-configurable.** Refined: auto-honour when the tombstone's origin matches the post's origin; never auto-honour a third-party tombstone. | §8.4 |
| **D7** | DM metadata privacy? (was `Q12`, part 1) | **Content confidentiality only.** Drops hashed-recipient routing — DMs address `nick@zone:net/node` in the clear, enabling immediate bounces and spam filtering, and saving 4 bytes. Must be documented plainly. | §6.4, §8.1 |
| **D8** | Mesh file transfer? (was `Q11`) | **None at all.** Not a quota or a toggle — the mesh link refuses file payloads by type. Catalogs replicate; bytes move over IP or sneakernet. | §1, §6.5, §7.5 |
| **D9** | Node addressing? (was `Q7`) | **FidoNet-style numeric `zone:net/node`.** Keys remain the cryptographic authority, bound to addresses by signed `NODE` records with first-seen-wins anti-squatting. Cheaper on the wire (4 B) than the key prefix it replaces, and shares an address space with the FTN gateway. Introduces `N1`/`N2`. | §6.1, §8.4 |
| **D10** | Register a portnum? (was `Q2`) | **Yes, but after the wire format is frozen and tested.** Versioning from day one; freeze + spec + conformance vectors + portnum request all in Phase 6. | §7.1, Phase 6 |
| **D11** | Hard-block encrypted DMs in ham mode? (was `Q14`) | **Hard-block with a documented override flag.** Extended: the channel PSK must also be disabled in ham mode, and signing remains legal and enabled. | §8.3 |
| **D12** | Telnet? (was `Q5`) | **Off by default, loud warning when enabled.** Guest-only telnet is the recommended middle setting. | §5.2 |
| **D13** | BLE? (was `Q4`) | **Dropped, future/never.** Makes every artifact cgo-free with no build tags — a real packaging simplification. | §2, §3, §4, §7.1, §10 |
| **D14** | FTN gateway? (was `Q13`) | **Yes.** Moves from non-goal to goal, Phase 6. Synergistic with `D9`. Constrained hard: IP-side by default, per-echo airtime caps, and explicitly labelled as a trust boundary since FTN mail is unsigned. | §2, §7.7, Phase 6 |
| **D15** | Theme customization? (was `Q6`) | **Small set of built-in themes.** Colours stay behind a `Theme` struct so packs remain a later addition rather than a rewrite. | §5.4, §12 `N5` |

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

Revised for the derived-`id` and packed-address changes in §6.2. Single-record bundle, worst case:

| Component | v0.1 | v0.2 |
|---|---:|---:|
| Meshtastic `Data` payload ceiling | 233 | 233 |
| L1 symbol header | −8 | −8 |
| L2 bundle header (dict id, area tag, base ts, count, flags) | −4 | −6 |
| Record `id` | −16 | **0** (derived, not sent) |
| `origin` | −8 | **−4** (packed `zone:net/node`) |
| `seq`, `ts` delta, `type` | −13 | **−5** (varint, ts hoisted to bundle) |
| `area` | −4 | **0** (hoisted to bundle header) |
| `parent` (threaded replies only) | −16 | −16 |
| Ed25519 signature | −64 | −64 |
| **Remaining for compressed body** | **106** | **130** |
| → decompressed at 3.5× with trained dictionary | ≈ 370 chars | **≈ 455 chars** |

A top-level post (no `parent`) has 146 bytes for the body, ≈ 510 characters.

Batching compounds this: across a 10-record bundle the bundle header and area tag are paid once, and per-record cost falls to `sig(64) + origin(4) + seq/ts/type(5) = 73` bytes plus body. **The signature is now 50–60% of the per-record overhead** — which is precisely why `D5` (node-signed, not dual-signed) matters, and it is the obvious target if a future revision needs more room (aggregate signatures over a whole bundle would recover ~64 bytes per record, at the cost of losing per-record verifiability).

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
