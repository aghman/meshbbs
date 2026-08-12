package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Door is one installed door game and everything the sysop decided about it
// (§9.1.1, §11.5).
//
// It is a database row rather than a config key because installing a door is a
// routine per-instance act, and `config check` refuses to start a BBS whose
// config file does not parse. A door row with a mistake in it should disable
// that door, not the board.
type Door struct {
	Name string
	Path string
	Args []string
	Cwd  string
	// EnvPassthrough names the server environment variables this door is
	// allowed to see. The runner replaces the environment rather than extending
	// it (§9.4), so anything not named here does not reach the door.
	EnvPassthrough []string
	DropfileType   string

	MaxConcurrent int
	NodeLock      bool
	WallClock     time.Duration
	// CPULimit and MemLimit are optional (§9.4). Whether they can be ENFORCED
	// depends on the operating system, which is not a question this package
	// can answer — internal/door does, and the launcher refuses a door whose
	// limits this platform cannot apply.
	CPULimit time.Duration
	MemLimit int64

	// RequiredCapability is what a USER needs in order to run this door, on top
	// of run_doors. Empty means run_doors is enough.
	RequiredCapability string

	// APILevel is the highest §9.1.1 level this door may use. 4 is act_as_user
	// and has to be set deliberately.
	APILevel int

	AnnounceArea    string
	AnnouncePerHour int
	StateQuota      int64

	// LeagueArea is the federated door area this door reports game events to
	// (§9.5), and the mirror image of AnnounceArea: that one must be local,
	// this one must be federated.
	LeagueArea    string
	LeaguePerHour int

	Enabled   bool
	CreatedAt int64
}

// Door API capability levels (§9.1.1). They nest: a door at level 3 also has 1
// and 2, because each level is a superset of the ones below rather than a
// separate permission.
const (
	// APISession is read-only session context. Always available.
	APISession = 1
	// APIState is the door's private key/value namespace. Always available.
	APIState = 2
	// APIAnnounce posts to a designated area as the DOOR's own identity, never
	// as the user. Always available, rate-limited.
	APIAnnounce = 3
	// APIActAsUser posts or sends as the logged-in user. Sysop grant, per door,
	// off by default.
	APIActAsUser = 4
)

// Dropfile formats a door may be given (§9.2).
const (
	DropfileNone     = "none"
	DropfileDoorSys  = "door.sys"
	DropfileDoor32   = "door32.sys"
	DropfileDorinfo1 = "dorinfo1.def"
)

// DoorAuthorPrefix marks a post as having been written by a door rather than a
// person (§9.1.1).
//
// # Why a marker and not the rendering
//
// §9.1.1 says a door's announcement is "Rendered as TRADEWARS (door@K7QM4X2P…)".
// That is twenty-six characters and a post's author field is SIXTEEN bytes,
// fixed by §6.2's byte budget and travelling on a 233-byte mesh MTU. The
// rendering cannot be the stored value.
//
// So the store keeps the marker and the door's name, and rendering the rest is
// the front end's job — which is where it belonged anyway, for the same reason
// §8.4 gives about display names: the node ID half of that string is a fact
// about which BBS you are reading, not a fact about the post, and a post that
// carried its own attribution would keep claiming it after being relayed.
//
// '!' is safe as the marker because a nick must start with a letter and may
// contain only letters, digits, underscore and hyphen (§6.7). No account can
// ever be spelled this way, so a door cannot be mistaken for a person and a
// person cannot impersonate a door.
const DoorAuthorPrefix = "!"

// MaxAnnounceDoorNameLen is the longest name a door that announces may have:
// the author field's sixteen bytes, less the marker.
//
// Checked when the door is saved rather than when it announces, so that a name
// nobody can post under is a configuration error the sysop sees immediately
// instead of a failure at two in the morning in a log they are not reading.
const MaxAnnounceDoorNameLen = 15

// DoorAuthor is the author string a door posts under.
func DoorAuthor(name string) string { return DoorAuthorPrefix + name }

// DoorNameFromAuthor reports whether an author is a door, and which one.
func DoorNameFromAuthor(author string) (string, bool) {
	name, ok := strings.CutPrefix(author, DoorAuthorPrefix)
	if !ok || name == "" {
		return "", false
	}
	return name, true
}

// ErrNoDoor is returned when no door has that name.
var ErrNoDoor = errors.New("no door by that name")

// ErrStateQuota is returned when a write would take a door past its state
// quota.
var ErrStateQuota = errors.New("that door has used up its saved-state allowance")

// Door state scopes (§9.1.1).
const (
	// ScopeUser is private to one player of one door.
	ScopeUser = "user"
	// ScopeGlobal is shared between that door's players — a high score table.
	ScopeGlobal = "global"
)

// MaxDoorStateKeyLen and MaxDoorStateValueLen bound one entry.
//
// The value bound is not the quota: the quota limits how much a door keeps in
// total, and this limits how much it can put anywhere in one call, so that a
// single write cannot be the whole allowance.
const (
	MaxDoorStateKeyLen   = 64
	MaxDoorStateValueLen = 4096
)

// MayAnnounce reports whether this door has somewhere to announce to.
//
// An unset area is not the same as a rate limit of zero: the sysop has not
// chosen a destination, so there is nowhere for a post to go, and the door
// should be told that rather than being throttled against a void.
func (d Door) MayAnnounce() bool {
	return d.APILevel >= APIAnnounce && strings.TrimSpace(d.AnnounceArea) != ""
}

// MayEmitEvents reports whether this door has a league to report to (§9.5).
//
// Same shape as MayAnnounce and the same distinction: no league area means the
// sysop has not chosen one, which the door should be told rather than being
// rate-limited against nothing.
func (d Door) MayEmitEvents() bool {
	return d.APILevel >= APIAnnounce && strings.TrimSpace(d.LeagueArea) != ""
}

// PutDoor inserts or replaces a door.
func (s *Store) PutDoor(ctx context.Context, d Door, actor string) error {
	if err := d.validate(); err != nil {
		return err
	}
	args, err := json.Marshal(nonNil(d.Args))
	if err != nil {
		return fmt.Errorf("encode door args: %w", err)
	}
	env, err := json.Marshal(nonNil(d.EnvPassthrough))
	if err != nil {
		return fmt.Errorf("encode door env_passthrough: %w", err)
	}

	created := d.CreatedAt
	if created == 0 {
		created = s.now()
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO doors (
			name, path, args, cwd, env_passthrough, dropfile_type,
			max_concurrent, node_lock, cpu_limit_secs, mem_limit_bytes,
			wall_clock_secs, required_capability, api_level,
			announce_area, announce_per_hour, state_quota_bytes,
			league_area, league_per_hour,
			enabled, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			path = excluded.path,
			args = excluded.args,
			cwd = excluded.cwd,
			env_passthrough = excluded.env_passthrough,
			dropfile_type = excluded.dropfile_type,
			max_concurrent = excluded.max_concurrent,
			node_lock = excluded.node_lock,
			cpu_limit_secs = excluded.cpu_limit_secs,
			mem_limit_bytes = excluded.mem_limit_bytes,
			wall_clock_secs = excluded.wall_clock_secs,
			required_capability = excluded.required_capability,
			api_level = excluded.api_level,
			announce_area = excluded.announce_area,
			announce_per_hour = excluded.announce_per_hour,
			state_quota_bytes = excluded.state_quota_bytes,
			league_area = excluded.league_area,
			league_per_hour = excluded.league_per_hour,
			enabled = excluded.enabled`,
		d.Name, d.Path, string(args), d.Cwd, string(env), d.DropfileType,
		d.MaxConcurrent, boolToInt(d.NodeLock),
		int64(d.CPULimit/time.Second), d.MemLimit,
		int64(d.WallClock/time.Second), d.RequiredCapability, d.APILevel,
		d.AnnounceArea, d.AnnouncePerHour, d.StateQuota,
		d.LeagueArea, d.LeaguePerHour,
		boolToInt(d.Enabled), created)
	if err != nil {
		return fmt.Errorf("save door: %w", err)
	}

	// Granting act_as_user is exactly the kind of decision someone should be
	// able to find afterwards, so it is audited as its own event rather than
	// being one field of a door that changed.
	detail := fmt.Sprintf("api_level=%d", d.APILevel)
	if d.APILevel >= APIActAsUser {
		if err := s.audit(ctx, actor, "door.grant_act_as_user", d.Name, detail); err != nil {
			return err
		}
	}
	return s.audit(ctx, actor, "door.save", d.Name, detail)
}

// GetDoor returns one door.
func (s *Store) GetDoor(ctx context.Context, name string) (Door, error) {
	rows, err := s.queryDoors(ctx, `WHERE name = ?`, name)
	if err != nil {
		return Door{}, err
	}
	if len(rows) == 0 {
		return Door{}, fmt.Errorf("%w: %s", ErrNoDoor, name)
	}
	return rows[0], nil
}

// ListDoors returns every door, enabled or not, by name.
func (s *Store) ListDoors(ctx context.Context) ([]Door, error) {
	return s.queryDoors(ctx, `ORDER BY name`)
}

// DeleteDoor removes a door and the state it kept.
func (s *Store) DeleteDoor(ctx context.Context, name, actor string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM doors WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("delete door: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("%w: %s", ErrNoDoor, name)
	}
	return s.audit(ctx, actor, "door.delete", name, "")
}

func (s *Store) queryDoors(ctx context.Context, where string, args ...any) ([]Door, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT name, path, args, cwd, env_passthrough, dropfile_type,
		       max_concurrent, node_lock, cpu_limit_secs, mem_limit_bytes,
		       wall_clock_secs, required_capability, api_level,
		       announce_area, announce_per_hour, state_quota_bytes,
		       league_area, league_per_hour,
		       enabled, created_at
		FROM doors `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list doors: %w", err)
	}
	defer rows.Close()

	out := []Door{}
	for rows.Next() {
		var (
			d                        Door
			rawArgs, rawEnv          string
			nodeLock, enabled        int
			cpuSecs, wallSecs        int64
			memBytes, stateQuotaByte int64
		)
		if err := rows.Scan(
			&d.Name, &d.Path, &rawArgs, &d.Cwd, &rawEnv, &d.DropfileType,
			&d.MaxConcurrent, &nodeLock, &cpuSecs, &memBytes,
			&wallSecs, &d.RequiredCapability, &d.APILevel,
			&d.AnnounceArea, &d.AnnouncePerHour, &stateQuotaByte,
			&d.LeagueArea, &d.LeaguePerHour,
			&enabled, &d.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan door: %w", err)
		}
		if err := json.Unmarshal([]byte(rawArgs), &d.Args); err != nil {
			return nil, fmt.Errorf("door %s: decode args: %w", d.Name, err)
		}
		if err := json.Unmarshal([]byte(rawEnv), &d.EnvPassthrough); err != nil {
			return nil, fmt.Errorf("door %s: decode env_passthrough: %w", d.Name, err)
		}
		d.NodeLock = nodeLock != 0
		d.Enabled = enabled != 0
		d.CPULimit = time.Duration(cpuSecs) * time.Second
		d.MemLimit = memBytes
		d.WallClock = time.Duration(wallSecs) * time.Second
		d.StateQuota = stateQuotaByte
		out = append(out, d)
	}
	return out, rows.Err()
}

func (d Door) validate() error {
	if n := strings.TrimSpace(d.Name); n == "" || len(n) > 32 {
		return fmt.Errorf("a door's name must be 1 to 32 characters, got %q", d.Name)
	}
	if strings.TrimSpace(d.Path) == "" {
		return errors.New("a door needs a path to its executable")
	}
	if strings.TrimSpace(d.Cwd) == "" {
		return errors.New("a door needs a working directory")
	}
	if d.APILevel < APISession || d.APILevel > APIActAsUser {
		return fmt.Errorf("api_level must be %d to %d, got %d",
			APISession, APIActAsUser, d.APILevel)
	}
	if d.WallClock <= 0 {
		return errors.New("a door needs a wall-clock limit")
	}
	switch d.DropfileType {
	case DropfileNone, DropfileDoorSys, DropfileDoor32, DropfileDorinfo1:
	default:
		return fmt.Errorf("unknown dropfile type %q", d.DropfileType)
	}
	if d.MayAnnounce() && len(d.Name) > MaxAnnounceDoorNameLen {
		return fmt.Errorf(
			"door %s: a door that announces needs a name of at most %d characters, "+
				"because a post's author field is %d bytes and one is spent on the "+
				"marker that says a door wrote it",
			d.Name, MaxAnnounceDoorNameLen, MaxAnnounceDoorNameLen+1)
	}
	if d.RequiredCapability != "" && !isKnownCapability(d.RequiredCapability) {
		return fmt.Errorf("unknown capability %q (known: %s)",
			d.RequiredCapability, strings.Join(KnownCapabilities, ", "))
	}
	return nil
}

// ---------------------------------------------------------------------------
// Level 2 — door state (§9.1.1)
// ---------------------------------------------------------------------------

// DoorStateGet reads one value. A missing key is not an error: a door asking
// for a saved game it has never written is the ordinary first-run case, and
// making that an error would have every door treat errors as normal.
func (s *Store) DoorStateGet(ctx context.Context, door, scope, owner, key string) (string, bool, error) {
	if err := checkScope(scope, owner); err != nil {
		return "", false, err
	}
	var value string
	err := s.db.QueryRowContext(ctx, `
		SELECT value FROM door_state
		WHERE door = ? AND scope = ? AND owner = ? AND key = ?`,
		door, scope, owner, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read door state: %w", err)
	}
	return value, true, nil
}

// DoorStateSet writes one value, within the door's quota.
func (s *Store) DoorStateSet(ctx context.Context, door, scope, owner, key, value string, quota int64) error {
	if err := checkScope(scope, owner); err != nil {
		return err
	}
	if k := strings.TrimSpace(key); k == "" || len(key) > MaxDoorStateKeyLen {
		return fmt.Errorf("a state key must be 1 to %d characters", MaxDoorStateKeyLen)
	}
	if len(value) > MaxDoorStateValueLen {
		return fmt.Errorf("a state value may be at most %d bytes, got %d",
			MaxDoorStateValueLen, len(value))
	}

	// The quota is checked and the write applied in ONE transaction. Checking
	// first and writing after would let two of a door's sessions each see room
	// and both take it, which is the ordinary shape of a quota that does not
	// hold: a busy door is exactly when it matters.
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin door state write: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	if quota > 0 {
		var used int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(SUM(length(key) + length(value)), 0)
			FROM door_state WHERE door = ?`, door).Scan(&used); err != nil {
			return fmt.Errorf("measure door state: %w", err)
		}
		var replacing int64
		if err := tx.QueryRowContext(ctx, `
			SELECT COALESCE(length(key) + length(value), 0) FROM door_state
			WHERE door = ? AND scope = ? AND owner = ? AND key = ?`,
			door, scope, owner, key).Scan(&replacing); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("measure door state entry: %w", err)
		}
		if after := used - replacing + int64(len(key)+len(value)); after > quota {
			return fmt.Errorf("%w (%d of %d bytes used)", ErrStateQuota, used, quota)
		}
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO door_state (door, scope, owner, key, value, updated_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(door, scope, owner, key) DO UPDATE SET
			value = excluded.value, updated_at = excluded.updated_at`,
		door, scope, owner, key, value, s.now()); err != nil {
		return fmt.Errorf("write door state: %w", err)
	}
	return tx.Commit()
}

// DoorStateDelete removes one value. Deleting what is not there succeeds, so
// that a door tidying up does not have to ask first.
func (s *Store) DoorStateDelete(ctx context.Context, door, scope, owner, key string) error {
	if err := checkScope(scope, owner); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `
		DELETE FROM door_state
		WHERE door = ? AND scope = ? AND owner = ? AND key = ?`,
		door, scope, owner, key); err != nil {
		return fmt.Errorf("delete door state: %w", err)
	}
	return nil
}

// DoorStateKeys lists the keys in one namespace, sorted.
func (s *Store) DoorStateKeys(ctx context.Context, door, scope, owner string) ([]string, error) {
	if err := checkScope(scope, owner); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT key FROM door_state
		WHERE door = ? AND scope = ? AND owner = ? ORDER BY key`,
		door, scope, owner)
	if err != nil {
		return nil, fmt.Errorf("list door state: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("scan door state key: %w", err)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DoorStateUsed returns how many bytes of its quota a door is using.
func (s *Store) DoorStateUsed(ctx context.Context, door string) (int64, error) {
	var used int64
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(SUM(length(key) + length(value)), 0)
		FROM door_state WHERE door = ?`, door).Scan(&used)
	if err != nil {
		return 0, fmt.Errorf("measure door state: %w", err)
	}
	return used, nil
}

// checkScope rejects the combinations the schema forbids, with a message that
// names the mistake rather than surfacing a CHECK constraint.
func checkScope(scope, owner string) error {
	switch scope {
	case ScopeGlobal:
		if owner != "" {
			return fmt.Errorf("global door state has no owner, got %q", owner)
		}
	case ScopeUser:
		if strings.TrimSpace(owner) == "" {
			return errors.New("user-scoped door state needs a nick")
		}
	default:
		return fmt.Errorf("unknown door state scope %q (want %q or %q)",
			scope, ScopeUser, ScopeGlobal)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Level 4 — the one-time notice (§9.1.1)
// ---------------------------------------------------------------------------

// DoorNoticeNeeded reports whether this user still has to be told that this
// door can act as them, and records that they have been told.
//
// One call rather than a read and a write, because the two must not be
// separable: a door acting as a user twice in quick succession from two
// sessions would otherwise show the notice twice or, worse, neither time. It
// returns true exactly once per (door, user), forever.
func (s *Store) DoorNoticeNeeded(ctx context.Context, door, nick string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO door_notices (door, nick, notified_at) VALUES (?, ?, ?)
		ON CONFLICT(door, nick) DO NOTHING`,
		door, nick, s.now())
	if err != nil {
		return false, fmt.Errorf("record door notice: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("record door notice: %w", err)
	}
	return n > 0, nil
}

// nonNil turns a nil slice into an empty one, so that JSON encoding produces
// [] rather than null and the column's default and its written value agree.
func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
