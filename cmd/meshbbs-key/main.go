// Command meshbbs-key holds a user's DM private key on their own machine
// (design §8.2, tier 3).
//
// It is deliberately small. The BBS keeps doing everything it did — discovering
// keys, addressing mail, verifying signatures, delivering — because all of that
// works from public keys alone. The one thing it can no longer do for a
// tier-3 user is decrypt for display, so it hands over the sealed block and
// this opens it.
//
// A separate binary rather than a subcommand of meshbbs, because the whole
// point is that it runs somewhere the server does not. A subcommand would sit
// in the same binary a sysop runs on the machine holding everyone's mail, which
// is exactly the association tier 3 exists to break.
package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/aghman/meshbbs/internal/dmkey"
	"golang.org/x/term"
)

const usage = `meshbbs-key — keep your MeshBBS DM key on your own machine (§8.2)

  meshbbs-key init            generate a key and print the public half
  meshbbs-key pubkey          print the public half again
  meshbbs-key open            decrypt a message pasted on stdin

  --key <path>                use this key file instead of the default
  --passphrase-file <path>    read the passphrase from a file, for scripts

Reading mail:

  meshbbs-key open            then paste the block and press Ctrl-D
  pbpaste | meshbbs-key open  on macOS
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintf(os.Stderr, "meshbbs-key: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	var cmd, keyPath, passFile string
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "-h" || a == "--help" || a == "help":
			fmt.Print(usage)
			return nil
		case a == "--key":
			if i++; i >= len(args) {
				return errors.New("--key needs a path")
			}
			keyPath = args[i]
		case a == "--passphrase-file":
			if i++; i >= len(args) {
				return errors.New("--passphrase-file needs a path")
			}
			passFile = args[i]
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("unknown flag %q\n\n%s", a, usage)
		case cmd == "":
			cmd = a
		default:
			return fmt.Errorf("unexpected argument %q\n\n%s", a, usage)
		}
	}

	env := &dmkey.Env{
		Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		KeyPath:    keyPath,
		Passphrase: passphraseReader(passFile),
	}

	switch cmd {
	case "init":
		return dmkey.Init(env)
	case "pubkey":
		return dmkey.Pubkey(env)
	case "open":
		return dmkey.Open(env)
	case "":
		fmt.Print(usage)
		return nil
	default:
		return fmt.Errorf("unknown command %q\n\n%s", cmd, usage)
	}
}

// passphraseReader returns how this invocation will ask for the passphrase.
//
// # Why not stdin
//
// `open` reads the CIPHERTEXT from stdin, so the passphrase cannot come from
// there — the two would be the same stream and whichever read first would eat
// the other. It is read from the controlling terminal instead, which also means
// it is not echoed and does not land in shell history.
//
// The repo's convention elsewhere is --password-stdin, chosen so a secret never
// appears in argv where `ps` can see it. That reasoning is honoured here rather
// than the mechanism: --passphrase-file keeps it out of argv too, and is what a
// script can use when there is no terminal to prompt on.
func passphraseReader(file string) func(string) (string, error) {
	if file != "" {
		return func(string) (string, error) {
			b, err := os.ReadFile(file)
			if err != nil {
				return "", fmt.Errorf("read the passphrase file: %w", err)
			}
			// One trailing newline is what an editor leaves; anything else is
			// the user's and is kept.
			return strings.TrimRight(string(b), "\r\n"), nil
		}
	}
	return func(prompt string) (string, error) {
		tty, err := os.OpenFile(ttyPath, os.O_RDWR, 0)
		if err != nil {
			return "", fmt.Errorf("no terminal to ask for a passphrase on "+
				"(use --passphrase-file when running without one): %w", err)
		}
		defer tty.Close()

		fmt.Fprint(tty, prompt)
		pass, err := term.ReadPassword(int(tty.Fd()))
		fmt.Fprintln(tty)
		if err != nil {
			return "", fmt.Errorf("read the passphrase: %w", err)
		}
		return string(pass), nil
	}
}
