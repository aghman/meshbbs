-- Local arrival order for records (§9.5's event.poll cursor).
--
-- ---------------------------------------------------------------------------
-- WHY A DOOR CANNOT POLL ON (origin, seq)
-- ---------------------------------------------------------------------------
--
-- (origin, seq) is the coordinate anti-entropy reconciles on, and it is the
-- WRONG cursor for delivery. Records arrive out of order as a matter of course
-- on a mesh: a bundle is repaired hours after the one that followed it, a peer
-- comes back from a week offline, §6.3 has orphaned replies re-parenting when
-- their parent finally lands. A door that remembered "I have seen up to seq 40
-- from pnw" would step straight past record 37 when it eventually arrived, and
-- would never ask again — the permanent silent gap this protocol is built to
-- avoid, reproduced one layer up.
--
-- What a door needs is the order records reached THIS node, which nothing
-- recorded.
--
-- ---------------------------------------------------------------------------
-- WHY NOT received_at, AND WHY NOT THE ROWID
-- ---------------------------------------------------------------------------
--
-- received_at is seconds. A league night delivers a fight as several records
-- inside one second, so a cursor on it either repeats them or skips them, and
-- which one depends on whether the comparison is > or >=.
--
-- The implicit rowid is monotonic for inserts and CAN BE REUSED after deletes:
-- SQLite hands out max(rowid)+1, so deleting the newest row makes the next
-- insert take its number. Retention exists to delete records (areas.
-- retention_days), and nothing prunes yet — which makes this the good moment to
-- get it right, because the failure it produces is a door silently missing
-- events months from now, on a board that has been running long enough for
-- retention to have started.
--
-- So the counter is explicit and lives outside the table it numbers. It only
-- ever goes up, whatever happens to records.
--
-- AUTOINCREMENT on a helper table would give the same guarantee and would mean
-- inserting and deleting a row per record to read a number back out. A counter
-- row updated in the same transaction as the insert is the same guarantee
-- without the churn, and it says what it is.

ALTER TABLE records ADD COLUMN local_seq INTEGER NOT NULL DEFAULT 0;

CREATE TABLE counters (
    name  TEXT PRIMARY KEY NOT NULL,
    value INTEGER NOT NULL
);

-- Backfill in a stable order: arrival first, then id to break ties inside a
-- second. Deterministic, so two nodes restoring the same backup number the same
-- records the same way — which matters only for debugging, but costs nothing.
--
-- The correlated subquery is quadratic and runs exactly once, on a log that has
-- no peers yet. Written for obviousness rather than speed on purpose.
UPDATE records SET local_seq = (
    SELECT COUNT(*) FROM records AS earlier
    WHERE earlier.received_at < records.received_at
       OR (earlier.received_at = records.received_at AND earlier.id <= records.id)
);

INSERT INTO counters (name, value)
VALUES ('record_local_seq', (SELECT COALESCE(MAX(local_seq), 0) FROM records));

CREATE INDEX idx_records_local_seq ON records(local_seq);
