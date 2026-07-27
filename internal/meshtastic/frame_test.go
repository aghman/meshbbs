package meshtastic

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// frame builds a well-formed frame around payload.
func frame(t *testing.T, payload []byte) []byte {
	t.Helper()
	b, err := AppendFrame(nil, payload)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestFrameRoundTrip(t *testing.T) {
	payloads := [][]byte{
		{},
		{0x00},
		[]byte("hello"),
		bytes.Repeat([]byte{0xff}, MaxFrame),
		// A payload that contains the header bytes, which must not confuse the
		// reader: framing is length-delimited, not delimiter-scanned.
		{start1, start2, 0x00, 0x05, start1, start2},
	}

	var stream []byte
	for _, p := range payloads {
		stream = append(stream, frame(t, p)...)
	}

	fr := NewFrameReader(bytes.NewReader(stream), nil)
	for i, want := range payloads {
		got, err := fr.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("frame %d = %x, want %x", i, got, want)
		}
	}
	if _, err := fr.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Errorf("after last frame: err = %v, want EOF", err)
	}
	if fr.Skipped() != 0 {
		t.Errorf("skipped %d bytes of a clean stream, want 0", fr.Skipped())
	}
}

func TestAppendFrameRejectsOversize(t *testing.T) {
	if _, err := AppendFrame(nil, make([]byte, MaxFrame+1)); !errors.Is(err, ErrFrameTooLarge) {
		t.Errorf("err = %v, want ErrFrameTooLarge", err)
	}
}

// AppendFrame must extend the caller's buffer rather than replace it, since
// Conn reuses one across sends.
func TestAppendFramePreservesPrefix(t *testing.T) {
	out, err := AppendFrame([]byte("prefix"), []byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if string(out[:6]) != "prefix" {
		t.Fatalf("prefix clobbered: %q", out)
	}
}

// The reason FrameReader is a resynchronising parser at all: a serial line
// carries the firmware's debug output interleaved with frames.
func TestReaderSkipsDeviceLogOutput(t *testing.T) {
	var stream []byte
	stream = append(stream, []byte("INFO | Booted\r\n")...)
	stream = append(stream, frame(t, []byte("first"))...)
	stream = append(stream, []byte("DEBUG | Sending packet\r\n")...)
	stream = append(stream, frame(t, []byte("second"))...)

	var logs []string
	fr := NewFrameReader(bytes.NewReader(stream), func(s string) { logs = append(logs, s) })

	for _, want := range []string{"first", "second"} {
		got, err := fr.ReadFrame()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Errorf("frame = %q, want %q", got, want)
		}
	}
	if want := []string{"INFO | Booted", "DEBUG | Sending packet"}; !equalStrings(logs, want) {
		t.Errorf("device log = %q, want %q", logs, want)
	}
	if fr.Skipped() == 0 {
		t.Error("skipped counter did not move despite 38 bytes of log output")
	}
}

// A stray start1 immediately before a real header is what a truncated frame
// leaves behind. The reference implementation in meshtastic-python drops both
// bytes on a start2 mismatch and loses the frame; we advance one byte and
// recover it.
func TestReaderRecoversFromStrayStart1(t *testing.T) {
	stream := append([]byte{start1, start1}, frame(t, []byte("payload"))...)
	fr := NewFrameReader(bytes.NewReader(stream), nil)
	got, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("frame lost after a stray start byte: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("frame = %q, want %q", got, "payload")
	}
}

// A length field beyond MaxFrame is noise that happened to look like a header.
// It must not allocate, and must not eat the real frame that follows.
func TestReaderResyncsPastOversizeLength(t *testing.T) {
	stream := []byte{start1, start2, 0xff, 0xff}
	stream = append(stream, frame(t, []byte("real"))...)

	fr := NewFrameReader(bytes.NewReader(stream), nil)
	got, err := fr.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if string(got) != "real" {
		t.Errorf("frame = %q, want %q", got, "real")
	}
}

func TestReaderTruncatedFrames(t *testing.T) {
	full := frame(t, []byte("abcdefgh"))
	for _, cut := range []int{1, 2, 3, 4, 6, len(full) - 1} {
		fr := NewFrameReader(bytes.NewReader(full[:cut]), nil)
		_, err := fr.ReadFrame()
		if !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Errorf("truncated at %d bytes: err = %v, want ErrUnexpectedEOF", cut, err)
		}
	}
}

// Device log output that never ends in a newline must still reach the sink when
// the next frame arrives; otherwise the most interesting line — the one the
// firmware printed just before it stopped talking — is the one that is lost.
func TestPartialLogLineFlushesBeforeFrame(t *testing.T) {
	stream := append([]byte("no newline here"), frame(t, []byte("f"))...)
	var logs []string
	fr := NewFrameReader(bytes.NewReader(stream), func(s string) { logs = append(logs, s) })
	if _, err := fr.ReadFrame(); err != nil {
		t.Fatal(err)
	}
	if !equalStrings(logs, []string{"no newline here"}) {
		t.Errorf("device log = %q", logs)
	}
}

func TestLogLineIsSanitisedAndBounded(t *testing.T) {
	var logs []string
	// An escape sequence in device output would otherwise be interpreted by the
	// sysop's terminal.
	stream := append([]byte("\x1b[2Jcleared\n"), bytes.Repeat([]byte{'x'}, maxLogLine+50)...)
	stream = append(stream, '\n')
	fr := NewFrameReader(bytes.NewReader(stream), func(s string) { logs = append(logs, s) })
	_, _ = fr.ReadFrame()

	if len(logs) != 2 {
		t.Fatalf("logs = %q, want 2 lines", logs)
	}
	if strings.ContainsRune(logs[0], 0x1b) {
		t.Errorf("escape byte survived sanitising: %q", logs[0])
	}
	if len(logs[1]) != maxLogLine {
		t.Errorf("line length = %d, want it capped at %d", len(logs[1]), maxLogLine)
	}
}

func TestSanitiseText(t *testing.T) {
	cases := map[string]string{
		"2.7.4.abcdef":   "2.7.4.abcdef",
		"bbs\x1b[31mnet": "bbs.[31mnet",
		"tab\there":      "tab.here",
		"日本語":            "日本語",
	}
	for in, want := range cases {
		if got := sanitiseText(in); got != want {
			t.Errorf("sanitiseText(%q) = %q, want %q", in, got, want)
		}
	}
}

// Nothing a radio (or a noisy wire) can produce may panic the reader or make it
// allocate on the length field's say-so.
func FuzzReadFrame(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{start1, start2, 0x00, 0x00})
	f.Add(frame(&testing.T{}, []byte("hello")))
	f.Add([]byte{start1, start2, 0xff, 0xff, start1, start2, 0x00, 0x01, 0x41})
	f.Add([]byte("INFO | boot\n\x94\xc3\x00\x02ab"))

	f.Fuzz(func(t *testing.T, data []byte) {
		fr := NewFrameReader(bytes.NewReader(data), func(string) {})
		consumed := 0
		for {
			payload, err := fr.ReadFrame()
			if err != nil {
				break
			}
			if len(payload) > MaxFrame {
				t.Fatalf("frame of %d bytes exceeds MaxFrame", len(payload))
			}
			consumed += headerLen + len(payload)
		}
		// Every byte of the input was either part of a frame or counted as
		// skipped. An accounting gap would mean the reader silently swallowed
		// input — the failure mode that makes a desynchronised stream look like
		// a quiet radio.
		if total := consumed + int(fr.Skipped()); total > len(data) {
			t.Fatalf("accounted for %d bytes of a %d byte input", total, len(data))
		}
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
