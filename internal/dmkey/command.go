package dmkey

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aghman/meshbbs/internal/keyring"
	"github.com/aghman/meshbbs/internal/store"
)

// Env is everything the commands touch outside themselves, so a test can drive
// them without a terminal, a home directory or a real file.
type Env struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Passphrase reads a passphrase from the user. It is a function rather than
	// a string because of `open`: stdin carries the CIPHERTEXT there, so the
	// passphrase has to come from the controlling terminal instead, and the two
	// cannot be the same stream. See readPassphrase in the command binary.
	Passphrase func(prompt string) (string, error)
	// KeyPath is the key file. Empty means DefaultPath.
	KeyPath string
}

func (e *Env) path() (string, error) {
	if e.KeyPath != "" {
		return e.KeyPath, nil
	}
	return DefaultPath()
}

// Init generates a keypair, writes the wrapped private half, and prints the
// public half for the user to give their BBS.
func Init(e *Env) error {
	path, err := e.path()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(path); statErr == nil {
		// Checked before asking for a passphrase. Prompting first and refusing
		// after would be an invitation to type the passphrase again somewhere
		// it will not be needed.
		return fmt.Errorf("%w at %s\n\n"+
			"Generating a new one would make every message ever sent to the old key\n"+
			"unreadable — the mail stays in your inbox and becomes ciphertext nobody\n"+
			"can open. Move that file somewhere safe first if you really mean to.",
			ErrKeyFileExists, path)
	}

	pass, err := e.Passphrase("Choose a passphrase for this key: ")
	if err != nil {
		return err
	}
	again, err := e.Passphrase("Again: ")
	if err != nil {
		return err
	}
	if pass != again {
		return errors.New("the two passphrases do not match")
	}
	// No minimum length and no complexity rule, deliberately. This wraps a key
	// on the user's own machine; a rule here would push people toward the
	// passwords rules produce, and the file is 0600 already.
	if strings.TrimSpace(pass) == "" {
		return errors.New("an empty passphrase would leave the key file unprotected")
	}

	priv, pub, err := keyring.Generate(nil)
	if err != nil {
		return err
	}
	defer priv.Zero()

	wrapped, err := keyring.Wrap(priv, pass)
	if err != nil {
		return err
	}
	if err := Save(path, wrapped); err != nil {
		return err
	}

	fmt.Fprintf(e.Stdout, "Wrote %s\n\n", path)
	fmt.Fprintf(e.Stdout, "Your public key — give this to your BBS:\n\n  %s\n\n", pub)
	fmt.Fprintf(e.Stdout,
		"  meshbbs user dm-key <your nick> %s\n\n"+
			"Back that file up. It is the only copy, and without it every message\n"+
			"sent to you becomes unreadable — including ones already in your inbox.\n", pub)
	if warn := PermissionWarning(path); warn != "" {
		fmt.Fprintf(e.Stderr, "\nwarning: %s\n", warn)
	}
	return nil
}

// Pubkey prints the public half again, for a user who needs to hand it over a
// second time.
//
// It needs the passphrase, because the public key is derived from the private
// one rather than stored beside it. Storing it in the clear would save a prompt
// and would mean the file said who its owner is — worth more to somebody who
// steals it than the prompt costs.
func Pubkey(e *Env) error {
	path, err := e.path()
	if err != nil {
		return err
	}
	priv, err := unlock(e, path)
	if err != nil {
		return err
	}
	defer priv.Zero()

	pub, err := priv.Public()
	if err != nil {
		return err
	}
	fmt.Fprintf(e.Stdout, "%s\n", pub)
	return nil
}

// Open decrypts one armoured DM read from stdin.
func Open(e *Env) error {
	path, err := e.path()
	if err != nil {
		return err
	}

	// Read the ciphertext BEFORE asking for the passphrase. A user pasting into
	// a pipe that is waiting on a hidden prompt sees nothing happen, and the
	// most likely thing they do next is type the passphrase into the paste.
	in, err := io.ReadAll(io.LimitReader(e.Stdin, MaxArmourBytes+1))
	if err != nil {
		return fmt.Errorf("read the message: %w", err)
	}
	sealed, err := Unarmour(string(in))
	if err != nil {
		return err
	}

	priv, err := unlock(e, path)
	if err != nil {
		return err
	}
	defer priv.Zero()

	plain, err := keyring.Open(priv, sealed)
	if err != nil {
		return fmt.Errorf("this message is not readable with this key: %w\n\n"+
			"Either it was sent to a different key of yours, or the paste was\n"+
			"incomplete. Copy the whole block including both ----- lines.", err)
	}

	// The payload is the same envelope the server stores, so a subject that was
	// sealed with the body comes back out here rather than being lost (§8.2's
	// note on why [D7] does not cover the subject).
	payload, err := store.UnmarshalSealedPayload(plain)
	if err != nil {
		// Older or foreign payloads: show the bytes rather than refusing. The
		// user asked to read their mail, and a framing they do not recognise is
		// not a reason to withhold what decrypted successfully.
		fmt.Fprintf(e.Stderr, "warning: unrecognised message framing; showing the raw contents\n")
		e.Stdout.Write(plain)
		fmt.Fprintln(e.Stdout)
		return nil
	}
	if payload.Subject != "" {
		fmt.Fprintf(e.Stdout, "Subject: %s\n\n", payload.Subject)
	}
	fmt.Fprintf(e.Stdout, "%s\n", payload.Text)
	return nil
}

// unlock loads and decrypts the key file.
func unlock(e *Env, path string) (keyring.PrivateKey, error) {
	wrapped, err := Load(path)
	if err != nil {
		return keyring.PrivateKey{}, err
	}
	if warn := PermissionWarning(path); warn != "" {
		fmt.Fprintf(e.Stderr, "warning: %s\n", warn)
	}
	pass, err := e.Passphrase("Passphrase: ")
	if err != nil {
		return keyring.PrivateKey{}, err
	}
	return keyring.Unwrap(wrapped, pass)
}
