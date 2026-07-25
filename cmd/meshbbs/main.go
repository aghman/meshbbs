// Command meshbbs is a cross-platform BBS federated over Meshtastic LoRa mesh.
//
// See docs/design.md for the architecture. Phase 0 (the skeleton) provides
// identity, configuration, storage and the CLI; the SSH front end arrives in
// Phase 1 and federation in Phase 2.
package main

import (
	"os"

	"github.com/aghman/meshbbs/internal/cli"
)

func main() {
	os.Exit(cli.Execute())
}
