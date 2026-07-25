package identity

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/rng"
)

// unixOnly skips a test that asserts Unix file permissions. Windows has no
// equivalent representation, so these checks are meaningless there rather than
// merely inconvenient.
func unixOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Unix file permissions are not represented on Windows")
	}
}

// randomIDs yields deterministic pseudo-random node IDs for property tests.
func randomIDs(t *testing.T, n int) []NodeID {
	t.Helper()
	src := rng.NewSeeded(0xB85EED)
	out := make([]NodeID, n)
	for i := range out {
		src.Read(out[i][:])
	}
	return out
}

// §12.2: base32 ↔ bytes round-trips losslessly.
func TestBase32RoundTrip(t *testing.T) {
	for _, id := range randomIDs(t, 2000) {
		s := id.Compact()
		if len(s) != 13 {
			t.Fatalf("expected 13 characters, got %d (%q)", len(s), s)
		}
		got, err := ParseNodeID(s)
		if err != nil {
			t.Fatalf("ParseNodeID(%q): %v", s, err)
		}
		if got != id {
			t.Fatalf("round-trip mismatch: %x -> %q -> %x", id, s, got)
		}
	}
}

// The grouped display form must parse back identically to the compact form.
func TestGroupedFormParses(t *testing.T) {
	for _, id := range randomIDs(t, 500) {
		got, err := ParseNodeID(id.String())
		if err != nil {
			t.Fatalf("ParseNodeID(%q): %v", id.String(), err)
		}
		if got != id {
			t.Fatalf("grouped round-trip mismatch for %q", id.String())
		}
	}
}

// §12.2: words ↔ bytes round-trips losslessly, and always uses exactly 6 words.
func TestWordsRoundTrip(t *testing.T) {
	for _, id := range randomIDs(t, 2000) {
		w := EncodeWords(id)
		if len(w) != WordCount {
			t.Fatalf("expected %d words, got %d", WordCount, len(w))
		}
		got, err := DecodeWords(w)
		if err != nil {
			t.Fatalf("DecodeWords(%v): %v", w, err)
		}
		if got != id {
			t.Fatalf("word round-trip mismatch: %x -> %v -> %x", id, w, got)
		}
	}
}

// Both renderings must decode to the same ID — they are two views of one
// identifier, which is the property §6.1.4.2 rests on.
func TestBothRenderingsAgree(t *testing.T) {
	for _, id := range randomIDs(t, 500) {
		fromB32, err := ParseNodeID(id.Compact())
		if err != nil {
			t.Fatal(err)
		}
		fromWords, err := ParseNodeID(id.Words())
		if err != nil {
			t.Fatalf("ParseNodeID(%q): %v", id.Words(), err)
		}
		if fromB32 != fromWords {
			t.Fatalf("renderings disagree for %x: %q vs %q", id, id.Compact(), id.Words())
		}
	}
}

// The checksum exists so a misheard word is rejected rather than silently
// decoding to a different valid-looking ID. Verify it actually catches
// single-word substitutions most of the time.
func TestWordChecksumCatchesSubstitution(t *testing.T) {
	var caught, total int
	for _, id := range randomIDs(t, 400) {
		w := EncodeWords(id)
		for i := range w {
			corrupted := append([]string(nil), w...)
			// Substitute a different word deterministically.
			idx := (wordIndex[w[i]] + 977) % len(wordList)
			corrupted[i] = wordList[idx]
			total++
			if got, err := DecodeWords(corrupted); err != nil || got != id {
				caught++
			}
		}
	}
	// With 2 checksum bits, ~75% of single-word errors are caught by the
	// checksum, and the rest change the payload (so decode to a different ID,
	// which this test also counts as caught since got != id).
	if caught != total {
		t.Fatalf("only %d/%d single-word corruptions detected", caught, total)
	}
}

func TestWordChecksumRejectsBadChecksum(t *testing.T) {
	id := randomIDs(t, 1)[0]
	w := EncodeWords(id)
	// The final word carries 9 payload bits followed by the 2 checksum bits.
	// Flip only its lowest bit so the payload is untouched and exactly one
	// checksum bit changes — this isolates the checksum mechanism. (Adding 1
	// instead would carry into the payload whenever the checksum is 0b11,
	// yielding a different ID whose own checksum may coincidentally match.)
	last := wordIndex[w[WordCount-1]]
	w[WordCount-1] = wordList[last^1]
	if _, err := DecodeWords(w); err == nil {
		t.Fatal("expected checksum failure, got nil error")
	} else if !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("expected a checksum error, got: %v", err)
	}
}

// Crockford's forgiving decode: I/L map to 1 and O maps to 0, so a human who
// transcribes an ID by eye is not punished for the obvious substitutions.
func TestCrockfordForgivingDecode(t *testing.T) {
	id := randomIDs(t, 1)[0]
	s := id.Compact()
	// Only meaningful if the ID contains a 1 or a 0 to substitute.
	for _, sub := range []struct{ real, typo string }{{"1", "I"}, {"1", "l"}, {"0", "O"}} {
		if !strings.Contains(s, sub.real) {
			continue
		}
		typoed := strings.Replace(s, sub.real, sub.typo, 1)
		got, err := ParseNodeID(typoed)
		if err != nil {
			t.Fatalf("ParseNodeID(%q) after %s->%s: %v", typoed, sub.real, sub.typo, err)
		}
		if got != id {
			t.Fatalf("forgiving decode failed for %s->%s", sub.real, sub.typo)
		}
	}
}

func TestParseRejectsGarbage(t *testing.T) {
	for _, s := range []string{
		"", "   ", "not-a-node-id",
		"K7QM4X2PB9TF",   // 12 chars: too short
		"K7QM4X2PB9TFRR", // 14 chars: too long
		"K7QM4X2PB9TFU",  // U is not in the Crockford alphabet
		"abandon abandon abandon abandon abandon", // 5 words
	} {
		if _, err := ParseNodeID(s); err == nil {
			t.Errorf("ParseNodeID(%q) succeeded, want error", s)
		}
	}
}

// §6.1.1: the ID is self-certifying — it must match the key it came from and
// no other.
func TestNodeIDSelfCertifying(t *testing.T) {
	a, err := GenerateNodeKey(rng.TestSecret(1))
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateNodeKey(rng.TestSecret(2))
	if err != nil {
		t.Fatal(err)
	}
	if !a.ID().Matches(a.Public) {
		t.Fatal("node ID does not match its own public key")
	}
	if a.ID().Matches(b.Public) {
		t.Fatal("node ID matched a different public key")
	}
	if a.ID() == b.ID() {
		t.Fatal("distinct keys produced the same node ID")
	}
}

func TestKeyRoundTripAndPermissions(t *testing.T) {
	dir := t.TempDir()
	// The 0600 assertion below is Unix-specific: Windows does not carry Unix
	// permission bits, and Go synthesizes a mode for them. checkKeyPerms skips
	// the check there for the same reason.
	unixOnly(t)
	key, err := GenerateNodeKey(rng.TestSecret(42))
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveNodeKey(dir, key); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(filepath.Join(dir, NodeKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file has mode %04o, want 0600", perm)
	}

	loaded, err := LoadNodeKey(dir)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID() != key.ID() {
		t.Fatalf("loaded key has ID %s, want %s", loaded.ID(), key.ID())
	}
}

// Overwriting a node key destroys the node's identity permanently (§6.1.6), so
// it must never happen by accident.
func TestSaveRefusesToOverwrite(t *testing.T) {
	dir := t.TempDir()
	key, _ := GenerateNodeKey(rng.TestSecret(7))
	if err := SaveNodeKey(dir, key); err != nil {
		t.Fatal(err)
	}
	other, _ := GenerateNodeKey(rng.TestSecret(8))
	if err := SaveNodeKey(dir, other); err == nil {
		t.Fatal("SaveNodeKey overwrote an existing key")
	}
}

func TestLoadRejectsLoosePermissions(t *testing.T) {
	unixOnly(t)
	if os.Getuid() == 0 {
		t.Skip("running as root; permission checks are not meaningful")
	}
	dir := t.TempDir()
	key, _ := GenerateNodeKey(rng.TestSecret(9))
	if err := SaveNodeKey(dir, key); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, NodeKeyFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadNodeKey(dir); err == nil {
		t.Fatal("LoadNodeKey accepted a world-readable private key")
	}
}
