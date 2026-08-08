-- ---------------------------------------------------------------------------
-- Doors, and the two things a door is allowed to keep (§9.1.1, §11.5).
-- ---------------------------------------------------------------------------
--
-- WHY DOOR CONFIGURATION IS HERE AND NOT IN config.toml
--
-- §11.5 puts every per-door key in the database, and the reason shows up the
-- first time a sysop installs a door: adding one is a routine, frequent,
-- per-instance act, and the config file is the thing `config check` validates
-- as a whole and refuses to start without. A door row is data. A door row with
-- a typo in it should disable that door, not the BBS.
--
-- WHY api_level IS A COLUMN AND NOT A GRANTS TABLE
--
-- §9.1.1 has one grantor (the sysop) and one grantee (the door), and levels
-- that nest: 1-3 are always available and 4 is granted explicitly. That is an
-- integer. A grants table would model a many-to-many relationship that does
-- not exist, and would let a door hold level 4 without level 1, which is not a
-- state the capability model has a meaning for.

CREATE TABLE doors (
    name        TEXT PRIMARY KEY NOT NULL COLLATE NOCASE,
    -- Path is absolute; the runner refuses anything else, so that which binary
    -- runs is not a question answered by the server's PATH.
    path        TEXT NOT NULL,
    -- Args and env_passthrough are JSON arrays of strings. They are lists of
    -- short opaque tokens that are read whole and never queried into, which is
    -- the case where a JSON column beats a side table.
    args            TEXT NOT NULL DEFAULT '[]',
    cwd             TEXT NOT NULL,
    env_passthrough TEXT NOT NULL DEFAULT '[]',
    dropfile_type   TEXT NOT NULL DEFAULT 'none',

    -- Limits (§9.4). cpu_limit and mem_limit are recorded but NOT yet enforced:
    -- applying them needs rlimits set between fork and exec, which Go offers no
    -- cgo-free hook for. They are here because leaving the column out would mean
    -- a migration later; a sysop setting one today gets a warning, not silence.
    max_concurrent    INTEGER NOT NULL DEFAULT 0,   -- 0 = no cap
    node_lock         INTEGER NOT NULL DEFAULT 0,
    cpu_limit_secs    INTEGER NOT NULL DEFAULT 0,   -- 0 = unset
    mem_limit_bytes   INTEGER NOT NULL DEFAULT 0,   -- 0 = unset
    wall_clock_secs   INTEGER NOT NULL DEFAULT 3600,

    -- What a USER needs to run this door, on top of run_doors.
    required_capability TEXT NOT NULL DEFAULT '',

    -- §9.1.1. Level 4 is act_as_user and has to be set deliberately.
    api_level   INTEGER NOT NULL DEFAULT 3,

    -- Level 3. An empty announce_area means the door may not announce at all,
    -- which is a different thing from being rate-limited to zero: the sysop has
    -- not chosen a destination, so there is nowhere for a post to go.
    announce_area       TEXT NOT NULL DEFAULT '',
    announce_per_hour   INTEGER NOT NULL DEFAULT 4,

    -- How much level-2 state this door may keep, across all its users.
    -- Extends §11.5's list: state is quota'd there without saying by what, and
    -- a door that legitimately needs more should not need a rebuild to get it.
    state_quota_bytes   INTEGER NOT NULL DEFAULT 65536,

    enabled     INTEGER NOT NULL DEFAULT 1,
    created_at  INTEGER NOT NULL,

    CHECK (length(name) BETWEEN 1 AND 32),
    CHECK (api_level BETWEEN 1 AND 4),
    CHECK (node_lock IN (0, 1)),
    CHECK (enabled IN (0, 1)),
    CHECK (max_concurrent >= 0),
    CHECK (wall_clock_secs > 0),
    CHECK (announce_per_hour >= 0),
    CHECK (state_quota_bytes >= 0),
    CHECK (dropfile_type IN ('none', 'door.sys', 'door32.sys', 'dorinfo1.def'))
);

-- ---------------------------------------------------------------------------
-- Level 2: a door's private key/value namespace.
-- ---------------------------------------------------------------------------
--
-- WHY ONE TABLE WITH A SCOPE COLUMN
--
-- §9.1.1 gives a door two namespaces, (door, user) and (door, global), with the
-- same rules, the same quota and the same key space. Two tables would duplicate
-- all three and invite them to drift; a door's global high-score table and a
-- player's saved game differ in who owns the row, not in what a row is.
--
-- The owner column is the nick for user scope and empty for global, which the
-- CHECK below makes unambiguous rather than conventional. Nicks cannot be
-- empty (§5.1), so '' is not a value a user can collide with.
--
-- The door reference cascades: deleting a door deletes what it kept. A door
-- that has been removed and reinstalled is a new installation, and inheriting
-- the old one's saved state is more surprising than starting clean.
CREATE TABLE door_state (
    door       TEXT NOT NULL COLLATE NOCASE REFERENCES doors(name) ON DELETE CASCADE,
    scope      TEXT NOT NULL,
    owner      TEXT NOT NULL DEFAULT '' COLLATE NOCASE,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,

    PRIMARY KEY (door, scope, owner, key),
    CHECK (scope IN ('user', 'global')),
    CHECK ((scope = 'global' AND owner = '') OR (scope = 'user' AND owner <> '')),
    CHECK (length(key) BETWEEN 1 AND 64)
) WITHOUT ROWID;

-- ---------------------------------------------------------------------------
-- Level 4: who has been told.
-- ---------------------------------------------------------------------------
--
-- §9.1.1 requires the user to be told the first time a given door acts as them,
-- "not buried in a log the user will never read". That is a once-per-(door,
-- user) fact, and it has to survive a restart or the notice becomes a nag that
-- appears again after every deploy — which is how people learn to dismiss it
-- without reading it.
--
-- Deliberately NOT cascaded on the door: a door that was removed and
-- reinstalled under the same name is, from the user's point of view, the same
-- door they were already told about. This is the opposite call from door_state
-- above, for the opposite reason — state is the door's and a new installation
-- should start clean, whereas the notice is the USER's and re-showing it says
-- something untrue about what is new.
CREATE TABLE door_notices (
    door       TEXT NOT NULL COLLATE NOCASE,
    nick       TEXT NOT NULL COLLATE NOCASE,
    notified_at INTEGER NOT NULL,

    PRIMARY KEY (door, nick)
) WITHOUT ROWID;
