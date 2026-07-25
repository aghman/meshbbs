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

**Status:** pre-implementation. Design decisions resolved; Phase 0 not yet started.

## Documentation

- [High-Level Design](docs/design.md) — architecture, mesh sync protocol, airtime budget, roadmap,
  and the decision log.

## License

MIT
