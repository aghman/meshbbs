# meshbbs

A modern, cross-platform BBS written in Go — SSH and browser access, door games, file areas,
forums, and direct messaging — with forums and DMs federated between independent BBS instances
over [Meshtastic](https://meshtastic.org) LoRa mesh.

Each BBS instance has its own Meshtastic node attached (USB serial or TCP) and syncs with peer
instances over a dedicated mesh channel. A BBS with no internet connection at all is a full
participant in the network.

Each instance's address is derived from its own identity key — self-certifying, with no registry
and no address authority — and sysops give peers short local names (`austin@pnw`) the way you'd
name hosts in an SSH config. A gateway can bridge selected echo areas to existing FidoNet-style
message networks.

**Status:** Phase 4 is code-complete — door games run over SSH and telnet, with the §9.1.1
capability model, resource limits, dropfiles, two bundled reference doors and a written
[spec](docs/doors.md). Phase 3 is code-complete — a usable single-node BBS, the sync protocol and its
simulator, federation over IP, and the Meshtastic link with its airtime governor, ham-mode safety
checks, `mesh survey`, and file catalog replication.

Two honest caveats. What has been demonstrated on real radios is the *link*: two instances
discovering each other, broadcasting and attributing packets over LoRa. A full record sync between
two instances over the air has not been recorded yet. And the airtime governor still runs on a
guessed flood multiplier of 4 — every airtime figure in the design scales linearly with it, and
`mesh survey` exists to measure the real number but nobody has yet.

The **browser front end** shipped ahead of its slot in the roadmap and works today. It is not a
terminal emulator in a web page: the session model emits a typed description of what is on screen,
and an ANSI renderer and an HTML renderer both consume it. One menu graph, so a screen cannot exist
over SSH and be missing from the web — and the browser wraps a post at a readable measure where SSH
cuts an area name at column 26. See [docs/webui.md](docs/webui.md).

## Try it

```
go build -o meshbbs ./cmd/meshbbs
./meshbbs init --display-name my-bbs --sysop-nick yournick --sysop-password-stdin
./meshbbs serve
```

Then connect:

```
ssh new@localhost -p 2222      register — your SSH key is enrolled automatically
ssh yournick@localhost -p 2222 log in
ssh guest@localhost -p 2222    browse read-only
sftp -P 2222 yournick@localhost   file areas
```

Forums, file areas, private mail, node chat and a sysop panel. Private messages are encrypted with
a key only your passphrase opens — subject lines included — so the sysop stores ciphertext and
cannot read your mail.

**File areas replicate their catalog, never their contents.** A federated file area puts its
listing on the mesh, so every BBS on the network can see what exists and which instance holds it —
but the files themselves never travel over LoRa at any size. That is a property of the record
format rather than a setting: every field of a catalog entry is bounded, so the largest one has a
211-byte body against the 8 KiB a record may carry — there is nowhere in one to put a file. Browse with `F` from the main menu, and move files with an ordinary SFTP client. A file
held by another BBS says so, and says which one, instead of offering a download that could not work.

The same BBS is also reachable from a browser. That front end is off until you give it a public
origin and a TLS certificate, because neither has a sensible default — passkeys are bound to the
origin, and a mismatch fails every sign-in with a browser error that says nothing about the cause:

```
[web]
enabled  = true
origin   = "https://bbs.example.com"   # required; passkeys bind to it
tls_cert = "/etc/meshbbs/fullchain.pem"
tls_key  = "/etc/meshbbs/privkey.pem"
```

Sign-in is a passkey and nothing else — no password path, and discoverable credentials mean you do
not type a nick either. An account that predates the web gets its first passkey by pressing `P` in
an SSH session and typing the code it shows into the sign-in page; the code registers a credential,
expires in ten minutes, and cannot mint a session. There is deliberately **no guest browsing on the
web** — an unauthenticated visitor sees a sign-in prompt and nothing else, while `ssh guest@` is
unaffected.

`init` generates the node key — there is no address to choose, request or register, because the
node ID is derived from the key itself. Back up the `keys/` directory: a lost node key cannot be
recovered, and the instance would have to re-establish with its peers as a new node.

```
./meshbbs user add bob                    # new accounts cannot post to federated areas by default
./meshbbs user grant bob post_federated
./meshbbs area create utils --files   # a file area; --federated puts its catalog on the mesh
./meshbbs file describe utils NOTES.TXT "Repeater notes"   # SFTP cannot carry one
./meshbbs peer alias pnw <node-id>    # local petname; never travels on the wire
./meshbbs config reference            # every setting, generated from the source
```

## Documentation

- [High-Level Design](docs/design.md) — architecture, mesh sync protocol, airtime budget, roadmap,
  and the decision log.
- [The Door API](docs/doors.md) — what a door is handed, what it may ask for, and what it
  will be refused. The contract a door author writes against, specified at the wire level.
- [Web UI Design](docs/webui.md) — the semantic-terminal shape, the block vocabulary, passkey
  authentication and enrolment. Owns §5.3 of the design doc in detail.
- [Configuration reference](docs/config.md) — generated; run `meshbbs config reference` for the
  same content.

### Keeping your own message key

By default the BBS holds your message key, wrapped under a passphrase it sees only while you are
logged in (§8.2 tier 2). If you would rather it never held the key at all, `meshbbs-key` keeps it
on your machine:

```
meshbbs-key init                       # generates a key, prints the public half
```

Give the sysop the public half; they run `meshbbs user dm-key <your nick> <key>`. From then on the
BBS seals your mail to that key and cannot read it. Reading becomes a copy and a paste:

```
meshbbs-key open                       # paste the block, press Ctrl-D
```

Two things to know before you do this. **Back the key file up** — it is the only copy, and without
it every message ever sent to you is unreadable, including ones already in your inbox. And reading
mail stops being something the BBS does for you: every message is a copy, a paste and a passphrase,
on a machine that is not the one you are logged into. That is the trade.

## Development

```
go test ./...
go run ./tools/checkdeterminism ./...        # enforces the design §12.1 constraints
go test ./internal/tui/ -update              # regenerate golden screen frames
```

Building needs nothing but a Go toolchain. The Meshtastic protobuf bindings in
`internal/meshtastic/meshpb` are generated but committed, so neither `protoc` nor the
`third_party/meshtastic-protobufs` submodule is required to build or test. They are needed only to
regenerate:

```
git submodule update --init third_party/meshtastic-protobufs
scripts/genproto.sh
```

With a Meshtastic node plugged in:

```
./meshbbs mesh ports                   # which serial port is the radio on
./meshbbs mesh info                    # what the radio says about itself
./meshbbs mesh info --tcp mesh.local   # or a node on WiFi
```

The determinism check is not a style linter. Deterministic simulation is how the federation
protocol gets tested at all (design §12.1), and it depends on domain code never calling
`time.Now()` or package-level `rand`, and never letting map iteration order reach output bytes —
the last of which would silently corrupt record signatures.

## License

MIT
