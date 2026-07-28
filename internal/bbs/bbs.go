// Package bbs is the domain service layer: the operations a session performs,
// independent of how the user reached us (SSH, telnet, or a test).
//
// It sits between the TUI and the store, and it owns the rule that every
// replicable action becomes a signed record in the log (§6.2). Phase 1 has no
// federation, but posts and DMs are already records — so Phase 2 replicates
// them without reworking anything here.
package bbs

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/aghman/meshbbs/internal/clock"
	"github.com/aghman/meshbbs/internal/identity"
	"github.com/aghman/meshbbs/internal/keyring"
	"github.com/aghman/meshbbs/internal/record"
	"github.com/aghman/meshbbs/internal/store"
)

// Service performs BBS operations on behalf of a user.
type Service struct {
	store *store.Store
	key   identity.NodeKey
	clock clock.Clock

	mu        sync.RWMutex
	publisher Publisher
}

// Publisher is how a locally written record reaches the federation.
//
// It is an interface set at runtime rather than a constructor argument because
// a BBS runs perfectly well without one — Phase 1 shipped that way, and an
// instance with no radio still has forums. When federation is up, the sync
// engine is what lands here.
type Publisher interface {
	Publish(area record.AreaTag, recs []*record.Record) error
}

// SetPublisher wires local writes to the federation, or unwires them with nil.
func (s *Service) SetPublisher(p Publisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.publisher = p
}

// publish hands a new record to the federation, if there is one.
//
// # Why a failure here does not fail the post
//
// The record is already written and indexed: the user's post exists, and it is
// theirs. Federation is how OTHER instances find out, and §7.3's whole design
// is that a missed transmission is repaired by anti-entropy rather than retried
// by the caller. Returning an error would tell a user their post failed when it
// did not, and would tempt them to write it again.
func (s *Service) publish(area record.AreaTag, rec *record.Record) {
	s.mu.RLock()
	p := s.publisher
	s.mu.RUnlock()
	if p == nil {
		return
	}
	if err := p.Publish(area, []*record.Record{rec}); err != nil {
		OnPublishError(err)
	}
}

// OnPublishError reports a federation failure. Set by serve; the default is
// silence, since a BBS without a radio has nothing to report.
var OnPublishError = func(error) {}

// New builds a Service.
func New(st *store.Store, key identity.NodeKey, clk clock.Clock) *Service {
	return &Service{store: st, key: key, clock: clk}
}

// Store exposes the underlying store for read-only queries from the UI.
func (s *Service) Store() *store.Store { return s.store }

// NodeID returns this instance's ID.
func (s *Service) NodeID() identity.NodeID { return s.key.ID() }

// ErrNotPermitted is returned when a user lacks a required capability.
var ErrNotPermitted = errors.New("not permitted")

// ErrNoRecipient is returned when a DM names an unknown user.
var ErrNoRecipient = errors.New("no such user on this BBS")

// Post publishes a message to a forum area.
//
// The capability check is the [N7] gate: anyone may post to a local area, but
// posting to a FEDERATED area spends the network's shared airtime and requires
// post_federated. The error explains the remedy rather than just refusing,
// because the user cannot grant it themselves.
func (s *Service) Post(ctx context.Context, author, areaName, subject, text string) (record.ID, error) {
	area, err := s.store.GetArea(ctx, areaName)
	if err != nil {
		return record.ID{}, err
	}
	if area.ReadOnly {
		return record.ID{}, fmt.Errorf("%w: %s is read-only", ErrNotPermitted, area.Name)
	}
	if area.Federated {
		ok, err := s.store.HasCapability(ctx, author, store.CapPostFederated)
		if err != nil {
			return record.ID{}, err
		}
		if !ok {
			return record.ID{}, fmt.Errorf(
				"%w: %s is a federated area, and posting there spends the mesh network's "+
					"shared airtime. Ask the sysop for the %s capability",
				ErrNotPermitted, area.Name, store.CapPostFederated)
		}
	}

	text = strings.TrimRight(text, "\n")
	if strings.TrimSpace(text) == "" {
		return record.ID{}, errors.New("post is empty")
	}

	body, err := store.MarshalPostBody(store.PostBody{
		Author: author, Subject: subject, Text: text,
	})
	if err != nil {
		return record.ID{}, err
	}

	rec, err := s.newRecord(ctx, record.TypePost, area.Tag, body)
	if err != nil {
		return record.ID{}, err
	}
	if err := s.store.PutRecord(ctx, rec); err != nil {
		return record.ID{}, err
	}
	if err := s.store.IndexPost(ctx, rec.ID(), author, subject); err != nil {
		return record.ID{}, err
	}

	// Hand it to the federation immediately when the area is on the mesh.
	//
	// Without this a new post waits for the next anti-entropy beat, which at
	// fifty instances is hours (§7.3). Anti-entropy would eventually carry it,
	// but "eventually" is the wrong answer for the post someone just wrote:
	// §7.3 has a push path precisely so the common case is prompt and the slow
	// path is only for repair.
	//
	// Local-only areas are never published. That is the sysop's decision about
	// what belongs on other people's radios, and it is checked here as well as
	// in the engine.
	if area.Federated {
		s.publish(area.Tag, rec)
	}
	return rec.ID(), nil
}

// SendDM composes and stores an encrypted direct message.
//
// Note what this does NOT need: the sender's private key. Sealing requires
// only the recipient's public key (§8.2), which is the boundary that keeps
// client-held keys (tier 3) an addition rather than a rewrite.
//
// The recipient reference may be a nick, or nick@node where node is a literal
// ID or a local alias. Aliases are resolved HERE, at compose time, and the
// resolved node ID is what goes into the record — an alias never travels on
// the wire (§6.1.4.1).
func (s *Service) SendDM(ctx context.Context, sender, recipientRef, subject, text string) (record.ID, error) {
	nick, node, err := s.ResolveRecipient(ctx, recipientRef)
	if err != nil {
		return record.ID{}, err
	}
	if node != s.key.ID() {
		// Phase 1 is single-node; off-node delivery arrives with federation.
		return record.ID{}, fmt.Errorf(
			"%s is on another BBS (%s), and federation is not available yet",
			nick, node.Short())
	}

	pub, err := s.store.DMPublicKey(ctx, nick)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return record.ID{}, fmt.Errorf("%w: %s", ErrNoRecipient, nick)
		}
		return record.ID{}, err
	}

	// The subject is sealed with the body, not carried in the clear: see the
	// note on store.DMBody for why [D7] does not cover it.
	payload, err := store.MarshalSealedPayload(store.SealedPayload{
		Subject: subject, Text: text,
	})
	if err != nil {
		return record.ID{}, err
	}
	sealed, err := keyring.Seal(pub, payload)
	if err != nil {
		return record.ID{}, err
	}

	body, err := store.MarshalDMBody(store.DMBody{
		Sender: sender, Recipient: nick, Sealed: sealed,
	})
	if err != nil {
		return record.ID{}, err
	}

	rec, err := s.newRecord(ctx, record.TypeDM, record.AreaTag{}, body)
	if err != nil {
		return record.ID{}, err
	}
	if err := s.store.PutRecord(ctx, rec); err != nil {
		return record.ID{}, err
	}
	parsed, err := store.UnmarshalDMBody(rec.Body)
	if err != nil {
		return record.ID{}, err
	}
	if err := s.store.IndexDM(ctx, rec.ID(), parsed, s.key.ID(), s.clock.Now().Unix()); err != nil {
		return record.ID{}, err
	}
	return rec.ID(), nil
}

// ResolveRecipient splits a recipient reference into nick and node ID.
//
// Accepted forms:
//
//	austin              a user on this BBS
//	austin@K7QM...      a literal node ID, in either rendering
//	austin@pnw          a local alias (§6.1.4)
//
// An unresolvable alias produces an error naming the remedy: because aliases
// are sysop-owned ([N1]), the user's remedy is to ask, not to fix it.
func (s *Service) ResolveRecipient(ctx context.Context, ref string) (string, identity.NodeID, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", identity.NodeID{}, errors.New("no recipient given")
	}

	nick, nodeRef, hasNode := strings.Cut(ref, "@")
	nick = strings.TrimSpace(nick)
	if nick == "" {
		return "", identity.NodeID{}, fmt.Errorf("%q has no user part", ref)
	}
	if !hasNode || strings.TrimSpace(nodeRef) == "" {
		return nick, s.key.ID(), nil
	}

	node, err := s.store.ResolveNodeRef(ctx, nodeRef)
	if err != nil {
		return "", identity.NodeID{}, err
	}
	return nick, node, nil
}

// OpenDM decrypts a message for display.
//
// This is the ONLY operation that needs a user's private key, and it takes the
// passphrase explicitly rather than reading it from session state. That is
// deliberate: it keeps the set of code paths touching private material small
// and visible, and it is exactly the operation tier 3 will move off the server.
func (s *Service) OpenDM(ctx context.Context, nick, passphrase string, sealed []byte) (store.SealedPayload, error) {
	wrapped, err := s.store.WrappedDMKey(ctx, nick)
	if err != nil {
		return store.SealedPayload{}, err
	}
	priv, err := keyring.Unwrap(wrapped, passphrase)
	if err != nil {
		return store.SealedPayload{}, err
	}
	defer priv.Zero()

	plain, err := keyring.Open(priv, sealed)
	if err != nil {
		return store.SealedPayload{}, err
	}
	return store.UnmarshalSealedPayload(plain)
}

// newRecord reserves a sequence number and builds a signed record.
//
// The ordering matters and is the §6.2.1 rule-3 discipline: NextSeq commits
// the high-water mark durably before the record exists, so a crash burns a
// sequence number rather than risking its reuse with different content.
func (s *Service) newRecord(ctx context.Context, typ record.Type, area record.AreaTag, body []byte) (*record.Record, error) {
	seq, err := s.store.NextSeq(ctx)
	if err != nil {
		return nil, err
	}
	return record.New(s.key, record.Record{
		Origin: s.key.ID(),
		Seq:    seq,
		TS:     uint32(s.clock.Now().Unix()),
		Type:   typ,
		Area:   area,
		Body:   body,
	})
}

// EnsureDMKey creates a DM keypair for a user if they have none.
//
// Called at signup and at first login for accounts created by the CLI, which
// cannot know a passphrase.
func (s *Service) EnsureDMKey(ctx context.Context, nick, passphrase string) error {
	if _, err := s.store.DMPublicKey(ctx, nick); err == nil {
		return nil
	}
	priv, pub, err := keyring.Generate(nil)
	if err != nil {
		return err
	}
	defer priv.Zero()
	wrapped, err := keyring.Wrap(priv, passphrase)
	if err != nil {
		return err
	}
	return s.store.SetDMKey(ctx, nick, pub, wrapped)
}

// SeedDefaultAreas creates the starter areas a new BBS needs.
//
// All local-only: a fresh instance must not start spending mesh airtime
// because of a default (§6.3).
func (s *Service) SeedDefaultAreas(ctx context.Context) error {
	defaults := []struct{ name, desc string }{
		{"general", "General discussion"},
		{"tech", "Radios, antennas, and the mesh"},
		{"sysop", "Announcements from the sysop"},
	}
	for _, d := range defaults {
		if _, err := s.store.GetArea(ctx, d.name); err == nil {
			continue
		}
		if _, err := s.store.CreateArea(ctx, d.name, d.desc, false); err != nil {
			if !errors.Is(err, store.ErrAreaExists) {
				return err
			}
		}
	}
	return nil
}

// VerifyPassphrase checks that a passphrase unwraps a user's DM key, without
// decrypting any message.
//
// This backs the session unlock step. It exists as its own operation so the
// UI never has to fake a decryption to test a passphrase — and so the set of
// code paths that touch private key material stays small and greppable.
func (s *Service) VerifyPassphrase(ctx context.Context, nick, passphrase string) error {
	wrapped, err := s.store.WrappedDMKey(ctx, nick)
	if err != nil {
		return err
	}
	priv, err := keyring.Unwrap(wrapped, passphrase)
	if err != nil {
		return err
	}
	priv.Zero()
	return nil
}
