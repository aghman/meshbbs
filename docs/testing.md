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

## The whole gate

`make check` runs formatting, vet and the race detector, which is what CI
runs:

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
