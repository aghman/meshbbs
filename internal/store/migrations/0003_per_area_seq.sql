-- ---------------------------------------------------------------------------
-- Sequences become per (origin, area), not per origin (§7.3).
-- ---------------------------------------------------------------------------
--
-- WHY
--
-- Version vectors are per AREA: a digest carries this node's state for each
-- area separately, and a peer reconciles by asking for the sequence ranges it
-- is missing in that area. That only works if an origin's sequences are DENSE
-- within an area, because the receiving side advances its vector to the highest
-- CONTIGUOUS sequence it holds — the run from 1 with no gaps.
--
-- Allocation was global per node: one counter shared by every area. A node
-- posting into `general`, `tech` and the roster therefore produced seqs 1, 2, 3
-- spread across three areas, and no area held a dense run. Every per-area
-- vector read as empty or near-empty, so nodes advertised nothing, asked for
-- nothing, and never converged.
--
-- Worse than slow: a receiver whose `general` vector sits at 5 while seq 6
-- belongs to `tech` asks for 6 in `general` forever. The gap is permanent and
-- silent, which is the failure mode the whole protocol is built to avoid.
--
-- The simulator never caught it because every simulated node publishes into
-- exactly one area, which makes global and per-area allocation identical.
--
-- WHAT THIS MIGRATION CANNOT DO
--
-- It cannot renumber existing records. A record's sequence is part of the bytes
-- its signature covers (§6.2.1 rule 1), so rewriting one would invalidate it —
-- and any peer that already holds it would see two different records claiming
-- one coordinate, which is the divergence §6.2.1 rule 3 exists to prevent.
--
-- So history keeps the sequences it was written with. Each area's counter
-- starts above the highest sequence already used in that area, which guarantees
-- new records never collide. Records written BEFORE this migration may still
-- leave gaps in their area, and those areas will not fully reconcile. That is
-- accepted deliberately: the wire format is not frozen until Phase 6 `[D10]`,
-- no instance has peers yet, and the alternative is invalidating signed
-- history to tidy up a development log.

CREATE TABLE area_seq_state (
    area       BLOB PRIMARY KEY NOT NULL,   -- 4-byte area tag
    high_water INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL DEFAULT 0,

    CHECK (length(area) = 4),
    CHECK (high_water >= 0)
);

-- Seed each area from what this node has already written there, so allocation
-- continues above existing records rather than colliding with them.
INSERT INTO area_seq_state (area, high_water, updated_at)
SELECT r.area, MAX(r.seq), 0
  FROM records r
  JOIN nodes n ON n.id = r.origin AND n.is_self = 1
 GROUP BY r.area;

-- Uniqueness follows allocation. Two records at one (origin, seq) in DIFFERENT
-- areas is now the normal case rather than a conflict; two in the SAME area is
-- still the equivocation that must never be papered over.
DROP INDEX IF EXISTS records_origin_seq;
CREATE UNIQUE INDEX records_origin_area_seq ON records (origin, area, seq);

-- The old global counter keeps only its incarnation, which is genuinely about
-- this node's log as a whole: §6.2.1 rule 3 uses it to tell peers that our
-- history needs re-verification, and that is not an area-scoped statement.
-- high_water stays in place, unused, rather than being dropped: SQLite would
-- rebuild the table, and there is nothing to gain from it.
