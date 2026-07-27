# meshbbs

A modern, cross-platform BBS written in Go — SSH access, door games, file areas, forums, and
direct messaging — with forums and DMs federated between independent BBS instances over
[Meshtastic](https://meshtastic.org) LoRa mesh.

Each BBS instance has its own Meshtastic node attached (USB serial or TCP) and syncs with peer
instances over a dedicated mesh channel. A BBS with no internet connection at all is a full
participant in the network.

Each instance's address is derived from its own identity key — self-certifying, with no registry
and no address authority — and sysops give peers short local names (`austin@pnw`) the way you'd
name hosts in an SSH config. A gateway can bridge selected echo areas to existing FidoNet-style
message networks.

**Status:** Phase 2 complete — a usable single-node BBS, plus the sync protocol and its simulator,
with instances able to federate over IP. Phase 3 is in progress: the Meshtastic link itself, so
federation over the mesh does not work yet.

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

Forums, private mail, node chat and a sysop panel. Private messages are encrypted with a key only
your passphrase opens — subject lines included — so the sysop stores ciphertext and cannot read
your mail.

`init` generates the node key — there is no address to choose, request or register, because the
node ID is derived from the key itself. Back up the `keys/` directory: a lost node key cannot be
recovered, and the instance would have to re-establish with its peers as a new node.

```
./meshbbs user add bob              # new accounts cannot post to federated areas by default
./meshbbs user grant bob post_federated
./meshbbs peer alias pnw <node-id>  # local petname; never travels on the wire
./meshbbs config reference          # every setting, generated from the source
```

## Documentation

- [High-Level Design](docs/design.md) — architecture, mesh sync protocol, airtime budget, roadmap,
  and the decision log.
- [Configuration reference](docs/config.md) — generated; run `meshbbs config reference` for the
  same content.

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
