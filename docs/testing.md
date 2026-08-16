# How meshbbs is tested

This is a contributor document. Nothing here is needed to run a node — see
[the project site](https://aghman.github.io/meshbbs/running.html) for that.

## Deterministic simulation is the centrepiece

It is how the federation protocol gets tested at all. Fifty simulated instances,
lossy links, partitions and clock skew are cheap in a simulator and impossible
to arrange with radios; nearly every measured number the project quotes —
convergence times, channel utilisation, the coding-overhead constants, the
publish ceiling — came out of it rather than a whiteboard. Several of those
numbers contradicted what an earlier draft of the design assumed, which is the
whole reason it exists.

Determinism is a property the domain code has to hold up, and it is easy to
break by accident. Three rules:

- No `time.Now()` in domain code. The clock is injected.
- No package-level `rand`. Randomness is seeded and passed in.
- Map iteration order never reaches output bytes.

The last one is the dangerous one: map order leaking into a canonical encoding
silently corrupts record signatures, and the failure appears as a peer rejecting
records rather than as anything local.

A checker enforces all three.

```
go test ./...
go run ./tools/checkdeterminism ./...
```

## Golden screens

The terminal interface is snapshot-tested. Regenerate the goldens after an
intentional change and read the diff:

```
go test ./internal/tui/ -update
```

## Frozen wire-format vectors

Every other test that touches an encoder is a round trip: encode, decode, check
you got back what you started with. That proves the two halves agree with each
other and says nothing about *which* bytes they agreed on. Flip the record
timestamp to little-endian on both sides and the entire suite stays green.

So the bytes themselves are frozen — 93 vectors covering node IDs, the record
envelope, each typed body, bundle framing, the gossip messages, the fountain
symbol headers and masks, and the signed announcements:

```
make conformance
```

**If this fails, do not regenerate it.** The bytes on the mesh have changed, and
regenerating turns that into a green test and a diff nobody reads closely. Find
the encoder change. If it was intended, it is a wire-format change and needs a
version bump, not an edited expectation. The generator refuses to help:

```
make vectors        # appends new vectors; refuses to alter an existing one
```

The vectors carry their own inputs, so they are also what a third-party
implementation would test against. Full schema in
`internal/conformance/testdata/v1/README.md`.

## The compression dictionary

`internal/bundle/dict1.zdict` is a committed binary artifact, trained from
`internal/dictcorpus`. Its ratio is gated, so a change that helps one record
type and hurts the rest gets caught, and so does one that makes any bundle
*larger* — plain zstd does that to a lone catalog entry, and fixing it is much of
why a dictionary is carried.

The corpus has a train half and a holdout half that share no content. Every
ratio quoted anywhere comes from the holdout half; measured on its training data
the same dictionary reads 3.19x against a real 1.41x, so the split is not
ceremony.

Retraining ships a **new** dictionary ID and a new file. Old dictionaries stay
readable forever, and editing one in place would leave every peer on an older
build unable to decode anything under an ID claiming they agree:

```
make dict           # refuses to overwrite; use -id 2 -out .../dict2.zdict
```

## The whole gate

`make check` runs formatting, vet, the wire-format vectors and the race
detector, which is what CI runs:

```
make check
```

## Generated documentation

Two documentation artifacts are generated from the config struct tags, so they
cannot drift from the binary. Regenerate both when a config key changes:

```
go run ./cmd/meshbbs config reference --markdown > docs/config.md
go run ./tools/genconfigsite
```

The first writes the table in [config.md](config.md); the second rewrites the
generated block in `site/config-reference.html` in place, leaving the page's
hand-written chrome alone.
