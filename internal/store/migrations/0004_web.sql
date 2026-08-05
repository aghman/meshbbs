-- Web front end: passkeys and their bootstrap (webui.md §7, §8).
--
-- Passkeys are the web UI's ONLY credential ([D17]). There is no web password
-- column here and there should never be one: the point of WebAuthn is that the
-- server holds no reusable secret, and adding a password path would restore the
-- exact loss it prevents.

-- The shape deliberately mirrors user_keys. A user accumulates authenticators —
-- some SSH keys, some passkeys — and any of them logs them in. That symmetry is
-- not decoration: it means "enrol another device" is one concept on this BBS
-- rather than two that happen to look alike.
CREATE TABLE webauthn_credentials (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    user_id       INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- The credential ID the authenticator minted. Unique across all users:
    -- discoverable credentials mean sign-in arrives with only this, and no
    -- nick, so it must identify the account by itself ([D17]).
    credential_id BLOB NOT NULL UNIQUE,
    public_key    BLOB NOT NULL,

    -- sign_count detects a CLONED authenticator: a real one only ever counts
    -- up, so a value at or below what we already saw means two devices are
    -- presenting the same credential. Authenticators that do not implement the
    -- counter report 0 forever, which is why zero is not treated as suspicious.
    sign_count    INTEGER NOT NULL DEFAULT 0,

    aaguid        BLOB NOT NULL DEFAULT x'',
    -- Transports the browser reported ("usb", "internal", …), stored so a later
    -- sign-in can hint at the right one instead of offering every option.
    transports    TEXT NOT NULL DEFAULT '',
    -- A name the user gives the device, so revoking one is a decision they can
    -- actually make.
    label         TEXT NOT NULL DEFAULT '',

    added_at      INTEGER NOT NULL,
    last_used_at  INTEGER
);

CREATE INDEX webauthn_credentials_user ON webauthn_credentials (user_id);

-- Enrolment codes bootstrap a passkey onto an account created before the web
-- existed — over SSH or the CLI — which is every account that exists today
-- ([D18]). A passkey cannot bootstrap itself: registering one requires a
-- browser and an already-established account holder.
--
-- The authority is deliberately narrow: a code REGISTERS A CREDENTIAL and
-- cannot mint a session. If that ever changes, this has become a password with
-- worse ergonomics and the rest of [D18] stops holding.
--
-- user_id is the PRIMARY KEY, which is what enforces ONE LIVE CODE PER ACCOUNT:
-- issuing a second replaces the first, so codes cannot be stockpiled and a
-- code read over someone's shoulder is invalidated by the next issue.
CREATE TABLE webauthn_enrolment_codes (
    user_id    INTEGER PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,

    -- Hashed for the same reason password_hash is: a leaked database must not
    -- yield live codes.
    code_hash  TEXT NOT NULL,

    issued_at  INTEGER NOT NULL,
    expires_at INTEGER NOT NULL
);

-- Redemption arrives with a code and no nick — the user types it into a
-- sign-in page that does not yet know who they are — so the hash is the lookup
-- key.
CREATE INDEX webauthn_enrolment_codes_hash ON webauthn_enrolment_codes (code_hash);
