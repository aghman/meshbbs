-- Door league grants and the outbound event queue (§9.5).
--
-- ---------------------------------------------------------------------------
-- WHY EMITTING IS NOT A NEW API LEVEL
-- ---------------------------------------------------------------------------
--
-- api_level is an integer because the levels NEST (see 0006): 4 can do what 3
-- can. A fifth level above act_as_user would assert that reporting a game
-- result is MORE authority than posting as the user, which is false.
--
-- So a league grant is a second axis on top of level 3, exactly as
-- announce_area is. The two are mirror images and that is the point: announce
-- requires a LOCAL area, because a door must not spend the mesh's airtime on
-- its own say-so, and league_area requires a FEDERATED one, because spending
-- the mesh's airtime is the entire feature. A sysop who has named a federated
-- door area has made that decision explicitly, which is §11.4 working rather
-- than being bypassed.
--
-- An empty league_area means the door may not emit at all — a different thing
-- from a rate limit of zero, same as announce_area.
--
-- ---------------------------------------------------------------------------
-- WHY A QUEUE TABLE AND NOT A CHANNEL
-- ---------------------------------------------------------------------------
--
-- §9.5 requires door events to be batched: a record's framing is 89 bytes of
-- which 64 is a signature, so one event per record spends 79% of the wire on
-- overhead. Batching means holding events for a window, and holding them in
-- memory means a restart in the middle of a league night loses them.
--
-- A door process is also short-lived — it exists only while someone is playing
-- — so there is nobody to hold them but the BBS.
--
-- The CHECKs mirror record.DoorEvent's wire bounds. Two places, deliberately:
-- the codec is what the mesh enforces and this is what a direct sqlite3 session
-- runs into, and a row that cannot become a valid record is a row the flusher
-- would have to drop later, silently, having already told the door "queued".

ALTER TABLE doors ADD COLUMN league_area TEXT NOT NULL DEFAULT '';
ALTER TABLE doors ADD COLUMN league_per_hour INTEGER NOT NULL DEFAULT 6;

CREATE TABLE door_event_queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    door        TEXT NOT NULL,
    -- The area NAME, not its tag: the flusher derives the tag, and a name is
    -- what a sysop reads in `meshbbs door events`.
    area        TEXT NOT NULL,
    game        TEXT NOT NULL,
    kind        INTEGER NOT NULL,
    actor       TEXT NOT NULL,
    target      TEXT NOT NULL DEFAULT '',
    target_node BLOB,
    payload     BLOB NOT NULL DEFAULT X'',
    queued_at   INTEGER NOT NULL,

    CHECK (kind BETWEEN 0 AND 255),
    CHECK (length(game) BETWEEN 1 AND 16),
    CHECK (length(actor) BETWEEN 1 AND 24),
    CHECK (length(target) <= 24),
    CHECK (length(payload) <= 48),
    -- A target node without a nick is unrepresentable on the wire, and a nick
    -- without a node addresses nothing. Both refused here as well as there.
    CHECK ((target = '' AND target_node IS NULL)
        OR (target != '' AND length(target_node) = 8))
);

-- The flusher groups by (area, game) and takes the oldest first.
CREATE INDEX idx_door_event_queue_flush ON door_event_queue(area, game, id);
