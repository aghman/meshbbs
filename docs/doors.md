# MeshBBS — The Door API

*What a door is handed, what it may ask for, and what it will be refused.*

**Status:** Draft v0.2
**Date:** 2026-08-12
**Amends:** design.md §9.1.1 (announce area), §9.2 (dropfile location), §9.4 (limits), §10 (build tags), §11.5 (door keys, and the league grant), §9.5 (batching is the BBS's job, not the door's; emitting is not a new API level)
**New decisions:** none — §9's decisions stand; this records how they were implemented and where reality forced a narrowing
**Open questions:** none

*v0.2 adds §6.7, the inter-BBS league half of §9.5: `event.emit` and `event.poll`, specified at the wire level. It is a second axis on level 3 rather than a fifth level, and the reasoning is in §10. §11's placeholder for it is replaced by what the wire commitment turned out to be.*

---

## 1. Who this is for

Two audiences, and they want different things.

A **sysop** wants to know what installing a door lets it do to their board. §3 and §9 are for them,
and the short version is: a door runs with the server's privileges and can read whatever the server
can read, it cannot run forever, and it cannot leave anything behind.

A **door author** wants to know the contract. §4 through §7 are that, specified at the wire level so
that a door in Python, C or Rust has everything it needs. Go authors can import
`internal/door`'s client instead, but the wire protocol is the specification and the client is a
convenience.

design.md §9.1 asks for exactly this: "document the contract as a spec rather than leaving it
implicit in the implementation."

---

## 2. What a door is

Any executable, in any language, that talks over stdin and stdout. The BBS gives it a real
pseudo-terminal — a pty on Unix, a ConPTY on Windows — bridges the caller's connection to it, and
gets the connection back when the door exits.

That is the whole of the minimum. A door that prints something and reads a line is a working door;
the reference `hello` door is thirty lines and uses nothing but the session context.

Everything beyond that is optional:

| | What it is | Needed for |
|---|---|---|
| Session descriptor | A JSON file naming the caller and holding the API token | Anything past drawing on the screen |
| API socket | A Unix socket or named pipe | State, announcements, acting as the user |
| Dropfile | `DOOR.SYS`, `DOOR32.SYS` or `DORINFO1.DEF` | Doors written before the API existed |

A door may use none, one or all of them.

---

## 3. What a door cannot do

Stated first, because it is what a sysop is really asking.

**It cannot run forever.** Every door has a wall-clock limit and it is not optional. When it runs
out the door is asked to stop, and then made to.

**It cannot leave anything behind.** When a door ends, everything in its process group goes with it
— on Windows, everything in its job object. A door that forks a worker and exits cleanly does not
get to keep the worker.

**It cannot see the server's environment.** The environment is *replaced*, not extended. A door
sees a short platform base, the session variables, and whatever the sysop named in
`env_passthrough`. An API token in the server's unit file does not reach a door.

**It cannot exceed the caller.** A door acting as a user is subject to exactly what that user is
subject to — see §7.

**It cannot spend the mesh's airtime.** See §6.

What it **can** do is everything the server's account can do on that machine. This is not a sandbox
and design.md §9.4 does not claim it is one: the working directory is set, not confined, and the
environment allowlist is an allowlist, not isolation. §9.4's actual advice — run the BBS as a
dedicated low-privilege account — remains the thing that limits a door's reach, and it is the
sysop's to do.

---

## 4. Starting up

A door is launched with:

- **argv** as configured, with `{dropfile}` and `{dropfile_dir}` substituted if present
- **stdin, stdout, stderr** attached to a pseudo-terminal
- **the working directory** set to the door's configured `cwd`
- **an environment** of the platform base, plus:

| Variable | Meaning |
|---|---|
| `MESHBBS_DOOR_DESCRIPTOR` | Path to the session descriptor (§5) |
| `MESHBBS_DOOR_SOCKET` | Address of the API socket |
| `MESHBBS_DOOR_API_LEVEL` | The level this door was granted, 1–4 |
| `MESHBBS_DOOR` | This door's name |
| `MESHBBS_USER` | The caller's handle, empty for a guest |
| `MESHBBS_REAL_NAME` | The caller's real name, if they gave one |
| `MESHBBS_NODE` | BBS node number |
| `MESHBBS_COLUMNS`, `MESHBBS_LINES` | Terminal size, also as `COLUMNS` and `LINES` |
| `MESHBBS_TIME_LEFT` | Seconds left in the session; **absent** when there is no limit |
| `MESHBBS_DROPFILE`, `MESHBBS_DROPFILE_DIR` | Present only when a dropfile was written |

**The token is not in the environment**, and never will be. §9.1.1 is explicit about why: argv and
the environment of a running process are readable by anything that can run `ps`.

`MESHBBS_TIME_LEFT` is **absent** rather than zero when the session has no limit. A door that reads
zero and shows the caller a goodbye they have not earned is the bug this avoids.

---

## 5. The session descriptor

JSON, mode 0600, in a directory mode 0700 that is created for this launch and deleted when it ends.

```json
{
  "version": 1,
  "door": "tradewars",
  "socket": "/tmp/mbdoor-1a2b3c/api",
  "token": "9f86d081884c7d659a2feaa0c55ad015...",
  "level": 3,
  "session": {
    "handle": "alice",
    "real_name": "Alice Anderson",
    "node": 3,
    "width": 80,
    "height": 24,
    "terminal": "xterm-256color",
    "ansi": true,
    "encoding": "cp437",
    "time_limited": true,
    "time_remaining_secs": 2400
  }
}
```

`version` is 1 and a door **should refuse** a version it does not know rather than guess. It exists
so that a format change is something a door can detect.

The `session` block is a snapshot taken at launch. A door that cares about time remaining should ask
the socket, because that number moves.

---

## 6. The API socket

A Unix domain socket, or a named pipe on Windows (`\\.\pipe\meshbbs-door-…`). On Unix the socket is
mode 0600 inside the 0700 directory; on Windows the pipe carries a security descriptor granting the
owner and Local System and nobody else.

The protocol is **newline-delimited JSON**, one request per line, one response per line, in order.
A request is at most 64 KiB.

### 6.1 Saying hello

The first message on a connection must be:

```json
{"id": 1, "op": "hello", "token": "<from the descriptor>"}
```

```json
{"id": 1, "ok": true, "level": 3}
```

Anything else first, or a wrong token, and the connection is closed with no second attempt. The
reply does not say which of the two was wrong: an oracle that distinguishes them is a token-guessing
tool.

A door has 30 seconds to say hello before the connection is dropped.

`level` is worth reading rather than assuming. A door that knows it has level 2 can hide the feature
it would otherwise offer and fail at.

### 6.2 Requests

Every response carries the `id` of its request, and `ok`. When `ok` is false there is an `error` in
prose and a `code` to branch on:

| `code` | Meaning | What a door should do |
|---|---|---|
| `forbidden` | You may not | Do not retry. Show the message if it concerns the player |
| `rate_limited` | Too often | Try later, or not at all |
| `quota` | Out of saved-state room | Delete something and retry |
| `bad_request` | Malformed, or an unknown operation | Fix the door |
| `internal` | The BBS failed | Retry once, then give up gracefully |

The distinction between `forbidden` and `internal` is the one that matters. A refusal is an answer;
a fault is not. A door that cannot tell them apart retries the one thing that will never succeed.

### 6.3 Level 1 — `session`

Always available.

```json
{"id": 2, "op": "session.get"}
```

Returns the same `session` object as the descriptor, read **now**. `time_limited` says whether
`time_remaining_secs` means anything; a door must not read zero as "out of time" without it.

### 6.4 Level 2 — `state`

Always available. A private key/value store, in two scopes:

- `"user"` — private to the caller. **The default**, and a door cannot name anyone else: there is no
  field for a nick, which is what "no cross-user reach" means in practice.
- `"global"` — shared between this door's players. A high-score table.

```json
{"id": 3, "op": "state.set", "scope": "user", "key": "save", "value": "sector=42"}
{"id": 4, "op": "state.get", "scope": "user", "key": "save"}
{"id": 5, "op": "state.delete", "scope": "global", "key": "record"}
{"id": 6, "op": "state.keys", "scope": "global"}
```

`state.get` answers with `value` and `found`. A key never written is **not an error** — a door
asking for a saved game on a player's first run is the ordinary case.

Keys are at most 64 bytes, values at most 4096, and the door's total is bounded by its quota. Past
it, `state.set` answers `quota`.

A guest session has no account, so `scope: "user"` is refused for one. `global` still works: it
belongs to the door, not the player.

### 6.5 Level 3 — `announce`

Always available, rate-limited, and only if the sysop chose an area.

```json
{"id": 7, "op": "announce", "subject": "New champion", "text": "alice won"}
```

Posts **as the door**, never as the player. The author is the door's name behind a marker that no
account can be spelled with, so a post is attributable to software rather than to a person.

**The announce area must not be federated.** design.md §9.1.1 says a sysop *can* point a door at a
local-only area; this implementation *requires* it, and the reason is §11.4: a sysop gets wide
latitude over their own instance and narrow latitude over shared airtime. An announcement on a
federated area spends the mesh's budget — around ten originated packets per node per day at the
guessed R of 4 — on the say-so of a third-party binary. Door activity that belongs on the mesh has a
mechanism designed and budgeted for it: `DOOR_EVENT` records, which are §6.7 below.

### 6.6 Level 4 — `act_as_user`

**Granted per door by the sysop, off by default.**

```json
{"id": 8, "op": "user.post", "area": "general", "subject": "I won", "text": "…"}
{"id": 9, "op": "user.dm", "to": "bob", "subject": "hi", "text": "…"}
```

Three rules attach, and all three are enforced rather than requested:

**Capabilities intersect, never escalate.** These calls go through the same code path the menu uses,
with the caller's own nick, so the caller's capability checks apply unchanged. A door running for a
user who lacks `post_federated` cannot federate on their behalf. There is no path here that skips
the user's own checks, which is a stronger guarantee than a check that could be forgotten.

**Every action is audit-logged**, with the door, the user and the record produced.

**The user is told.** The first time a given door acts as a given user, the response carries a
`notice` string. **A door must show it.** Nothing can force that — the terminal belongs to the door
— so it is stated here, the audit log holds the sysop's copy either way, and a door that swallows
the notice is a door to uninstall.

### 6.7 Level 3 + a league grant — `event.emit` and `event.poll`

**Available at level 3, and only if the sysop named a *federated* door area.** This is not a fifth
level; §10 says why. It is a second axis on level 3, exactly as `announce` is, and the mirror image
of it: `announce` requires a **local** area because a door must not spend the mesh's airtime on its
own say-so, and a league requires a **federated** one because spending it is the entire feature. A
door with no league area is refused `forbidden` for both operations, which is a different thing from
a rate limit of zero.

A league is an area (design.md §9.5, §6.3), so the same `meshbbs area` commands create it, federate
it and cap what it may spend. A door does not learn any of that: it names nothing but its game.

#### Emitting

```json
{"id": 10, "op": "event.emit", "game": "lord", "kind": 3, "to": "bob@pnw", "payload": "CQk="}
```

```json
{"id": 10, "ok": true, "queued": true}
```

| Field | Rule |
|---|---|
| `game` | Required, at most **16 bytes**. A key, not prose: it is matched exactly, so every sysop in a league must type it identically, and it is hoisted onto the record so it is paid once per batch rather than once per event |
| `kind` | 0–255, door-defined. MeshBBS cannot enumerate any door's events and will not try; an unknown code is forward-compatible by construction, and a receiver that does not know code 7 renders "event 7" and passes the payload on |
| `to` | Optional. A nick, or `nick@node` in any form `user.dm` accepts — resolved by the same call, so it means here what it means there. Unresolvable is `bad_request` |
| `payload` | Optional, **base64** of at most **48 raw bytes**. JSON strings are text and this is arbitrary bytes |

**There is no `actor` field and there will not be one.** The actor is the session's own nick,
always, for the same reason `state.get` has no field for a nick. A door that could name the actor
would let any board credit any result to anyone — and unlike a forged post, nobody local would ever
see it happen. Every field on the request has been tried as a way in and each one is queued under
the session's nick.

**A guest cannot emit.** A guest has no name to attribute a result to, and inventing one puts an
unaccountable actor on other people's mesh. It is `forbidden`. Polling is a different matter — see
below.

**The target is a claim, not a verified fact.** It has to exist, because a league exists so that
"alice slew bob@pnw" can cross boards, and it cannot be inferred from the record's origin the way
the actor's board can. What travels is the resolved node ID; local aliases never do (design.md
§6.1.4.1). But this node is *asserting* that something happened to somebody on another board, and
design.md §9.5 already says what that is worth: leagues rest on sysop-to-sysop trust, and a league
wanting integrity against its own members needs commit-reveal or cross-checked game state, not a
signature layer.

**`queued` is the whole answer.** There is no record ID and no estimate of when it will go out, and
both omissions are deliberate. Nothing has been signed at this point: the BBS batches events and
publishes them when the league area's share of the airtime budget allows, which may be hours. §6.5
spent a paragraph on why a promise nothing will keep is worse than a spinner, and an id printed for
an unsigned record would be exactly that. A door that wants to tell the player something should say
it was reported, not that it was sent.

Batching is the BBS's job. A door could not do it anyway — a door process exists only while somebody
is playing — and asking third-party binaries to be careful with a commons is asking the wrong party.

**48 bytes of payload is the limit, and it is not a tuning knob.** It is what makes design.md §7.5
true by arithmetic rather than by inspection: at 48 bytes an event, 8 events a record, and a league
area's share of the budget — on the order of one packet a day at fifty instances and the guessed R
of 4 — dripping a megabyte of file content through payloads takes on the order of decades. The
figure moves with R, which is unmeasured; the conclusion does not. The refusal says "a door that
needs more than this is doing state synchronisation, which is not what this is", because "limit is
48" invites a workaround and that does not. A door with real state to move has level 2 for the local
half and no mesh answer for the rest, by design.

The payload is opaque and is **not** checked for control characters, unlike every rendered field —
a door may legitimately put anything in it. That is precisely why nothing prints one raw, and a door
rendering another board's payloads should treat them as hostile bytes.

**Refusals**, all distinguishable by `code`:

| `code` | Cause |
|---|---|
| `forbidden` | No league area; the area is not a federated door league; the session is a guest |
| `bad_request` | No game; a payload that is not base64; a target that resolves to nobody; anything the wire format would reject |
| `rate_limited` | Past `--league-per-hour` for this door |
| `quota` | The queue is full — 100 events per door — which means nothing is draining it |

The response also carries the same one-time `notice` level 4 uses, on the same terms: a door putting
a player's nick on other people's mesh is the same category of surprise as one posting as them.

#### Polling

```json
{"id": 11, "op": "event.poll", "game": "lord", "after": 128}
```

```json
{"id": 11, "ok": true, "cursor": 141,
 "events": [
   {"origin": "K7QM4X2P…", "at": 1765000000, "kind": 3,
    "actor": "alice", "target": "bob", "target_node": "R3TF9WQ2…", "payload": "CQk="}
 ]}
```

`game` filters; omitting it returns everything on the league, which is what a sysop-facing door or a
board carrying more than one game wants. `origin` and `target_node` are rendered node IDs — a door
gets strings it can print and compare and never has to know how eight bytes become a name.

**The cursor is the door's to keep, in its own level-2 state.** The BBS tracks no per-door read
position. That keeps the operation stateless and means two invocations of the same door cannot fight
over one. Pass `0` to start from everything held; pass back the `cursor` you were given to continue.
A poll returns at most 200 records' worth of events, so a door catching up should keep polling until
the cursor stops moving.

**It is arrival order, not `(origin, seq)`.** That coordinate is what anti-entropy reconciles on and
it is the wrong thing to deliver by: records arrive out of order on a mesh as a matter of course — a
bundle repaired hours after the one that followed it, a peer back from a week offline — and a door
that remembered "seen up to seq 40 from pnw" would step straight past record 37 when it finally
landed and never ask again. The cursor is this node's own arrival counter, which only ever goes up.

**`truncated` means retention got there first**: records this cursor had not reached have been
pruned, and no amount of further polling will fill the gap. A door that ignores it shows an
incomplete league table and calls it complete. It is the one flag here a door must actually branch
on.

**A guest may poll.** Emitting needs a name to attribute a result to and reading a scoreboard does
not, and refusing would mean a door could not show the standings to somebody browsing.

**A door is not called; it polls.** Doors are not servers, and an event that crosses the mesh at 3am
arrives at a board where nothing is running. The log is the delivery mechanism and a door drains it
on next launch.

---

## 7. Dropfiles

For doors that predate all of the above. One of `DOOR.SYS` (52 lines), `DOOR32.SYS` (11) or
`DORINFO1.DEF` (13), written before the door starts, in the same private directory as the
descriptor and gone when the door ends.

design.md §9.2 assumes the DOS-era convention of a per-node directory. This writes
**per-invocation** instead, which is the same idea with a stronger guarantee — two callers cannot
collide, and neither can one caller with themselves. Doors are told where it is by
`{dropfile}`/`{dropfile_dir}` in their arguments or by `MESHBBS_DROPFILE`.

Two things a door author should know:

**`DOOR.SYS` line 14 is empty.** That field is the caller's password in the clear. MeshBBS does not
have it — passwords are Argon2id hashes — and would not write it into a file for a third-party
binary if it did. The line is present because the format is positional.

**The security level is a projection, not a permission.** Guest 10, user 50, sysop 100. MeshBBS has
capabilities, not levels; this number exists because the format demands one. Whether a caller may
run a door was settled before it started, by `run_doors` and the door's `required_capability`. A
door making access decisions from this number is trusting a value it was handed rather than one it
checked.

---

## 8. Limits, and what each platform can enforce

Measured, not assumed. The answers are not the ones the manual pages suggest.

| | Linux | macOS | Windows |
|---|---|---|---|
| Wall clock | yes | yes | yes |
| CPU | yes | yes | yes |
| Memory | yes | **no** | yes |
| Graceful stop before the kill | yes | yes | **no** |

**Memory cannot be limited on macOS.** Setting `RLIMIT_AS` or `RLIMIT_DATA` returns `EINVAL`, and a
child asked to allocate 256 MiB against a 64 MiB limit gets it. A door configured with a memory
limit therefore **refuses to launch** on macOS, with the platform named. Running it without the
limit was the one option not available: a sysop who set a memory limit believes there is one.

**A door written in Go ignores the CPU limit.** The Go runtime delivers `SIGXCPU` to a listener and
ignores it when there is none. Measured: a Go child spun ten seconds against a one-second limit and
was never signalled, while `awk` died in one. This is survivable because the CPU limit is not the
safety net — the wall-clock limit is, it is required, and it applies to every door whatever it is
written in.

**Windows has no polite stop.** A console control event must come from a process attached to the
door's console, and the server is attached to its own. So a door that has run out of time is
terminated rather than asked, and a door that wanted to save state on the way out does not get to.

On Unix the limits are applied by the server re-executing itself behind a sentinel argument, setting
the limits, and then `exec`ing the door in place. There is no wrapper process: same pid, same
process group. On Windows the job object carries them.

---

## 9. Installing a door

```
meshbbs door add tradewars /opt/doors/tw/tw2002 \
    --cwd /opt/doors/tw --limit 90m --node-lock --api-level 3 \
    --announce-area games
```

Door configuration lives in the database, not `config.toml`, as design.md §11.5 specifies. Adding a
door is routine, and `config check` refuses to start a BBS whose config file does not parse — a door
row with a mistake in it should disable that door, not the board.

A door that plays in an inter-BBS league (§6.7) needs two more flags and an area to point them at:

```
meshbbs area create lordleague --kind door
meshbbs area federate lordleague
meshbbs area share lordleague 0.10

meshbbs door add lord /opt/doors/lord/lord \
    --cwd /opt/doors/lord --limit 60m --node-lock --api-level 3 \
    --league-area lordleague --league-per-hour 6
```

`--league-area` is the federated door league this door reports to and reads back. Empty — the
default — means it may not emit at all, which is a different thing from a rate limit of zero.
`--league-per-hour` (default 6) bounds how often it may emit; past it the door gets `rate_limited`.

The area must exist, be `--kind door`, and be federated, or every emit is refused `forbidden`. The
number to look at afterwards is not the share but what the share buys, which `meshbbs serve` prints
per area in full packets per day — and which scales with R, still a guess of 4 (design.md `N10`).
`meshbbs door events` shows what each league is carrying, what is waiting to go out, and which doors
are pointed at it.

To see the whole thing working before pointing it at somebody else's software:

```
meshbbs door examples
```

That installs `hello`, `guess` and `arena`, which run from the meshbbs binary itself. `guess` uses levels 1,
2 and 3 and is the shortest complete example of the contract above. `arena` is the one that exercises
§6.7: it reports a result, reads the league back, and keeps its own cursor and running tally in level-2
state. It needs a league to report to, so `door examples --league-area <name>` checks that the area exists,
is of kind `door` and federates before installing anything — and without one it installs `arena` anyway and
says what is missing, since a door with no league is refused at the API rather than broken.

---

## 10. Amendments to design.md

Recorded here rather than folded in silently.

**§9.1.1, announce area.** Narrowed from "a sysop can point a door at a local-only area" to "must".
§6.5 above gives the reasoning.

**§9.1.1, the rendered author.** §9.1.1 renders a door announcement as `TRADEWARS
(door@K7QM4X2P…)`, which is 26 characters. A post's author field is 16 bytes, fixed by §6.2's budget
against a 233-byte MTU. The store keeps a marker and the door's name; the front end renders the
rest — which is where it belonged anyway, since the node ID half says which BBS you are reading
rather than who wrote the post. A door that announces is refused a name longer than 15 characters
when it is installed.

**§9.2, dropfile location.** Per-invocation rather than per-node. §7 above.

**§9.4, limits.** Enforced where the platform allows and refused where it does not, per §8 above.
§9.4's list does not say what happens when a platform cannot deliver one.

**§10, "no build tags".** §10 says the BLE decision leaves "no build tags, no cgo variants, no split
release artifacts". The first clause is now false: the pty and the API socket have `_unix`/`_windows
` file pairs. The claim was about **cgo variants and split release artifacts**, and both of those
still hold — one build matrix, one set of binaries, `CGO_ENABLED=0` everywhere. `creack/pty` and
`charmbracelet/x/conpty` were both already in the module graph and both cross-compile cgo-free.

**§11.5, door keys.** Four additions. `state_quota_bytes`, because §11.5 says level-2 state is
quota'd without saying by what, and a door that legitimately needs more should not need a rebuild.
`enabled`, so a sysop can take a door off the menu without losing its configuration and the saved
games attached to it. And `league_area` with `league_per_hour` (§6.7), which §11.5's door row had no
equivalent of at all — it lists `announce_area` and `announce_rate_limit` and stops there, because
§9.5 was written before there was a grant to hang a league on.

**§9.5, emitting is not a new API level.** §9.1.1's levels **nest** — 4 can do everything 3 can — so
a fifth level above `act_as_user` would assert that reporting a game result is *more* authority than
posting as the user, which is false. A league is a second axis on level 3 instead, exactly like the
announce area, and the two are mirror images: `announce_area` must be local because a door must not
spend the mesh's airtime on its own say-so, `league_area` must be federated because spending it is
the feature. That makes design.md §11.4's asymmetry visible in the grant rather than only in a
document, and it means a sysop who has named a federated door area has made the decision explicitly.

**§9.5, `required_capability` is the user-level gate.** §9.5 says nothing about which *users* may put
events on somebody else's mesh, and nothing in §6.7 gates one: a league grant is per door, and any
account that can run the door can emit. A sysop who wants a narrower rule already has the knob —
`required_capability` on the door row (§7), which is checked before the door starts and therefore
before any of this is reachable. There is deliberately no second, league-specific capability: two
gates for one decision is how a board ends up with one of them wrong.

**§4, dependencies.** One addition: `Microsoft/go-winio`, for the Windows named pipe §9.1.1
specifies. Pure Go, cross-compiles cgo-free, and its only requirement is `golang.org/x/sys`, which
was already in the graph.

---

## 11. What is not here

**~~Inter-BBS doors.~~** v0.1 said the `DOOR_EVENT` record type was reserved and deliberately had no
body codec, because adding one would be a wire-format commitment made in the wrong phase. Phase 5
made it, and it is worth saying exactly what was committed to, since `[D10]` freezes the wire format
in Phase 6 and this is one of the things being frozen: a body is a game name of at most 16 bytes and
up to 8 events, each carrying a `uint8` kind, an actor and optional target of at most 24 bytes, an
8-byte target node when there is a target, and at most 48 opaque payload bytes. There is not a
single varint in it — every bound fits in one byte — because the `FILE` codec's first fuzz run found
an overlong varint giving one logical entry two wire forms and therefore two content addresses that
anti-entropy would have carried apart forever. A format with no varints cannot have that bug rather
than merely defending against it. §6.7 is the door-facing half; the arithmetic that forced the shape
is in design.md §9.5.

What is **not** committed to is any meaning for `kind` or for the payload. MeshBBS cannot model
TradeWars' state or enumerate LORD's events and does not try, and a receiver that meets a code it
does not know renders "event 7" and passes the payload on.

**A built-in league scoreboard.** There is none, in the TUI or anywhere else, and it was left out
rather than missed: a scoreboard is what a *door* renders, and it renders it better, because only
the door knows what kind 3 means. The case a built-in listing would serve is a league with no door
installed — which is the case where there is nothing to play. What did get built is the sysop
surface, `meshbbs door events`, because the sysop is who pays for carrying a league.

**DOS doors.** Phase 7, and may never ship `[D4]`. The dropfile support above is the part §9.2 said
could land early, and it did.

**A sandbox.** §3.
