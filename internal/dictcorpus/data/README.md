# Dictionary training corpus (§7.4)

The text dictionary 1 is trained on, and the text its ratio is measured against.

**`train/` and `holdout/` must never overlap.** The dictionary is built from
`train/` only; every number in `compression_test.go` and in §7.4 comes from
`holdout/`, which the trainer never sees. A dictionary measured on its own
training data reports how well it memorised, not how well it compresses, and the
figure is flattering by a wide margin — `dev seed`'s `"seeded post %d in %s"`
would score spectacularly and mean nothing.

The two halves share a *register* and share no *content*. That is deliberate:
sharing the register is exactly the property a dictionary is supposed to
generalise across — quoting conventions, sign-offs, the vocabulary of people
running radios — while sharing content would be measuring a cache.

## Why this is synthetic

§7.4 asks for "a corpus of real BBS/Usenet/forum text". A zstd dictionary embeds
literal fragments of whatever it is trained on directly into the shipped binary,
so training on archived message bases would put third-party text of uncertain
provenance inside an MIT-licensed program. This corpus is written for the
purpose and carries the repository's licence like everything else.

The cost is honestly stated: it is a plausible imitation of BBS traffic, not a
sample of it. If a real archive with a compatible licence ever shows up — §7.4
particularly wants FTN echomail, whose `SEEN-BY`, `PATH` and `MSGID` kludge
lines are extremely repetitive — retraining on it should beat this, and the
holdout measurement is what would show it.

## Format

`forum.txt` holds posts separated by a line containing only `%%`. Leading and
trailing blank lines around each post are stripped.

`files.txt` holds one catalog entry per line: `name<TAB>size<TAB>description`.

`doors.txt` holds one game per line: `game<TAB>nick,nick,…`. The trainer builds
event batches from these, which is why no event payloads appear here — they are
opaque bytes a door defines, and there is nothing to learn from a made-up one.
