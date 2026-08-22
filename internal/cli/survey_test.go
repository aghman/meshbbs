package cli

import (
	"strconv"
	"strings"
	"testing"

	"github.com/aghman/meshbbs/internal/meshlink"
)

// A load packet above the usable payload is never transmitted, while the radio
// still charges the attempt to airtime — so the survey would see its own TX
// rise, no answering rise in channel utilization, and report R = 1 with a tight
// interval. The flag has to refuse before the radio is even opened.
func TestSurveyRefusesAnUndeliverableLoadSize(t *testing.T) {
	for _, size := range []string{"0", "-1", "226", "233", "1000"} {
		t.Run(size, func(t *testing.T) {
			out, err := run(t, "mesh", "survey", "--port", "/dev/null/no-such-radio",
				"--bytes", size, "--yes")
			if err == nil {
				t.Fatalf("--bytes %s was accepted", size)
			}
			if !strings.Contains(out+err.Error(), strconv.Itoa(meshlink.MaxPayload)) {
				t.Errorf("refusal does not name the real limit %d: %v", meshlink.MaxPayload, err)
			}
		})
	}
}

// The boundary itself must be allowed, or the clamp is off by one and the
// largest deliverable packet — the one that measures R best — is unusable.
//
// Asserted by the error NOT being the clamp's: the command still fails, at the
// dial, which is the next thing it does.
//
// --port is EXPLICIT and unopenable on purpose. Without it the command
// auto-detects, and on a developer machine with a radio plugged in this test
// dials it, passes preflight and begins a real two-and-a-half-hour survey that
// TRANSMITS. That is not hypothetical: it happened here the moment preflight
// stopped refusing nodes on their declared telemetry interval, and the run was
// still inside the silent probe when the test timeout killed it. A unit test
// must not be able to reach a radio at all.
func TestSurveyAcceptsTheLargestDeliverableLoadSize(t *testing.T) {
	out, err := run(t, "mesh", "survey", "--port", "/dev/null/no-such-radio",
		"--bytes", strconv.Itoa(meshlink.MaxPayload), "--yes")
	if err == nil {
		t.Fatal("expected the dial to fail")
	}
	if strings.Contains(out+err.Error(), "--bytes must be between") {
		t.Errorf("--bytes %d was rejected, but it is exactly what the radio will send", meshlink.MaxPayload)
	}
}
