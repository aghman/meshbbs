# meshbbs

A modern, cross-platform BBS written in Go — SSH access, door games, file areas, forums, and
direct messaging — with forums and DMs federated between independent BBS instances over
[Meshtastic](https://meshtastic.org) LoRa mesh.

Each BBS instance has its own Meshtastic node attached (USB, TCP, or BLE) and syncs with peer
instances over a dedicated mesh channel. A BBS with no internet connection at all is a full
participant in the network.

**Status:** pre-implementation. Design in review.

## Documentation

- [High-Level Design](docs/design.md) — architecture, mesh sync protocol, airtime budget, roadmap,
  and open questions.

## License

MIT
