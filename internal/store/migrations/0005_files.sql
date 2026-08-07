-- ---------------------------------------------------------------------------
-- File areas and the content-addressed blob store (§6.5).
-- ---------------------------------------------------------------------------
--
-- WHY FILE AREAS SHARE THE `areas` TABLE
--
-- An area's identity on the wire is its AreaTag: BLAKE3(name)[:4]. That tag is
-- a single global namespace — the bundle header carries it, and the gossip
-- engine reconciles by tag alone with no notion of what kind of thing the area
-- holds. A separate `file_areas` table with its own names would therefore let a
-- sysop create a message area and a file area with the SAME name, and the two
-- logs would silently merge into one area tag: two version vectors over one
-- coordinate space, each advertising records the other cannot interpret.
--
-- Sharing the table makes that collision a UNIQUE violation at creation time,
-- which is where a sysop can still do something about it. `kind` then says what
-- the area holds, and the gossip store's federated-area query does not change:
-- a federated file area joins the digest rotation exactly like a message area,
-- because at the protocol layer it IS one.
--
-- WHY THERE IS NO REFCOUNT COLUMN ON `blobs`
--
-- The count of files referencing a blob is derivable — SELECT COUNT(*) FROM
-- files WHERE hash = ?. A stored counter would be a second source of truth that
-- can drift from the first, and the only thing it buys is a fast answer to a
-- question asked at delete time, once per deletion. Derive it.
--
-- WHAT THIS TABLE IS NOT
--
-- `files` is the catalog of what THIS node holds on disk. It is not the
-- network-wide catalog: that is the record log itself, queried as records of
-- type FILE, the same way ListPosts reads POST records rather than a posts
-- table (§6.2). Keeping the two apart is what makes "held by" answerable —
-- a record whose hash has no row here is a file some other BBS holds.

-- The default is 'message' so every area that already exists stays what it was.
ALTER TABLE areas ADD COLUMN kind TEXT NOT NULL DEFAULT 'message'
    CHECK (kind IN ('message', 'file'));

-- ---------------------------------------------------------------------------
-- Blobs: content-addressed file bodies.
-- ---------------------------------------------------------------------------
--
-- The hash is the full 32-byte BLAKE3, not the 16-byte truncation the FILE
-- record carries on the wire. Local integrity checking is not paying mesh
-- airtime, so it gets the full width; the wire truncation is a separate
-- decision made against a 233-byte MTU.
--
-- Bytes live on disk at <files_dir>/blobs/ab/cd/<hex>, not in SQLite. A BBS on
-- a Raspberry Pi (§10) should not be asked to stream a 40 MB file through a
-- single-connection database pool.
CREATE TABLE blobs (
    hash       BLOB PRIMARY KEY NOT NULL,   -- BLAKE3-256 of the content
    size       INTEGER NOT NULL,
    created_at INTEGER NOT NULL,

    CHECK (length(hash) = 32),
    CHECK (size >= 0)
);

-- ---------------------------------------------------------------------------
-- Files: one named entry in one file area.
-- ---------------------------------------------------------------------------
--
-- Two areas holding the same content share one blob and get one row each, which
-- is the dedup §6.5 asks for.
--
-- `record_id` is the FILE record minted when the file was published, and is
-- NULL for a file in a local-only area — there is nothing to publish and no
-- airtime to spend. It is deliberately nullable rather than absent: whether a
-- local file has been announced to the network is exactly the question the
-- browser has to answer, and a join against the record log answers it.
CREATE TABLE files (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    area_id     INTEGER NOT NULL REFERENCES areas(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    hash        BLOB NOT NULL REFERENCES blobs(hash),
    description TEXT NOT NULL DEFAULT '',
    uploader    TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
    uploaded_at INTEGER NOT NULL,
    record_id   BLOB REFERENCES records(id) ON DELETE SET NULL,

    UNIQUE (area_id, name),
    CHECK (length(name) BETWEEN 1 AND 64),
    CHECK (length(hash) = 32),
    CHECK (record_id IS NULL OR length(record_id) = 16)
);

-- The browser lists one area at a time, newest first.
CREATE INDEX files_area ON files (area_id, uploaded_at);

-- "Do we hold this content?" is asked once per row when rendering the
-- network-wide catalog, so it must not be a scan.
CREATE INDEX files_hash ON files (hash);
