-- Phase 0 schema (design §6.1, §6.2, §6.7, §11.1).
--
-- The file/database split from §11.1 governs what lives here: this holds
-- content and operational state a sysop edits at runtime. Listener ports and
-- identity live in config.toml.

-- ---------------------------------------------------------------------------
-- Records: the append-only signed log (§6.2).
-- ---------------------------------------------------------------------------
--
-- `signed` holds the exact bytes that were signed, retained verbatim. This is
-- §6.2.1 rule 1: verification must never re-serialize parsed columns, because
-- any future encoder change would then invalidate every historical signature.
-- The decomposed columns exist only for querying.
CREATE TABLE records (
    id          BLOB PRIMARY KEY NOT NULL,   -- BLAKE3(signed)[:16], derived
    origin      BLOB NOT NULL,               -- 8-byte node ID
    seq         INTEGER NOT NULL,
    ts          INTEGER NOT NULL,            -- advisory only (§6.2.1)
    type        INTEGER NOT NULL,
    area        BLOB NOT NULL,               -- 4-byte area tag
    parent      BLOB,                        -- 16-byte record ID, NULL if top-level
    body        BLOB NOT NULL,
    signed      BLOB NOT NULL,               -- exact signed bytes; do not regenerate
    sig         BLOB NOT NULL,               -- 64-byte Ed25519 signature
    received_at INTEGER NOT NULL,            -- local arrival, for retention

    CHECK (length(id) = 16),
    CHECK (length(origin) = 8),
    CHECK (length(area) = 4),
    CHECK (length(sig) = 64),
    CHECK (parent IS NULL OR length(parent) = 16),
    CHECK (seq >= 0)
);

-- (origin, seq) is the coordinate version vectors reconcile on (§7.3), and it
-- must be unique: two different records sharing one coordinate is precisely the
-- silent divergence §6.2.1 rule 3 exists to prevent.
CREATE UNIQUE INDEX records_origin_seq ON records (origin, seq);
CREATE INDEX records_area_ts ON records (area, ts);
CREATE INDEX records_parent ON records (parent) WHERE parent IS NOT NULL;
CREATE INDEX records_type ON records (type);

-- ---------------------------------------------------------------------------
-- Known nodes: the roster built from NODE records (§6.1.2).
-- ---------------------------------------------------------------------------
--
-- There is no first-seen binding and no conflict resolution here, because the
-- node ID *is* the hash of the public key: a row can only exist if the key
-- verified against the ID. Nothing to squat, nothing to arbitrate (§6.1.1).
CREATE TABLE nodes (
    id            BLOB PRIMARY KEY NOT NULL,  -- 8-byte node ID
    public_key    BLOB NOT NULL,              -- 32-byte Ed25519 key; hashes to id
    display_name  TEXT NOT NULL DEFAULT '',   -- self-declared, NOT authoritative
    sysop_contact TEXT NOT NULL DEFAULT '',
    incarnation   INTEGER NOT NULL DEFAULT 0, -- §6.2.1 rule 3
    is_self       INTEGER NOT NULL DEFAULT 0,
    first_seen    INTEGER NOT NULL,
    last_seen     INTEGER NOT NULL,

    CHECK (length(id) = 8),
    CHECK (length(public_key) = 32),
    CHECK (is_self IN (0, 1))
);

-- Exactly one row may be this instance.
CREATE UNIQUE INDEX nodes_self ON nodes (is_self) WHERE is_self = 1;

-- ---------------------------------------------------------------------------
-- Aliases: the sysop-owned petname table (§6.1.4, [N1]).
-- ---------------------------------------------------------------------------
--
-- Sysop-owned means one namespace per instance — there is deliberately no
-- user_id column. Aliases are resolved locally at compose time and NEVER go on
-- the wire (§6.1.4.1), so two instances may disagree about a name and neither
-- is wrong.
CREATE TABLE aliases (
    alias      TEXT PRIMARY KEY NOT NULL COLLATE NOCASE,
    node_id    BLOB NOT NULL,
    created_at INTEGER NOT NULL,

    CHECK (length(node_id) = 8),
    CHECK (length(alias) BETWEEN 1 AND 32)
);

CREATE INDEX aliases_node ON aliases (node_id);

-- ---------------------------------------------------------------------------
-- Sequence high-water mark (§6.2.1 rule 3).
-- ---------------------------------------------------------------------------
--
-- Our own seq must never regress, including across a restore from backup: a
-- peer that has seen seq <= N will never request it again, so reissuing a
-- number with different content diverges the network permanently and silently.
--
-- Single row, enforced by the CHECK. Advanced with a durable write before any
-- record using the number is published.
CREATE TABLE seq_state (
    only_row     INTEGER PRIMARY KEY CHECK (only_row = 1),
    high_water   INTEGER NOT NULL DEFAULT 0,
    incarnation  INTEGER NOT NULL DEFAULT 0,
    updated_at   INTEGER NOT NULL DEFAULT 0
);

INSERT INTO seq_state (only_row, high_water, incarnation, updated_at) VALUES (1, 0, 0, 0);

-- ---------------------------------------------------------------------------
-- Users and capabilities (§6.7).
-- ---------------------------------------------------------------------------
--
-- Nicks are unique per instance only — never globally (§6.1.5). This is what
-- lets registration work with no network round trip at all, on a node with no
-- radio attached.
--
-- There is deliberately no email column ([N8]).
CREATE TABLE users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    nick          TEXT NOT NULL UNIQUE COLLATE NOCASE,
    display_name  TEXT NOT NULL DEFAULT '',
    password_hash TEXT NOT NULL DEFAULT '',   -- Argon2id encoded string; empty = no password login
    directory_listed INTEGER NOT NULL DEFAULT 1,  -- listed by default ([N9])
    is_sysop      INTEGER NOT NULL DEFAULT 0,
    can_login     INTEGER NOT NULL DEFAULT 1,
    state         TEXT NOT NULL DEFAULT 'active',  -- active | pending | dormant | disabled
    created_at    INTEGER NOT NULL,
    last_login_at INTEGER NOT NULL DEFAULT 0,

    CHECK (length(nick) BETWEEN 2 AND 16),
    CHECK (is_sysop IN (0, 1)),
    CHECK (can_login IN (0, 1)),
    CHECK (directory_listed IN (0, 1)),
    CHECK (state IN ('active', 'pending', 'dormant', 'disabled'))
);

-- Multiple public keys per user is a v1 requirement, not a later nicety
-- (§6.7): one key per laptop, phone, and shell box is the normal case, and a
-- single-key column would be a painful migration later.
CREATE TABLE user_keys (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    public_key  TEXT NOT NULL,          -- SSH authorized_keys format
    fingerprint TEXT NOT NULL UNIQUE,   -- SHA256:... for display and lookup
    comment     TEXT NOT NULL DEFAULT '',
    added_at    INTEGER NOT NULL
);

CREATE INDEX user_keys_user ON user_keys (user_id);

-- Capabilities are per-user grants rather than a role ladder (§6.7), because
-- they map directly onto the abuse vectors that actually exist here. Note that
-- post_federated is NOT granted by default: open front door, gated commons
-- ([N7]).
CREATE TABLE user_capabilities (
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    capability TEXT NOT NULL,
    granted_at INTEGER NOT NULL,
    granted_by TEXT NOT NULL DEFAULT '',

    PRIMARY KEY (user_id, capability)
);

-- ---------------------------------------------------------------------------
-- Audit log (§11.6).
-- ---------------------------------------------------------------------------
--
-- Anything destructive writes here regardless of which surface performed it.
CREATE TABLE audit_log (
    id       INTEGER PRIMARY KEY AUTOINCREMENT,
    ts       INTEGER NOT NULL,
    actor    TEXT NOT NULL,
    action   TEXT NOT NULL,
    target   TEXT NOT NULL DEFAULT '',
    detail   TEXT NOT NULL DEFAULT ''
);

CREATE INDEX audit_log_ts ON audit_log (ts);
