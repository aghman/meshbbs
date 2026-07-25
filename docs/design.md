# MeshBBS — High-Level Design

*A modern, cross-platform BBS in Go with SSH access, door games, file areas, forums, and DMs — federated between independent BBS instances over Meshtastic LoRa.*

**Status:** Draft v0.1 — design for review
**Date:** 2026-07-24
**Open questions are collected in §12, and inline as `❓Q#` markers throughout.**

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

**Three conclusions fall straight out of this table:**

1. **Forums and DMs over mesh are entirely viable.** Even at a polite 5% airtime budget on LongFast, ~1,500 posts/day is far more than a hobby BBS network will generate. At a very polite 1% it's still ~300/day.
2. **File transfer over mesh is not viable** and should not be attempted as a general feature. A single 1 MB file at 5% LongFast takes **2.2 days** of continuous channel time. Mesh should replicate *file catalogs* (name, size, hash, description, which BBSes hold it) and let the bytes move over IP or sneakernet. See §7.5.
3. **Every byte on the wire must be justified.** This pushes toward binary encoding, truncated hashes, a pre-shared compression dictionary, and batching — not JSON, not UUIDs, not per-post packets.

There is also a social constraint that matters as much as the technical one: **a shared Meshtastic channel is a commons.** If a BBS network parks itself on a community mesh and eats 30% of airtime, it will be (rightly) unwelcome. The airtime governor (§7.6) is a first-class feature, not a nice-to-have.

---

## 2. Goals and non-goals

### Goals

- Single self-contained Go binary; runs on Linux, macOS, Windows (amd64 + arm64)
- SSH as the primary access method, with a modern TUI that still feels like a BBS
- Forums (message bases), direct messages, file areas, door games, multi-node presence
- Federation of forums and DMs between independent BBS instances
- Meshtastic as a *first-class but not exclusive* federation transport
- Off-grid operation: a BBS with no internet at all is a full participant
- Sysop-friendly: one config file, sane defaults, no external database or runtime

### Non-goals (v1)

- Bulk file replication over mesh (catalogs only — see §7.5)
- Real-time inter-BBS chat over mesh (latency is 10s of seconds to minutes; do it over IP)
- FidoNet/FTN gateway compatibility (design leaves room; not built in v1) — `❓Q13`
- Web forum UI. A read-only web view might come later; SSH is the product.

---

## 3. Architecture overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                          FRONT ENDS                                  │
│   SSH (primary)   Telnet (legacy/doors)   WebSocket+xterm.js (opt)   │
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
                            ┌─────────────────────────┼──────────────────┐
                            │                         │                  │
                   ┌────────▼────────┐   ┌────────────▼───────┐  ┌───────▼──────┐
                   │  MESH LINK      │   │   IP LINK          │  │  SNEAKERNET  │
                   │  (Meshtastic)   │   │  (QUIC/TCP+Noise)  │  │  (file bundle)│
                   │  MTU 233, ~108B/s│   │  MTU ~1400, MB/s   │  │  offline     │
                   └────────┬────────┘   └────────────────────┘  └──────────────┘
                            │
              ┌─────────────┼─────────────┐
        ┌─────▼────┐  ┌─────▼────┐  ┌─────▼────┐
        │  Serial  │  │   TCP    │  │   BLE    │
        │  (USB)   │  │ (WiFi)   │  │(optional)│
        └──────────┘  └──────────┘  └──────────┘
                     Meshtastic node
```

### The two load-bearing abstractions

**(a) `Link` — the federation transport interface.** Meshtastic is treated as *one* link type: a datagram link with a 233-byte MTU, ~108 B/s, high loss, and multi-minute latency. IP is another with a 1400-byte MTU and megabytes/second. Sneakernet (export a bundle to a USB stick) is a third.

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

**(b) `Transport` — how we talk to the Meshtastic node.** Serial/TCP/BLE all speak the same protobuf stream API, so this is a thin layer. See §7.1.

---

## 4. Technology choices

| Concern | Choice | Why |
|---|---|---|
| Language | Go 1.23+ | Stated requirement; excellent cross-compilation |
| SSH server | `charmbracelet/wish` (over `gliderlabs/ssh`) | Middleware model, native Bubble Tea integration, PTY + window-resize handling |
| TUI | `charmbracelet/bubbletea` + `lipgloss` + `bubbles` | Elm architecture fits a menu-driven BBS well; one `tea.Program` per SSH session |
| Database | **`modernc.org/sqlite`** (pure Go) | **Critical:** avoids cgo. `mattn/go-sqlite3` needs a C toolchain, which destroys clean cross-compilation to Windows/macOS/arm64. Non-negotiable if you want `GOOS=windows go build` to just work. |
| Serial | `go.bug.st/serial` | Pure Go, cross-platform, well-maintained |
| BLE | `tinygo.org/x/bluetooth` | Only credible cross-platform Go BLE option. **Caveat:** Windows and macOS are central-only (fine for us) but the backends need platform SDK bindings — likely cgo on macOS. Recommend BLE behind a build tag so the default binary stays cgo-free. `❓Q4` |
| Protobuf | `google.golang.org/protobuf` + `meshtastic/protobufs` as a submodule | Official schema, generate Go bindings at build time |
| Compression | `klauspost/compress/zstd` with a **trained dictionary** | Pure Go. Dictionary compression is the single highest-leverage optimization on the mesh — see §7.4 |
| Hashing | BLAKE3 (`lukechampine.com/blake3`) | Fast, pure Go, good truncation properties |
| Signing | Ed25519 (`crypto/ed25519`) | Stdlib |
| DM encryption | X25519 sealed box (`golang.org/x/crypto/nacl/box` or a minimal custom construction) | 48-byte overhead vs. age's ~200; see §8.2 |
| Assets | `embed.FS` | Single-binary deployment with ANSI art, themes, migrations |

### On existing Meshtastic Go libraries

None of the options are a comfortable dependency:

- `meshnet-gophers/meshtastic-go` — has the right shape (`transport/`, `radio/`, `mqtt/`) but 11 stars, last release Feb 2024, and the README literally says *"Consider these contracts written with a half-eaten crayon; they will change."*
- `lmatte7/gomesh` / `lmatte7/meshtastic-go` — CLI-oriented, serial+TCP only, no BLE.
- `exepirit/meshtastic-go` — newer (published March 2026), MIT, but unproven.

**Recommendation:** vendor the official `meshtastic/protobufs` as a git submodule and write our own thin transport layer. The framing is genuinely trivial — a 4-byte header (`0x94 0xC3`, then a 16-bit big-endian length) wrapping a `ToRadio`/`FromRadio` protobuf, identical across serial, TCP, and BLE. That's maybe 300 lines. Cribbing structure from `meshnet-gophers` is fair game; depending on it is not. `❓Q3`

---

## 5. Front ends and the session layer

### 5.1 SSH (primary)

Wish gives a middleware chain per connection. Auth supports:

- **Public key** — a user registers their SSH pubkey; subsequent logins are passwordless. This is the modern path and should be the default we advertise.
- **Password** — over the SSH transport, keyboard-interactive. Needed for new-user signup and for people connecting from a client they don't control.
- **Guest/anonymous** — read-only browsing, configurable.

Notably, SSH gives us **SFTP for free** as the file-transfer mechanism. No ZMODEM, no XMODEM, no serial-protocol emulation nonsense — `sftp bbs.example.com` and you're in the file areas. This is a large simplification over legacy BBS software.

### 5.2 Telnet (legacy, optional)

Off by default. Exists because (a) some ANSI terminal clients (SyncTERM, NetRunner, MagiTerm) are telnet-only and are what BBS people actually use, and (b) DOS door bridging is easier over a raw socket. If enabled, it should warn loudly that credentials cross the wire in plaintext. `❓Q5`

### 5.3 Web terminal (optional, later)

`xterm.js` over a WebSocket, same session API. Good for casual visitors and for showing the BBS off. Low priority.

### 5.4 Terminal rendering — the fiddly bit

BBS aesthetics mean **CP437 + ANSI art**, but modern terminals are UTF-8. Plan:

- Detect capability: `LANG`/`LC_ALL` env, terminal type, and a first-run user preference toggle.
- Store art as CP437 bytes; render through a translation layer that either passes bytes through (legacy client) or maps CP437 → Unicode box-drawing/block glyphs (modern client).
- Support SAUCE metadata on `.ANS` files (dimensions, font hints) — it's what art packs ship with.
- Handle window size: SSH gives us `pty-req` and `window-change`; Bubble Tea consumes these natively. Fall back to 80×24.
- Theme packs as directories of ANSI templates + a manifest, so sysops can reskin without recompiling. `❓Q6`

---

## 6. Domain model

### 6.1 Identity

Two distinct identity concepts, and conflating them is a classic mistake:

- **Node identity** = the BBS instance. Ed25519 keypair generated at first run. Short form is the first 8 bytes base32-encoded (e.g. `K7QM4X2P`) — this is what appears on the wire. Human-readable name and a sysop-chosen tag (`❓Q7` — do you want FidoNet-style numeric addresses, or opaque key-derived IDs? Key-derived means no registry and no address authority, which fits mesh well).
- **User identity** = a person. Also an Ed25519 keypair, generated server-side at signup (with an option for the user to supply their own). A globally-addressable user is `nick@NODEID`. The pubkey is what actually matters for DM encryption; the nick is a display convenience and *is not globally unique*.

### 6.2 The record log — the heart of federation

Everything replicable is an **immutable, signed record** in a single append-only log:

```
Record {
  id        [16]byte   // BLAKE3(canonical_body)[:16] — content-addressed
  origin    [8]byte    // node ID that authored it
  seq       uint64     // per-origin monotonic sequence
  ts        uint32     // unix seconds
  type      uint8      // POST, DM, PROFILE, AREA, FILE, TOMBSTONE, VOTE, DOOR_EVENT
  area      [4]byte    // truncated hash of area name (forums only)
  parent    [16]byte   // threading: parent record id (optional)
  body      []byte     // type-specific, zstd-compressed with shared dict
  sig       [64]byte   // Ed25519 over everything above, by origin node key
}
```

Design notes:

- **Content addressing** gives free dedup — the mesh floods packets, and the same record will arrive multiple times via multiple paths. Dedup by `id` is trivial and exact.
- **Truncated to 16 bytes.** A full 32-byte BLAKE3 would cost 7% of a mesh packet per reference. 16 bytes (128 bits) is ample collision resistance for a network of dozens of BBSes. Threading references (`parent`) cost 16 bytes each — acceptable.
- **`(origin, seq)` pairs make reconciliation a version-vector problem**, which is the simplest correct approach. See §7.3.
- **Immutable + tombstones** means forums have *no merge conflicts at all*. An edit is a new record with a `supersedes` pointer; a delete is a signed tombstone. Whether a BBS honors a *remote* delete is local sysop policy — important, because otherwise a rogue node could nuke content network-wide. `❓Q10`
- **The node signs, not the user** (for forum posts). This is a deliberate choice: a 64-byte Ed25519 signature is 27% of a mesh packet, so signing every post with *both* user and node keys is expensive. Node-signing means "BBS K7QM4X2P vouches that user `austin` posted this," which matches the FidoNet trust model — you trust the sysop, and the sysop is accountable. DMs are different (§8.2). `❓Q9`

### 6.3 Forums

- **Areas** (a.k.a. echo areas / conferences) are the unit of replication. Each has a name, description, moderation policy, and a list of peer nodes it federates with.
- Local-only areas exist and never touch the mesh. Default new areas to local-only — sysops should opt *in* to burning airtime.
- Threading via `parent` pointers; the UI reconstructs trees. Out-of-order arrival is normal on a mesh, so orphaned replies must render gracefully ("parent not yet received") and re-parent when the parent arrives.
- Per-area retention policy (age, count) — old records are prunable; the log is not required to be complete forever.

### 6.4 Direct messages

- Addressed to `nick@NODEID`. Routed by node ID.
- **End-to-end encrypted** (§8.2). Intermediate BBSes and everyone else on the mesh channel store and forward opaque bytes.
- Store-and-forward: if the destination node isn't reachable, hold and retry with exponential backoff, with a TTL (default 7 days) after which we return a bounce to the sender.

### 6.5 Files

- Local file areas with upload/download via SFTP over SSH, plus an in-TUI browser.
- Content-addressed blob store (BLAKE3 → `blobs/ab/cd/abcd...`), so identical files across areas dedup.
- **Over mesh, only the catalog replicates**: `FILE` records carry name, size, hash, description, tags, and origin node — roughly 120–200 bytes compressed. Users see the whole network's file list and can request a fetch, which is satisfied over IP if available, queued for sneakernet if not, or (with sysop approval and a hard quota) trickled over mesh for genuinely tiny files. `❓Q11`

### 6.6 Doors — see §9.

---

## 7. Federation: the mesh sync protocol

Call it **BSMP** (BBS Sync over Mesh Protocol). Five layers, each with one job.

```
┌─ L4  Records ──────── POST / DM / PROFILE / FILE / TOMBSTONE …
├─ L3  Replication ──── version vectors, anti-entropy digests, delta requests
├─ L2  Bundle ────────── zstd(shared dict) + framing of N records
├─ L1  Fragmentation ── split bundle → ≤225B fragments, ARQ or fountain code
└─ L0  Datagram ─────── Meshtastic MeshPacket, portnum PRIVATE_APP, own channel
```

### 7.1 L0 — the Meshtastic datagram

**Channel.** A dedicated Meshtastic channel (e.g. named `bbsnet`) with its own PSK, distinct from the community's primary channel. Meshtastic supports 8 channel slots; this keeps BBS traffic logically separate and lets non-BBS nodes ignore it. **But note:** channel separation is *not* airtime separation — all channels on the same frequency slot share the same physical airtime. A separate channel does not make us a better neighbor; the governor (§7.6) does.

**Port number.** `PRIVATE_APP` (256) for development. The 256–511 range is reserved for private apps and needs no registration. If this ever goes public, request an allocation from the Meshtastic project so other tools can decode BBS traffic sensibly. `❓Q2`

**Transports to the local node** — all three speak the same protobuf stream:

- **Serial/USB** — `go.bug.st/serial`, 4-byte frame header (`0x94 0xC3 <len_hi> <len_lo>`) + protobuf. Most reliable; recommended default.
- **TCP** — the node's WiFi API on port 4403, same framing. Best when the node is mounted somewhere with better RF than the server closet.
- **BLE** — GATT service `6ba1b218-15a8-461f-9fa8-5dcae273eafd`, with `toradio` (write), `fromradio` (read), `fromnum` (notify) characteristics. Set MTU to 512. More fragile, and platform support in Go is the weakest link. Ship it, but not as the default. `❓Q4`

The adapter should auto-detect: scan serial ports for Meshtastic VID/PIDs, try configured TCP host, fall back to BLE scan.

**Reliability.** Meshtastic offers `want_ack` with limited firmware-level retries, but it is not a reliable transport and shouldn't be treated as one. Use `want_ack` only for small control packets (digests, delta requests) and let L1 handle bulk reliability. Also respect the hop limit (0–7, default 3) — set it explicitly based on the BBS network's topology rather than inheriting the node default.

### 7.2 L1 — fragmentation and reliability

A bundle exceeds 233 bytes almost always, so fragmentation is mandatory. Header design (target ≤ 8 bytes so ~225 bytes of payload survive):

```
byte 0     : version(2b) | type(3b) | flags(3b)
bytes 1-4  : bundle_id (uint32, random per bundle)
byte 5     : frag_index (or symbol id, for fountain mode)
byte 6     : frag_total
byte 7     : reserved / extended index high bits
```

Two candidate reliability strategies, and the second is arguably the more interesting answer — this is `❓Q1`, the most important open question in the document:

**(a) Selective-repeat ARQ.** Receiver tracks which fragments arrived and NACKs the gaps as a bitmap (1 byte covers 8 fragments, so even a 64-fragment bundle NACKs in 8 bytes). Sender retransmits only the missing ones. Simple, well-understood, and optimal for a *single* receiver.

**(b) Rateless fountain coding (RaptorQ / LT codes).** Here's the thing about a mesh: a broadcast reaches *every* listening BBS at once, and each one misses a *different* random subset of fragments. Under ARQ, that means N receivers sending N different NACK sets and the sender retransmitting the union — the cost grows with the number of peers. Under a fountain code, the sender emits an endless stream of encoded symbols and each receiver decodes as soon as it has collected *any* K+ε symbols. One transmission serves everyone, and there is no NACK traffic at all. For a broadcast medium with independent per-receiver loss, this is a much better fit, and the overhead (~5% for RaptorQ) is cheap next to a retransmit round-trip that costs minutes.

Recommendation: **build ARQ first** (simpler, needed anyway for point-to-point DM delivery), design the L1 header so fountain mode is a `type` bit, and add fountain coding for multi-peer area broadcast in a later phase. Go options: `github.com/google/gofountain`, or a small LT-code implementation (a few hundred lines).

### 7.3 L3 — replication and anti-entropy

**Version vectors.** For each area, a node tracks `{origin_node → highest_contiguous_seq}`. This is the whole state needed for reconciliation, and it's tiny: 8 bytes of node ID + ~2 bytes of varint seq = **10 bytes per known origin per area**. For a 20-BBS network that's 200 bytes per area — one to two mesh packets.

For v1, skip the fancier set-reconciliation machinery (Merkle trees, IBLTs, range-based reconciliation). Those pay off at thousands of peers with heavily diverged state. At BBS-network scale (`❓Q8` — how many instances are you actually imagining? 5? 50? 500? This changes the answer), version vectors are simpler, exact, and small enough.

**The gossip cycle:**

1. **Digest broadcast** (every T minutes, default 15–30, jittered): a compact summary of `(area, high-water marks)` for federated areas. Deliberately small — target one packet. Use a rolling hash per area so peers can detect divergence without exchanging full vectors.
2. **Delta request** (unicast): a peer that notices it's behind requests `(area, origin, from_seq, to_seq)`. ~12 bytes per range.
3. **Bundle push** (broadcast): the holder packages the requested records into a bundle and broadcasts it — other lagging peers benefit for free.
4. **Opportunistic push**: new local posts are batched (default 5–15 min) and pushed proactively without waiting for a digest cycle, subject to the governor.

Batching is important. Sending one packet per post wastes the packet header on every post and burns 2.16 s of airtime for a 40-character message. Accumulating 15 minutes of posts into one bundle amortizes framing and lets zstd find cross-message redundancy.

**Failure behavior:** a node offline for a week comes back, broadcasts a digest showing low high-water marks, and peers backfill it. There's no session, no handshake, no state machine that can wedge. This is the property that makes anti-entropy the right choice over a FidoNet-style polling session — mesh links are too flaky for sessions.

### 7.4 L2 — compression with a trained dictionary

This deserves its own section because it's the highest-leverage optimization available.

Generic zstd on a 400-byte forum post gets maybe 1.3–1.5× — there just isn't enough data for the compressor to build a model. **zstd with a pre-trained dictionary** on the same post typically gets 3–5×, because the dictionary already contains common English fragments, BBS-specific vocabulary, quoting conventions (`> `), signature patterns, and structural boilerplate.

At 108 B/s, tripling effective throughput is equivalent to switching from LongFast to MediumFast — *without* giving up 5 dB of link budget. It is free range.

Implementation:
- Ship a versioned dictionary (`dict_v1`, a few tens of KB) embedded in the binary.
- Dictionary ID goes in the bundle header (1 byte). Nodes negotiate/announce which dictionaries they hold in their digest.
- Train it (`zstd --train`) on a corpus of real BBS/Usenet/forum text. Refresh in later versions; old dictionaries stay supported.
- Compress the *bundle*, not individual records, so cross-record redundancy is captured.

### 7.5 File catalogs, not files

Restating the §1 conclusion as a design rule, since it will be tempting to violate:

- `FILE` records replicate metadata only (~120–200 B compressed).
- The UI shows network-wide file listings with a "held by" indicator.
- Fetch paths, in priority order: (1) direct IP from a holding BBS, (2) queued for the next sneakernet exchange, (3) mesh trickle — only for files under a sysop-configured cap (suggest 8 KB default), only with explicit approval, and charged against a separate quota.
- Be honest in the UI: show an ETA. "This 240 KB file will take approximately 14 hours over mesh" is a feature, not a failure.

### 7.6 The airtime governor

The most important piece of civic infrastructure in the system.

- **Token bucket** sized in *airtime seconds*, not bytes. Compute per-packet airtime from the active preset using the Semtech formula (Appendix A) — bytes are a bad proxy since airtime is superlinear in payload at low SF.
- **Configurable ceiling**, default **3%** of wall-clock airtime, hard max enforced in code (suggest 15%) so a sysop can't accidentally make the whole mesh hostile.
- **Read the node's own telemetry.** Meshtastic reports `channel_utilization` and `air_util_tx`. Above ~25% channel utilization, back off exponentially; above ~40%, transmit nothing but already-queued DM traffic.
- **Respect regional duty cycle.** EU 433/868 is limited to 10% duty over a rolling hour, enforced by firmware. Don't fight it — track it locally so we queue rather than getting rejected.
- **Quiet hours** — optional sysop-configured windows of zero transmission.
- **Priority classes:** control (digests, delta requests) > DM > forum posts > file catalog > everything else. Under backpressure, drop from the bottom.
- **Per-peer inbound quotas** so a rogue or malfunctioning BBS can't flood us — and log/alert the sysop when a peer exceeds them.

---

## 8. Security and trust

### 8.1 Threat model

The mesh channel PSK is shared by every BBS on the network (and Meshtastic's channel encryption has known limitations). **Treat the mesh as a public broadcast medium.** Anyone with the channel key — which includes every participating sysop — sees all traffic. Design accordingly:

- Everything is **signed**, so content can't be forged or tampered with in transit.
- Forum posts are **public by definition** — no confidentiality expectation, and the UI should say so.
- DMs are **end-to-end encrypted** so the shared channel key is irrelevant to them.

### 8.2 DM encryption

Per-user X25519 keys (derived from or paired with their Ed25519 identity). A DM body is sealed to the recipient's public key:

```
ephemeral_pubkey (32B) || ChaCha20-Poly1305(body) || tag (16B)   = 48B overhead
```

48 bytes is **21% of a mesh packet** — real, but acceptable, and it's the floor for anything with forward secrecy properties. Do *not* use `age`'s file format here (header, armor, and stanza overhead run to ~200 bytes); use `nacl/box` or an equivalent minimal construction directly.

Routing without reading: address DMs to `BLAKE3(recipient_pubkey)[:8]`. Intermediate nodes route and store without learning the recipient's identity beyond the tag. `❓Q12` — is metadata privacy (hiding *who is talking to whom* from other sysops) a requirement, or is content confidentiality enough? Full metadata privacy is substantially harder and costs airtime.

**Key discovery:** `PROFILE` records replicate nick + node + X25519 pubkey + signature (~100 B). A network-wide user directory that fits comfortably in the airtime budget. First-contact key verification is trust-on-first-use, with an optional short fingerprint users can compare out of band.

### 8.3 ⚠️ Encryption and amateur radio — a real legal constraint

If a sysop runs their node in Meshtastic's **ham mode** (`is_licensed=true`, which unlocks higher power on amateur allocations), they are operating under **FCC Part 97**, which **prohibits encryption intended to obscure the meaning of a communication**. E2E-encrypted DMs would be non-compliant.

On the default ISM allocations (US 902–928 MHz under Part 15, EU 868 MHz), encryption is fine.

The software should detect ham mode from the node config and either refuse to send encrypted DMs or require an explicit "I understand" override with a clear warning. Sysops should not stumble into an FCC violation because our defaults were convenient. `❓Q14`

### 8.4 Inter-BBS trust

- **Peer allowlist.** A sysop explicitly adds peer node keys. Unknown-origin records are dropped by default (configurable to "quarantine for review", which is nicer for network growth).
- **Per-peer quotas** on records/hour and bytes/hour.
- **Local moderation always wins.** Remote tombstones are advisory; local bans, filters, and area policy are authoritative.
- **No transitive trust in v1.** If A trusts B and B trusts C, A does *not* automatically accept C's records — though A will happily *relay* them if it's carrying an area they share. Records are signed end-to-end, so relaying is safe regardless of trust.

### 8.5 Local security

- Door sandboxing is the biggest risk surface — see §9.4.
- Rate-limit SSH auth; log and optionally auto-ban.
- Users' private keys: server-side storage means the sysop can technically read DMs. Be honest about this in the docs. An optional client-side key mode (user holds the key, decryption happens in a local helper) is possible but complicates the SSH-only UX substantially. `❓Q12`

---

## 9. Door games

The least tractable part of cross-platform BBS software, and worth being upfront about.

### 9.1 Tier 1 — modern doors (recommended primary path)

Define a clean contract and make it the first-class citizen:

- Door is any executable, any language
- Communicates over **stdin/stdout** (the BBS bridges the user's PTY)
- Session context via **environment variables** + a **JSON session descriptor** on a passed fd or temp file: user handle, real name, node number, time remaining, terminal size, ANSI capability, and a callback token
- Optional **local API socket** so doors can post to forums, send DMs, and read/write persistent per-user state — which is what makes inter-BBS door leagues possible (§9.5)

Works identically on all three OSes. Encourages new doors to be written in Go/Python/Node. This is where the project's leverage is.

### 9.2 Tier 2 — legacy DOS doors

The classics (LORD, TradeWars 2002, Barren Realms Elite, Usurper) are 16-bit DOS binaries that talk to a COM port and read a dropfile. Options:

| Approach | Platforms | Notes |
|---|---|---|
| **DOSBox-X subprocess** | Linux, macOS, Windows | Bridge `serial1=nullmodem server:127.0.0.1:PORT` to the session. **Recommended** — only genuinely cross-platform option. External dependency. |
| **dosemu2** | Linux only | Better performance and multi-instance behavior; `$_com1 = "virtual"`. Note FOSSIL drivers (BNU, X00) *interfere* and must not be loaded. |
| **Embedded x86 emulator (v86 via WASM)** | All | What ENiGMA½ does, giving zero external dependencies. In Go this means embedding a WASM runtime (`wazero` — pure Go, no cgo) and running v86. Genuinely attractive for single-binary distribution, but a significant engineering effort. Phase 4+ candidate. |

Dropfile generation for `DOOR.SYS`, `DOOR32.SYS`, `DORINFO1.DEF` is straightforward and should be supported regardless of emulator.

`❓Q15` — **How much do legacy DOS doors matter?** If they're essential, DOSBox-X bridging should be in Phase 2. If they're a nice-to-have, defer to Phase 4 and focus on the modern door API. This meaningfully changes the roadmap.

### 9.3 Windows-specific plumbing

Windows has no `fork`/`exec` with a PTY in the Unix sense. Use **ConPTY** (Windows 10 1809+) via `github.com/UserExistsError/conpty` or similar to give doors a real console. Budget real time for this; it's where cross-platform door support usually breaks.

### 9.4 Sandboxing

We're executing arbitrary binaries on the sysop's machine on behalf of remote users. Minimum bar:

- Dedicated low-privilege user account (documented, not enforced by us)
- Working directory confined per-door
- Resource limits: CPU time, memory, wall-clock, max concurrent instances
- Node locking — many DOS doors assume single-instance-per-node
- Never pass user-supplied strings into a shell

### 9.5 Inter-BBS doors — the payoff

Classic BBS culture had InterBBS leagues (LORD inter-BBS wars, TradeWars leagues) that exchanged score/battle files over FidoNet. **The same federation bus works here.** A `DOOR_EVENT` record type carries game events between instances, and suddenly you have inter-BBS door leagues over LoRa mesh with no internet. That's a genuinely novel thing that has never existed, and it's nearly free once §7 is built.

---

## 10. Cross-platform and packaging

- **Single static binary per platform.** `CGO_ENABLED=0` for the default build — which is exactly why `modernc.org/sqlite` matters (§4). BLE gets its own build tag and its own release artifacts.
- **Targets:** linux/amd64, linux/arm64 (Raspberry Pi — likely the most common deployment), darwin/arm64, darwin/amd64, windows/amd64.
- **Data layout:**
  ```
  <datadir>/
    config.toml
    bbs.db              # SQLite
    keys/               # node + host keys (0600)
    blobs/ab/cd/...     # content-addressed files
    themes/             # overridable art
    doors/              # door installs
    logs/
  ```
  Default datadir follows OS convention (`~/.local/share/meshbbs`, `~/Library/Application Support/MeshBBS`, `%APPDATA%\MeshBBS`), overridable.
- **Service integration:** systemd unit, launchd plist, Windows Service wrapper. Generate them with a `meshbbs install-service` subcommand.
- **First-run wizard:** generate keys, pick a node name, scan for a Meshtastic device, configure the channel, create the sysop account. Should take under two minutes.
- **Testing:** the `Link` abstraction lets us build a **simulated mesh harness** — N in-process BBS instances connected by a fake link with configurable MTU, latency, loss, and airtime budget. This is essential; you cannot iterate on a sync protocol by physically deploying radios. Meshtastic's own discrete-event simulator (`meshtasticator`) can validate RF-level assumptions later.

---

## 11. Roadmap

| Phase | Scope | Why this order |
|---|---|---|
| **0 — Skeleton** | Go module, SQLite schema + migrations, config, logging, key generation, CI cross-compiling all 5 targets | Prove the cgo-free build works on day one, before there's anything to port |
| **1 — Single-node BBS** | SSH server, Bubble Tea UI, menus, ANSI/CP437 rendering, users/auth, local forums, local DMs, file areas via SFTP, presence/node chat | A genuinely usable BBS with zero federation. Ship this; get people on it. |
| **2 — Federation over IP** | Record log, version vectors, anti-entropy, bundle format, zstd dictionary, `Link` abstraction, **simulated mesh harness**, QUIC/Noise IP link | Build and debug the sync protocol where iteration is fast — but design every byte for the mesh MTU |
| **3 — Meshtastic link** | Serial + TCP transports, protobuf framing, L1 fragmentation + ARQ, airtime governor, ham-mode safety checks, file catalog replication | The protocol already fits 233 bytes because Phase 2 was designed that way |
| **4 — Doors** | Modern door API, PTY/ConPTY bridging, dropfiles, DOSBox-X bridge for legacy DOS doors, sandboxing | Independent of federation; can run in parallel with 2–3 |
| **5 — Polish & reach** | BLE transport, fountain coding for multi-peer broadcast, web terminal, inter-BBS door events, theme packs, sneakernet bundles | The genuinely optional tier |

Phases 2 and 4 are parallelizable if there's more than one person working on it.

---

## 12. Open questions

Grouped by how much they'd change the design. The first four are the ones that block detailed design work.

### Would change the architecture

1. **`❓Q1` — ARQ or fountain codes for mesh reliability?** Fountain coding is arguably the "right" answer for a broadcast medium with independent per-receiver loss (§7.2), but it's more complex and the payoff scales with peer count. Simple thing first, or design for fountain coding from the start?
   1. Answer: do the right thing from the beginning - fountain coding.

2. **`❓Q8` — How many BBS instances on one mesh?** 5, 50, or 500? This determines whether version vectors suffice or whether we need real set reconciliation, and it changes the airtime budget per node substantially.
   1. I don't have a good notion, as the idea is new.  All of our local meshes in the Pacific Northwest have dozens or more nodes per mesh, but it's not likely to have that many BBSs.  50 at most? 

3. **`❓Q3` — Vendor the protobufs and write our own Meshtastic transport, or depend on an existing Go library?** Leaning strongly toward our own (~300 lines, no unstable dependency).
   1. Vendor the protobufs and roll our own.

4. **`❓Q15` — How important are legacy DOS door games?** Essential (Phase 2), nice-to-have (Phase 4), or skip entirely in favor of a modern door API? This is the single biggest swing in scope.
   1. It's nice-to-have

### Would change the protocol

5. **`❓Q9` — Node-signed or user-signed forum posts?** Node-signed saves 64 bytes/post (27% of a packet) and matches FidoNet's trust model. User-signed gives cryptographic non-repudiation but roughly doubles per-post overhead. Or: user-signed only for DMs, node-signed for forums (current recommendation)?
   1. Go with your recommendation

6. **`❓Q10` — Should remote deletes be honored automatically?** Auto-honoring is what users expect; ignoring them prevents a compromised node from nuking network-wide content. Proposed: advisory, sysop-configurable.
   1. Agree with your proposition

7. **`❓Q12` — Is metadata privacy a requirement for DMs?** Content confidentiality is straightforward. Hiding *who talks to whom* from other sysops is much harder and costs airtime. Related: is it acceptable for the sysop to technically be able to read DMs (server-held keys), or should keys be client-held?
   1. Content confidentiality.  Metadata/who talks to who is not critical.  Client held keys are preferrable. 

8. **`❓Q11` — File transfer over mesh: hard-block, or allow with quotas?** Proposed: allow tiny files (<8 KB) with explicit approval. Or make it impossible?
   1. No mesh file transfers.

9.  **`❓Q7` — Node addressing: opaque key-derived IDs, or FidoNet-style hierarchical numeric addresses?** Key-derived means no registry and no authority, which fits mesh. Numeric addresses are more familiar to BBS people and enable hierarchical routing.
    1.  I want it to be easy to understand how to access and address different nodes.  I'm leaning towards numeric addresses.  

10. **`❓Q2` — Register a Meshtastic portnum, or stay on `PRIVATE_APP` (256)?** Registering means other Meshtastic tools can identify BBS traffic; it also means committing to a stable wire format publicly.
    1.  Let's register a portnum and have a stable wire format...once we have it all hammered out and tested. 

### Operational / product

11. **`❓Q14` — Hard-block encrypted DMs when the node is in ham mode?** (§8.3.) Refusing outright is the legally safe default; an override with a warning is friendlier. Inclination: hard-block with a documented override flag.
    1.  Hard-block with documented override flag

12. **`❓Q5` — Telnet at all?** It's how actual BBS terminal clients (SyncTERM, NetRunner) connect, but it's plaintext. Off-by-default with a warning?
    1.  Off by default, with a warning is fine. 

13. **`❓Q4` — Is BLE support important enough to justify a separate cgo build?** USB and TCP cover essentially all realistic deployments; BLE adds meaningful build and support complexity.
    1.  bluetooth is not important enough. Let's leave that for future/never. 

14. **`❓Q13` — Any interest in FidoNet/FTN gateway compatibility?** There's a live FTN scene, and gatewaying would connect this to existing message networks. Real work, but the record model above maps onto FTN echomail reasonably cleanly.
    1.  Yes. 

15. **`❓Q6` — How customizable should the UI be?** Full ANSI theme packs the sysop can author (classic BBS expectation, more work), or a smaller set of built-in themes?
    1.  For now, let's leave a small set of built-in themes. 

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

Validation: SF11 / BW250 kHz / CR 4/5 (LongFast), PL=16 → **354 ms**, exactly matching Meshtastic's documented figure. PL=256 → **2.157 s** vs. their stated "~2 s".

Note that airtime is *superlinear* in payload size at high SF, which is why the governor must budget in airtime-seconds rather than bytes.

## Appendix B — Payload budget for a typical forum post

| Component | Bytes |
|---|---:|
| Meshtastic `Data` payload ceiling | 233 |
| L1 fragment header | −8 |
| L2 bundle header (dict id, count, flags) | −4 |
| Record header (id, origin, seq, ts, type, area, parent) | −51 |
| Ed25519 signature | −64 |
| **Remaining for compressed body** | **106** |
| → decompressed at 3.5× with trained dictionary | **≈ 370 chars** |

This is why batching matters: amortized across a 10-record bundle, the per-record fixed cost drops sharply and a typical post fits comfortably.

---

## References

- [Meshtastic — Radio Settings (presets, data rates, link budgets)](https://meshtastic.org/docs/overview/radio-settings/)
- [Meshtastic — Client API (Serial/TCP/BLE), framing and BLE GATT UUIDs](https://meshtastic.org/docs/development/device/client-api/)
- [Meshtastic — LoRa Configuration](https://meshtastic.org/docs/configuration/radio/lora/)
- [Meshtastic — Mesh Broadcast Algorithm](https://meshtastic.org/docs/overview/mesh-algo/)
- [Meshtastic — Encryption limitations](https://meshtastic.org/docs/about/overview/encryption/limitations/)
- [Meshtastic — Why your mesh should switch from LongFast](https://meshtastic.org/blog/why-your-mesh-should-switch-from-longfast/)
- [meshtastic/protobufs — mesh.proto](https://github.com/meshtastic/protobufs/blob/master/meshtastic/mesh.proto)
- [Port Numbers — PRIVATE_APP and the 256–511 range](https://deepwiki.com/meshtastic/protobufs/2.2-port-numbers)
- [Message Architecture — 233-byte payload limit](https://deepwiki.com/meshtastic/protobufs/2-message-architecture)
- [meshnet-gophers/meshtastic-go](https://github.com/meshnet-gophers/meshtastic-go)
- [lmatte7/goMesh](https://github.com/lmatte7/goMesh)
- [charmbracelet/wish — SSH apps](https://github.com/charmbracelet/wish)
- [charmbracelet/bubbletea](https://github.com/charmbracelet/bubbletea)
- [tinygo-org/bluetooth — cross-platform Go BLE](https://github.com/tinygo-org/bluetooth)
- [ENiGMA½ BBS — Features](https://enigma-bbs.github.io/features/)
- [ENiGMA½ — FidoNet-Style Networks (FTN)](https://nuskooler.github.io/enigma-bbs/messageareas/ftn.html)
- [tsali/dosemu2-bbs-doors](https://github.com/tsali/dosemu2-bbs-doors)
- [Synchronet wiki — DOS doors with dosemu](http://wiki.synchro.net/howto:dosemu)
- [FTSC — FidoNet technical standards](http://ftsc.org/docs/fsc-0070.002)
