// Package meshtastic speaks to a locally attached Meshtastic node over serial
// or TCP (design §7.1).
//
// # Why this is ours and not a dependency
//
// `[D3]` decides to vendor the official `meshtastic/protobufs` and write the
// transport ourselves, because none of the Go libraries in the ecosystem is a
// comfortable dependency and the thing they wrap is genuinely small: a 4-byte
// header, a length, and a protobuf. The generated bindings live in ./meshpb and
// are produced by scripts/genproto.sh from the pinned submodule; everything
// else here is a few hundred lines.
//
// # What this package is not
//
// It is not a link.Link. Nothing here knows about node IDs, bundles, fountain
// symbols or the airtime governor — this layer moves ToRadio and FromRadio
// messages to and from the radio attached to this machine, and that is all.
// The BSMP link that sits on top of it, and the governor that decides whether a
// send is allowed at all, come next in Phase 3.
package meshtastic

import (
	"bufio"
	"errors"
	"fmt"
	"io"
)

const (
	// start1 and start2 open every frame on the wire. The values are firmware
	// constants, not something we chose, and they are identical over serial and
	// TCP — which is the whole reason one Conn type serves both transports.
	start1 = 0x94
	start2 = 0xC3

	// headerLen is start1, start2 and a 16-bit big-endian length.
	headerLen = 4

	// MaxFrame is the largest payload the firmware will send or accept in one
	// frame (MAX_TO_FROM_RADIO_SIZE). It bounds the reader's allocation, so it
	// must stay a constant and not become "whatever the length field said".
	//
	// Note it is NOT the mesh MTU: this is the local wire to our own radio, and
	// a single FromRadio carrying a NodeInfo or a Config is comfortably larger
	// than anything that could cross the air.
	MaxFrame = 512

	// MTU is the usable application payload in one Meshtastic packet, from
	// `Data.payload max_size:233` in the vendored mesh.options — the number §1
	// says shapes every other decision in the design.
	MTU = 233
)

// ErrFrameTooLarge is returned when a payload will not fit one frame.
var ErrFrameTooLarge = errors.New("meshtastic: frame exceeds MaxFrame")

// AppendFrame appends a framed payload to dst and returns the extended slice.
func AppendFrame(dst, payload []byte) ([]byte, error) {
	if len(payload) > MaxFrame {
		return dst, fmt.Errorf("%w: %d > %d", ErrFrameTooLarge, len(payload), MaxFrame)
	}
	dst = append(dst, start1, start2, byte(len(payload)>>8), byte(len(payload)))
	return append(dst, payload...), nil
}

// FrameReader extracts frames from a byte stream that also carries other things.
//
// # Why this is a resynchronising parser rather than a struct read
//
// A serial line from a Meshtastic node is not a clean frame stream. The
// firmware writes human-readable debug output to the same UART, so plain text
// arrives interleaved between frames; a device that was mid-frame when we
// attached hands us a fragment; and reconnecting to a sleeping node means
// sending deliberate garbage to wake it (see Conn.Wake). A reader that assumed
// the next byte was always a header would desynchronise on the first log line
// and never recover.
//
// So the rule is: anything that is not a well-formed header is skipped one byte
// at a time until one is found. Skipped bytes are counted, and offered to
// OnDeviceLog as text, because a serial port producing nothing but skipped
// bytes is the signature of a wrong baud rate — a diagnostic worth having
// rather than a silent hang.
type FrameReader struct {
	br      *bufio.Reader
	skipped uint64

	onText func(string)
	line   []byte
}

// maxLogLine caps assembled device log lines. Device output is unframed, so a
// node emitting binary garbage would otherwise grow this buffer without bound.
const maxLogLine = 512

// NewFrameReader wraps r. onText, if non-nil, receives device output found
// between frames, one line at a time and sanitised for terminal display.
func NewFrameReader(r io.Reader, onText func(string)) *FrameReader {
	// The buffer must hold a whole frame plus its header so that Peek can look
	// at a complete header without ever needing to grow.
	return &FrameReader{br: bufio.NewReaderSize(r, MaxFrame+headerLen), onText: onText}
}

// ReadFrame returns the next frame payload, skipping anything that is not one.
//
// The returned slice is freshly allocated and owned by the caller.
func (fr *FrameReader) ReadFrame() ([]byte, error) {
	for {
		hdr, err := fr.br.Peek(headerLen)
		if err != nil {
			// A partial header at end of stream is not a frame. Consume what is
			// there as skipped bytes so the accounting stays honest, then report
			// the underlying error.
			if len(hdr) > 0 && (hdr[0] != start1 || (len(hdr) > 1 && hdr[1] != start2)) {
				fr.skipByte()
				continue
			}
			if len(hdr) > 0 && errors.Is(err, io.EOF) {
				for range hdr {
					fr.skipByte()
				}
				fr.flushLine()
				return nil, io.ErrUnexpectedEOF
			}
			fr.flushLine()
			return nil, err
		}

		// Skip ONE byte on a mismatch, not the whole candidate header.
		//
		// The reference implementation in meshtastic-python drops both bytes
		// when the second is not start2, which loses a frame whose header is
		// preceded by a stray 0x94 — `94 94 C3 ...` is exactly what a truncated
		// previous frame can leave behind. Advancing a single byte re-examines
		// that 0x94 as a candidate start1 and recovers the frame.
		if hdr[0] != start1 || hdr[1] != start2 {
			fr.skipByte()
			continue
		}

		n := int(hdr[2])<<8 | int(hdr[3])
		if n > MaxFrame {
			// Either noise that happened to look like a header, or a node
			// speaking a protocol we do not. Resync from the next byte.
			fr.skipByte()
			continue
		}

		if _, err := fr.br.Discard(headerLen); err != nil {
			return nil, err
		}
		fr.flushLine()
		buf := make([]byte, n)
		if _, err := io.ReadFull(fr.br, buf); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, io.ErrUnexpectedEOF
			}
			return nil, err
		}
		return buf, nil
	}
}

// Skipped counts bytes discarded outside frames since the reader was created.
//
// Some skipping is normal (device log output). A count that climbs while no
// frames arrive means the stream is not a Meshtastic stream: wrong baud rate,
// wrong port, or a device in bootloader mode.
func (fr *FrameReader) Skipped() uint64 { return fr.skipped }

// skipByte consumes one byte outside a frame and routes it to the log sink.
func (fr *FrameReader) skipByte() {
	b, err := fr.br.ReadByte()
	if err != nil {
		return
	}
	fr.skipped++
	if fr.onText == nil {
		return
	}
	switch {
	case b == '\n':
		fr.flushLine()
	case b == '\r':
		// Ignore: the firmware emits CRLF and a bare CR would otherwise flush
		// an empty line for every real one.
	default:
		if len(fr.line) < maxLogLine {
			fr.line = append(fr.line, sanitise(b))
		}
	}
}

func (fr *FrameReader) flushLine() {
	if fr.onText == nil || len(fr.line) == 0 {
		return
	}
	fr.onText(string(fr.line))
	fr.line = fr.line[:0]
}

// sanitise maps control bytes to '.' because device output is written straight
// to a sysop's terminal by `meshbbs mesh info`. The bytes come from firmware we
// did not write, over a wire that also carries noise, so treating them as
// display-safe would make an escape sequence out of a glitch.
func sanitise(b byte) byte {
	if b < 0x20 || b == 0x7f {
		return '.'
	}
	return b
}

// sanitiseText is sanitise for strings that arrive inside protobuf fields —
// firmware versions, channel names, log records. Same reasoning: these are set
// by whoever configured the radio, not by us, and they end up on a terminal.
//
// Multi-byte UTF-8 passes through untouched: its continuation bytes are all
// >= 0x80, so a channel name in Cyrillic or Japanese survives, while an
// embedded escape sequence does not.
func sanitiseText(s string) string {
	needs := false
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] == 0x7f {
			needs = true
			break
		}
	}
	if !needs {
		return s
	}
	b := []byte(s)
	for i := range b {
		b[i] = sanitise(b[i])
	}
	return string(b)
}
