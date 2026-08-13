-- ---------------------------------------------------------------------------
-- The sneakernet request queue (§6.5 fetch path 2).
-- ---------------------------------------------------------------------------
--
-- `[D8]` named exactly two ways to get a file's bytes: a direct IP link to the
-- BBS holding it, or "queued for the next sneakernet exchange". This table is
-- the queue. Until it existed, §6.5's own honesty clause applied — v0.17
-- REMOVED the "queued for next exchange" wording from the file browser, because
-- nothing recorded a request and nothing would satisfy one, and a promise the
-- software has no intention of keeping is worse than the spinner that bullet
-- warns about.
--
-- ---------------------------------------------------------------------------
-- WHY THE ROW NAMES A HASH AND NOT A FILE
-- ---------------------------------------------------------------------------
--
-- The requester does not hold the file. All it has ever seen is the FILE record
-- the holding BBS announced, and that carries `record.FileHashLen` bytes of
-- BLAKE3 rather than all 32 (§6.5) — a truncation chosen against a 233-byte
-- MTU, not against this. So a request says "send me the bytes whose hash starts
-- like this", which is exactly the question `HoldsBlob` already answers on the
-- other side, by prefix, against the blobs table.
--
-- That width is what a carrier can ask for and no more. It is ample for the job
-- it does: an attacker who wanted to answer a request with different content
-- would need a 128-bit preimage, and the alternative — naming the file by area
-- and name — would let a holder answer with whatever it happened to have filed
-- under that name. Content addressing is the stronger claim, so it is the one
-- the wire carries.
--
-- The NAME is kept anyway, for the requesting end only: it is where the bytes
-- get filed when they arrive, and it is what the person who asked will look for
-- in the browser. It never travels.
--
-- ---------------------------------------------------------------------------
-- WHY THE NICK IS PART OF THE ROW, AND OF THE UNIQUE KEY
-- ---------------------------------------------------------------------------
--
-- §6.5 asks for "the requesting user notified when it lands", so there has to be
-- a user on the row. Two people wanting the same file is not a conflict — it is
-- one hash on the carrier and two people to tell — so the key is (area, hash,
-- nick) and the de-duplication that matters happens when the carrier is packed.
--
-- ---------------------------------------------------------------------------
-- WHY arrived_at AND notified_at ARE SEPARATE
-- ---------------------------------------------------------------------------
--
-- The bytes land during a `sneakernet import`, which is a sysop at a command
-- line, and the person who asked is not there — they were on an SSH session
-- days ago. Collapsing the two would mean the notice is either lost (marked
-- told when nobody was) or repeated forever (never marked at all).
--
-- `note` is the one case where the bytes arrived and could not be filed: an
-- upload took the name in the meantime. Recorded rather than retried, because
-- the content IS here and asking the other board to carry it again would spend
-- a second car journey on a problem at this end.

CREATE TABLE file_requests (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    area_id      INTEGER NOT NULL REFERENCES areas(id) ON DELETE CASCADE,
    -- Where the bytes get filed when they arrive. Local only; see above.
    name         TEXT NOT NULL,
    -- The truncated BLAKE3 a FILE record carries. This is what goes on a
    -- carrier, and it is the whole of the request as far as the wire is
    -- concerned.
    wire_hash    BLOB NOT NULL,
    -- The node that announced it. Advisory: a carrier is answered by whoever
    -- is holding it, which on a multi-hop hand-off is often not this node.
    holder       BLOB,
    nick         TEXT NOT NULL COLLATE NOCASE,
    requested_at INTEGER NOT NULL,
    arrived_at   INTEGER NOT NULL DEFAULT 0,
    notified_at  INTEGER NOT NULL DEFAULT 0,
    note         TEXT NOT NULL DEFAULT '',

    UNIQUE (area_id, wire_hash, nick),
    CHECK (length(name) BETWEEN 1 AND 64),
    CHECK (length(wire_hash) = 16),
    CHECK (holder IS NULL OR length(holder) = 8),
    -- Notified without arrived would be a notice about nothing.
    CHECK (notified_at = 0 OR arrived_at != 0)
);

-- Packing a carrier asks for every distinct hash still outstanding, oldest
-- first, so this is the index that query runs on.
CREATE INDEX file_requests_open ON file_requests (arrived_at, requested_at);

-- An import arrives holding a hash and asks who wanted it.
CREATE INDEX file_requests_hash ON file_requests (wire_hash);

-- The menu asks "has anything this user asked for landed?" once per session.
CREATE INDEX file_requests_nick ON file_requests (nick, arrived_at, notified_at);
