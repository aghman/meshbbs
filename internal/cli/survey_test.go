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
			out, err := run(t, "mesh", "survey", "--bytes", size, "--yes")
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
// Asserted by the error NOT being the clamp's: with no radio attached the
// command still fails, at the dial, which is the next thing it does.
func TestSurveyAcceptsTheLargestDeliverableLoadSize(t *testing.T) {
	out, err := run(t, "mesh", "survey", "--bytes", strconv.Itoa(meshlink.MaxPayload), "--yes")
	if err == nil {
		return // a radio happened to be attached; the size was accepted
	}
	if strings.Contains(out+err.Error(), "--bytes must be between") {
		t.Errorf("--bytes %d was rejected, but it is exactly what the radio will send", meshlink.MaxPayload)
	}
}
