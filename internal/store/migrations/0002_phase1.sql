-- Phase 1: SSH front end, forums, DMs (design §5, §6.3, §6.4, §8.2).

-- ---------------------------------------------------------------------------
-- DM key custody, tier 2 (§8.2).
-- ---------------------------------------------------------------------------
--
-- dm_public_key is not secret: it is what other users encrypt to, and what a
-- PROFILE record will publish in Phase 2.
--
-- dm_wrapped_key is the private half sealed under the user's passphrase. The
-- sysop holds ciphertext at rest. There is deliberately NO column holding the
-- plaintext key or the passphrase — the server only ever has the plaintext in
-- memory, during an authenticated session, for as long as it takes to decrypt
-- something for display. That restriction is what keeps tier 3 (client-held
-- keys) an addition rather than a rewrite.
ALTER TABLE users ADD COLUMN dm_public_key TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN dm_wrapped_key BLOB;

-- ---------------------------------------------------------------------------
-- Forum areas (§6.3).
-- ---------------------------------------------------------------------------
--
-- `federated` defaults to 0. Sysops opt IN to spending the network's airtime,
-- never out — at ~10 originated packets/day/node (§1.1) this default is doing
-- real work.
CREATE TABLE areas (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT NOT NULL UNIQUE COLLATE NOCASE,
    tag           BLOB NOT NULL UNIQUE,        -- 4-byte truncated hash of name
    description   TEXT NOT NULL DEFAULT '',
    federated     INTEGER NOT NULL DEFAULT 0,
    read_only     INTEGER NOT NULL DEFAULT 0,
    retention_days INTEGER NOT NULL DEFAULT 0, -- 0 = keep forever
    created_at    INTEGER NOT NULL,

    CHECK (length(tag) = 4),
    CHECK (federated IN (0, 1)),
    CHECK (read_only IN (0, 1)),
    CHECK (length(name) BETWEEN 1 AND 32)
);

-- ---------------------------------------------------------------------------
-- Post authorship index.
-- ---------------------------------------------------------------------------
--
-- Records are node-signed (§6.2, [D5]): "instance X vouches that user austin
-- posted this". The record itself therefore carries the author's nick in its
-- body rather than a cryptographic user identity, and this table is the local
-- index that makes "show me austin's posts" a query rather than a scan.
CREATE TABLE post_authors (
    record_id BLOB PRIMARY KEY NOT NULL REFERENCES records(id) ON DELETE CASCADE,
    author    TEXT NOT NULL COLLATE NOCASE,
    subject   TEXT NOT NULL DEFAULT '',

    CHECK (length(record_id) = 16)
);

CREATE INDEX post_authors_author ON post_authors (author);

-- ---------------------------------------------------------------------------
-- DM index (§6.4).
-- ---------------------------------------------------------------------------
--
-- The DM body is sealed, but recipient addressing is deliberately in the clear
-- ([D7]): metadata privacy is explicitly not a requirement, and routing on a
-- readable recipient buys immediate bounces and per-recipient spam filtering.
-- This table is that cleartext routing information.
--
-- Note there is no plaintext column. The server cannot read a DM without the
-- recipient's passphrase, and nothing here changes that.
CREATE TABLE dm_index (
    record_id   BLOB PRIMARY KEY NOT NULL REFERENCES records(id) ON DELETE CASCADE,
    sender      TEXT NOT NULL COLLATE NOCASE,
    recipient   TEXT NOT NULL COLLATE NOCASE,
    sender_node BLOB NOT NULL,
    subject     TEXT NOT NULL DEFAULT '',
    sent_at     INTEGER NOT NULL,
    read_at     INTEGER NOT NULL DEFAULT 0,

    CHECK (length(record_id) = 16),
    CHECK (length(sender_node) = 8)
);

CREATE INDEX dm_index_recipient ON dm_index (recipient, sent_at);
CREATE INDEX dm_index_sender ON dm_index (sender, sent_at);
