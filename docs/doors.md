# MeshBBS — The Door API

*What a door is handed, what it may ask for, and what it will be refused.*

**Status:** Draft v0.1
**Date:** 2026-08-09
**Amends:** design.md §9.1.1 (announce area), §9.2 (dropfile location), §9.4 (limits), §10 (build tags), §11.5 (door keys)
**New decisions:** none — §9's decisions stand; this records how they were implemented and where reality forced a narrowing
**Open questions:** none

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
federated area spends the mesh's budget — around ten originated packets per node per day — on the
say-so of a third-party binary. Door activity that belongs on the mesh has a mechanism designed and
budgeted for it: `DOOR_EVENT` records (§9.5), in Phase 5.

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

To see the whole thing working before pointing it at somebody else's software:

```
meshbbs door examples
```

That installs `hello` and `guess`, which run from the meshbbs binary itself. `guess` uses levels 1,
2 and 3 and is the shortest complete example of the contract above.

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

**§11.5, door keys.** Two additions. `state_quota_bytes`, because §11.5 says level-2 state is
quota'd without saying by what, and a door that legitimately needs more should not need a rebuild.
And `enabled`, so a sysop can take a door off the menu without losing its configuration and the
saved games attached to it.

**§4, dependencies.** One addition: `Microsoft/go-winio`, for the Windows named pipe §9.1.1
specifies. Pure Go, cross-compiles cgo-free, and its only requirement is `golang.org/x/sys`, which
was already in the graph.

---

## 11. What is not here

**Inter-BBS doors.** `DOOR_EVENT` and the leagues of §9.5 are Phase 5. The record type is reserved
and deliberately has no body codec yet: adding one would be a wire-format commitment made in the
wrong phase.

**DOS doors.** Phase 7, and may never ship `[D4]`. The dropfile support above is the part §9.2 said
could land early, and it did.

**A sandbox.** §3.
