-- Door areas: a league is an area (§9.5).
--
-- ---------------------------------------------------------------------------
-- WHY A LEAGUE IS AN AREA RATHER THAN A NEW THING
-- ---------------------------------------------------------------------------
--
-- An inter-BBS door league needs exactly what a forum needs: a named namespace
-- that some instances federate and others do not, with its own sequence space,
-- its own retention, and a version vector the sync engine can reconcile. That
-- is an area. Building a parallel `leagues` table would mean reimplementing
-- anti-entropy, digests, bundles, the fountain codec and the sysop CLI for a
-- feature whose whole appeal is that §7 already carries it.
--
-- It also inherits the constraint that made file areas share this table: the
-- AreaTag namespace is global. A `leagues` table with its own names would let a
-- sysop create a message area and a league with the same name, and the two
-- would silently merge into one tag — two version vectors over one coordinate
-- space, each advertising records the other cannot read.
--
-- The sequence space matters more here than for either other kind. Migration
-- 0003 made sequences per-area precisely so a chatty area cannot leave gaps in
-- a quiet one, and a door game is the chattiest thing this design contemplates:
-- §9.5's own caveat is that "a chatty game will notice" the priority order.
--
-- ---------------------------------------------------------------------------
-- WHY THIS REBUILDS THE TABLE
-- ---------------------------------------------------------------------------
--
-- `kind` arrived in 0005 with CHECK (kind IN ('message', 'file')). SQLite has
-- no ALTER TABLE that widens a CHECK, so admitting a third value means the
-- documented twelve-step rebuild. The alternative — dropping the constraint
-- and validating only in Go — would put the rule in one place instead of two,
-- and the one it would keep is the one a direct sqlite3 session bypasses.
--
-- The DROP below cascades into `files` unless foreign keys are suspended, which
-- is why Store.migrate pins a connection, turns them off around the whole run,
-- and finishes with a foreign_key_check over every row. A migration cannot do
-- that itself: PRAGMA foreign_keys is a no-op inside a transaction, and each
-- migration file runs in one.
--
-- Row ids are preserved explicitly because files.area_id references them.

CREATE TABLE areas_new (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    name          TEXT NOT NULL UNIQUE COLLATE NOCASE,
    tag           BLOB NOT NULL UNIQUE,        -- 4-byte truncated hash of name
    kind          TEXT NOT NULL DEFAULT 'message',
    description   TEXT NOT NULL DEFAULT '',
    federated     INTEGER NOT NULL DEFAULT 0,
    read_only     INTEGER NOT NULL DEFAULT 0,
    retention_days INTEGER NOT NULL DEFAULT 0, -- 0 = keep forever
    created_at    INTEGER NOT NULL,

    CHECK (length(tag) = 4),
    CHECK (federated IN (0, 1)),
    CHECK (read_only IN (0, 1)),
    CHECK (length(name) BETWEEN 1 AND 32),
    CHECK (kind IN ('message', 'file', 'door'))
);

INSERT INTO areas_new
    (id, name, tag, kind, description, federated, read_only, retention_days, created_at)
SELECT
     id, name, tag, kind, description, federated, read_only, retention_days, created_at
FROM areas;

DROP TABLE areas;

ALTER TABLE areas_new RENAME TO areas;
