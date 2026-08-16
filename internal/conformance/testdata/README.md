# BSMP wire-format conformance vectors

These files freeze the bytes MeshBBS puts on a mesh. They are the teeth behind
design §12.6 and behind `[D10]`'s commitment to publish the format in Phase 6.

Every other test that touches an encoder is a round trip — encode, decode, assert
you got back what you started with. That proves the two halves agree with each
other and says nothing about *which* bytes they agreed on. A change that flips
the record timestamp to little-endian on both sides passes the entire test suite;
it fails here, and only here.

**These vectors are append-only.** Nothing in these directories is ever edited or
regenerated. See "Changing things" below.

## Generations

`v1/`, `v2/` — each one a snapshot of all three wire versions as they stood when
it was frozen, listed in `conformance.Generations`:

| generation | record | bundle | gossip | what moved |
|---|---|---|---|---|
| `v1` | 1 | 1 | 1 | the first corpus |
| `v2` | 1 | 1 | 2 | the digest gained a dictionary byte (§7.4) |

A generation is a snapshot of all three rather than a directory per format,
because the only combination worth testing against is one that actually shipped
together — which is what a third-party implementation needs to know, and what a
directory per format would need extra bookkeeping to express.

**An old generation is not dead weight.** It becomes the cross-version corpus,
and it earns its keep in both directions at once:

- Layers whose version did **not** change must still encode byte-identically.
  That is the half nobody thinks to check. When one format moves, the risk is not
  to the format that moved — it gets a new generation and everybody's attention —
  but to the ones that did not, where the current generation is regenerated at
  the same moment and would happily freeze a fresh mistake.
- Layers whose version **did** change must now be **rejected**, with a version
  error rather than a truncation. That is §7.1's pre-freeze drop-and-log policy
  as an assertion.

That second one earned its place on its first run: it found that `DecodeDigest`
checked the v2 header length *before* the version byte, so a three-byte v1 digest
came back as "truncated gossip message" — sending a sysop to look at the radios
when the answer was that a peer needed upgrading. Version is now read before any
structural rule, in every gossip decoder, because a length rule belongs to the
version that defined it.

## Reading a vector

Every entry carries its **input** and its expected **output**. Nothing is implied
by position or by a builder living somewhere else — the file says what went in
and what must come out. All byte strings are lowercase hex.

```json
{
  "name": "post-toplevel",
  "note": "the ordinary case: no parent, so no parent field is written",
  "input": { "key": "node-a", "seq": 1, "ts": 1700000000, "type": 1,
             "area": "general", "body": "48656c6c6f2c206d6573682e0a" },
  "area_tag": "79d42c56",
  "canonical": "010001e41be10756a217e5016553f10079d42c560d48656c6c6f2c206d6573682e0a",
  "id": "57e579bc5b6ecf2fbd822b669c01c368",
  "signature": "566d71f313fdd0da...",
  "marshal": "010001e41be10756a217e5...566d71f3..."
}
```

Reading that `canonical` against §6.2.1's layout — `format(1) | flags(1) |
type(1) | origin(8) | seq(uvarint) | ts(4 BE) | area(4) | [parent(16)] |
bodyLen(uvarint) | body` — gives `01 | 00 | 01 | e41be10756a217e5 | 01 |
6553f100 | 79d42c56 | 0d | "Hello, mesh.\n"`. The `flags` byte is zero because
there is no parent, and a top-level record therefore does not pay the 16 bytes
one would cost. `marshal` is the same bytes with the 64-byte signature appended.

A third-party implementation tests itself by reading `input`, encoding it, and
comparing. That is exactly what `conformance_test.go` does.

## The files

| File | What it pins |
|---|---|
| `keys.json` | Ed25519 seed → public key → node ID → every rendering of it (Crockford base32, grouped, short, BIP-39 words) |
| `records.json` | The record envelope of §6.2.1: canonical bytes, derived ID, signature, and the marshalled form |
| `bodies.json` | The typed body encoders — NODE, PROFILE, SUCCESSION, FILE, DOOR_EVENT — on their own, so a body failure and an envelope failure cannot be confused |
| `bundles.json` | The uncompressed bundle framing, plus frozen packed blobs in the decode direction |
| `control.json` | Gossip DIGEST / VECTOR_REQ / VECTOR / RANGE_REQ, and version-vector encoding and hashing |
| `fountain.json` | Symbol headers, **repair-symbol masks**, and real symbols off a real encoder |
| `meshlink.json` | Signed ANNOUNCE and WHO_IS frames |

Vectors reference each other by name rather than repeating bytes: a record names
a key from `keys.json`, a bundle names records from `records.json`. That keeps a
bundle vector a statement about *composition* — the origin table, the timestamp
deltas, the length prefixes — instead of a second copy of the record vectors.

## Three things that are not obvious

**Bundles pin the uncompressed body, not the packed bytes.** `bundle.Pack` runs
the body through zstd, and that output moves whenever the compressor is upgraded
— and it *will* move when §7.4's real trained dictionary lands, which is supposed
to happen before the Phase 6 freeze. Pinning it would freeze a library's
behaviour and call it a wire format. What is actually the format is
`bundle.EncodeBody`, which is deterministic by construction.

The `decode_only` section covers the other direction: packed blobs produced once,
which must always *decode* to the named body. That is also what stops dictionary
0's corpus being edited in place. A retrained dictionary must ship as dictionary
1, leaving 0 untouched and supported, exactly as §7.4 promises — rewriting 0
would silently break every peer running an older build, and this is the only
place that would notice.

**The fountain masks are the highest-value vectors here.** A repair symbol's mask
says which source symbols XOR into it, and it *never travels on the wire* — both
ends derive it from `(sender, bundle_id, index)`. An implementation that derives
it differently does not fail a length check or a signature check. It decodes to
garbage, silently, with every field in every header agreeing. There is nowhere
else in BSMP where being wrong is this quiet. `mask` is packed LSB-first: source
symbol *i* is included when `mask[i/8] & (1 << (i%8))` is set.

**Keys are frozen as raw 32-byte seeds.** Tests elsewhere in the tree build keys
through `identity.GenerateNodeKey(rng.TestSecret(n))`, which is reproducible but
depends on `internal/rng`'s stream and on how `ed25519.GenerateKey` reads from
it. A corpus meant to outlive this codebase cannot rest on either, so the seed
itself is the artifact and `ed25519.NewKeyFromSeed` is the only step.

## Changing things

**Adding a vector.** Add its input to the table in `tools/conformance/main.go`
and run:

```bash
go run ./tools/conformance
```

New names are appended. Existing vectors are recomputed and compared; the
generator exits non-zero if any of them would change, and it refuses to write.
Removing a vector is also an error — a third-party implementation may already be
testing against it.

**A vector fails.** An encoder now produces different bytes for an input that was
frozen from a build believed correct. Do not regenerate; the generator will
refuse anyway. Find the encoder change. If it was intended, then it is a wire
format change and needs a version bump and a compatibility story in both
directions — not an edited expectation.

Note that the generator only ever writes the **current** generation. There is
deliberately no flag aiming it at an older one: it can only produce the bytes
today's encoders emit, so such a flag could only write wrong bytes into a frozen
directory and then be refused by the append-only check.

**A format version is bumped.** Add a generation to `conformance.Generations`
recording the new combination, then run the generator — it creates the directory
and fills it. `TestCurrentGenerationMatchesTheCode` fails until you do, which is
the point: it stops the tempting alternative of repointing the existing
generation at a format it was not frozen under.

Do not delete the old directory. It is the cross-version corpus from that moment
on, and deleting it would discard the only evidence of what a shipped build put
on the air.

## Provenance

Generated by `tools/conformance` on a tree where all eight wire-format fuzz
targets had just been run clean for 90 seconds each. That matters: freezing
vectors freezes current behaviour including its bugs, and three canonicalisation
bugs in this codebase were found by fuzzing rather than by inspection (an
overlong varint in the record codec, the same in `vv`, unvalidated reserved bits
in the fountain symbol header). Run the fuzzers before adding vectors, for the
same reason.

The key seeds are `BLAKE3("meshbbs/conformance/v1/key/" + name)`. They are
obviously synthetic and have never protected anything.
