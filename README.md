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

**Status:** Phase 0 (skeleton) in progress. Not yet a usable BBS — there is no SSH server until
Phase 1 and no federation until Phase 2. What works today is identity, configuration, storage and
the CLI.

## Try it

```
go build -o meshbbs ./cmd/meshbbs
./meshbbs init --display-name my-bbs --sysop-nick yournick --development
./meshbbs id
```

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
go run ./tools/checkdeterminism ./...   # enforces the design §12.1 constraints
```

The determinism check is not a style linter. Deterministic simulation is how the federation
protocol gets tested at all (design §12.1), and it depends on domain code never calling
`time.Now()` or package-level `rand`, and never letting map iteration order reach output bytes —
the last of which would silently corrupt record signatures.

## License

MIT
