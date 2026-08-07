package record

// DMArea is the well-known area private mail lives in (§6.4).
//
// # Why this is not the zero tag
//
// It was, and that was a leak. The zero tag is the ROSTER (store.RosterArea) —
// the one area that is ALWAYS federated, because it is how trust bootstraps
// (§6.1.2). A DM written with a zero area therefore landed in the bootstrap
// area, where the gossip store's version vector counted it and its record
// queries would have served it to any peer that asked. The bodies are sealed
// (§8.2), but the DM body's sender and recipient nicks are cleartext by
// decision `[D7]` — and §8.1 says to treat the mesh as a public broadcast
// medium, so "who mails whom on this BBS" would have been readable by every
// sysop on the channel. Private mail also consumed the roster's sequence
// numbers, spending the one stream that must stay dense.
//
// A distinct tag, derived the same way as ProfileArea, gives mail its own
// sequence space and its own federation decision.
//
// # It is NOT on the wire yet
//
// bbs.SendDM refuses off-node delivery — Phase 1 is single-node, and store and
// forward (§6.4) is not built. So there is nothing to gain from replicating
// this area and everything to lose, and the gossip store excludes it: it is
// never offered in a digest, and DM records are never served at all. When
// federated mail arrives, that exclusion is the one place to change, and
// §8.3's Part 97 gate is already waiting for it — the outbox classifies this
// tag as governor.ClassDM, so a licensed node refuses to put it on the air.
var DMArea = AreaTagFor("_mail")
