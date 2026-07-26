// Package iplink is the federation transport over IP (design §3, §11).
//
// # What this is, and why it is not what §11 first said
//
// §11 called for a "QUIC/Noise" link. That is not directly implementable:
// QUIC mandates TLS 1.3 as its handshake, and there is no standard way to run
// Noise inside it. The reason Noise was chosen was to avoid PKI — no
// certificate authorities, no chain of trust to bootstrap, just two static keys
// authenticating each other.
//
// TLS 1.3 gives exactly that here, because meshbbs already has the missing
// piece: node IDs are SELF-CERTIFYING. ID = BLAKE3(ed25519_pubkey)[:8], so a
// self-signed certificate whose key hashes to the expected node ID authenticates
// that node completely, with no authority to consult and no extra key material
// to bind to the identity. Noise over a separate X25519 static key would need
// that binding designed, implemented and reviewed; here there is nothing to
// bind.
//
// So: TCP with mutually-authenticated TLS 1.3, self-signed Ed25519 certificates
// pinned to node IDs, no third-party dependency at all. The sync protocol is
// batch-oriented — bundles every 15 to 30 minutes (§7.3) — so QUIC's
// multiplexing, 0-RTT and connection migration would buy almost nothing over
// it.
//
// # The certificate is a container, not a credential
//
// Nothing here checks a certificate's validity dates, issuer, or subject, and
// that is deliberate rather than an omission. The key IS the identity: a peer is
// authenticated because it proved possession of the private half of a key that
// hashes to the node ID we expected. Expiry would add nothing, since there is no
// authority to renew from and no revocation list to consult — the recovery path
// for a compromised key is a SUCCESSION record (§6.1.6), not a CRL.
package iplink

import (
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/aghman/meshbbs/internal/identity"
)

// ErrUnknownPeer is returned when a peer authenticates as a node we do not
// allow.
var ErrUnknownPeer = errors.New("peer is not an allowed node")

// ErrNoPeerCertificate is returned when a peer presents no certificate.
var ErrNoPeerCertificate = errors.New("peer presented no certificate")

// certLifetime is how long a generated certificate claims to be valid.
//
// Long, because nothing consults it: see the package comment. A short lifetime
// would imply a renewal process that does not exist and cannot exist without an
// authority.
const certLifetime = 100 * 365 * 24 * time.Hour

// selfSignedCert builds a certificate carrying the node's Ed25519 public key,
// signed by its own private key.
func selfSignedCert(key identity.NodeKey, now time.Time) (tls.Certificate, error) {
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		// The node ID in its compact rendering, so a human reading a packet
		// capture or an openssl dump sees an identifier they can check against
		// their peer list rather than an opaque blob.
		Subject:               pkix.Name{CommonName: key.ID().Compact()},
		NotBefore:             now.Add(-time.Hour), // tolerate modest clock skew
		NotAfter:              now.Add(certLifetime),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(nil, tmpl, tmpl, key.Public, key.Private)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("building the node certificate: %w", err)
	}
	return tls.Certificate{
		Certificate: [][]byte{der},
		PrivateKey:  key.Private,
		Leaf:        tmpl,
	}, nil
}

// PeerIDFromCertificate extracts the node ID a certificate authenticates.
//
// This is the whole authentication decision in one function: take the key the
// peer proved possession of, hash it, and that is who they are. There is no
// name to compare, no issuer to trust, and nothing an attacker can assert.
func PeerIDFromCertificate(raw [][]byte) (identity.NodeID, error) {
	if len(raw) == 0 {
		return identity.NodeID{}, ErrNoPeerCertificate
	}
	cert, err := x509.ParseCertificate(raw[0])
	if err != nil {
		return identity.NodeID{}, fmt.Errorf("parsing the peer certificate: %w", err)
	}
	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		// Refusing other key types is not pedantry. A node ID is defined as
		// BLAKE3 of an Ed25519 public key (§6.1.1), so any other algorithm
		// cannot correspond to a node ID at all, and accepting one would mean
		// authenticating a peer we have no way to name.
		return identity.NodeID{}, fmt.Errorf(
			"peer certificate uses %T; node identities are Ed25519 by definition", cert.PublicKey)
	}
	return identity.NodeIDFromPublicKey(pub), nil
}

// Authorizer decides whether an authenticated node may connect.
//
// Authentication and authorisation are separate on purpose: the TLS layer
// establishes WHO a peer is with certainty, and this decides whether that
// particular node is welcome. §11's peer table, with its trust levels of
// accept/quarantine/reject, is what a real implementation passes here.
type Authorizer func(identity.NodeID) bool

// AllowList returns an Authorizer permitting exactly the given nodes.
func AllowList(ids ...identity.NodeID) Authorizer {
	allowed := make(map[identity.NodeID]bool, len(ids))
	for _, id := range ids {
		allowed[id] = true
	}
	return func(id identity.NodeID) bool { return allowed[id] }
}

// tlsConfig builds the shared TLS settings for both directions.
//
// InsecureSkipVerify is set, and it is load-bearing rather than lazy: it turns
// OFF the library's chain-of-trust verification, which is meaningless without
// an authority, and hands the decision to VerifyPeerCertificate below. That
// callback is STRICTER than the default path, not weaker — it requires the
// peer's key to hash to a specific expected identity, which no CA-issued
// certificate could ever prove.
func tlsConfig(cert tls.Certificate, authorize Authorizer, expect *identity.NodeID) *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS13,
		// Every peer must present a certificate; there are no anonymous nodes.
		ClientAuth:         tls.RequireAnyClientCert,
		InsecureSkipVerify: true, //nolint:gosec // replaced by VerifyPeerCertificate
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			id, err := PeerIDFromCertificate(raw)
			if err != nil {
				return err
			}
			// When dialling we know exactly who we expect. Checking it here,
			// during the handshake, means a substituted peer never gets to send
			// us a single byte of application data.
			if expect != nil {
				if id != *expect {
					return fmt.Errorf("%w: dialled %s but the peer authenticated as %s",
						ErrUnknownPeer, expect.Compact(), id.Compact())
				}
				// Naming a peer to dial IS the authorization decision: the
				// caller asked for this exact node and got it. Requiring an
				// allow list as well would mean a node could not reach a peer it
				// had explicitly been told to contact.
				return nil
			}

			// Inbound, with no expectation to check against, the allow list is
			// the only thing standing between this port and the open internet.
			//
			// A nil Authorizer therefore REJECTS. An earlier version of this
			// function read `authorize != nil && !authorize(id)`, which failed
			// open: a listener configured without an allow list accepted every
			// node that dialled it, turning the instance into an open relay for
			// the federation. The doc comment said the opposite of what the code
			// did, and only a test that asserted the default caught it.
			if authorize == nil {
				return fmt.Errorf(
					"%w: %s (%s) — this link has no allow list, so it accepts nobody",
					ErrUnknownPeer, id.Compact(), id.Words())
			}
			if !authorize(id) {
				return fmt.Errorf("%w: %s (%s)", ErrUnknownPeer, id.Compact(), id.Words())
			}
			return nil
		},
	}
}
