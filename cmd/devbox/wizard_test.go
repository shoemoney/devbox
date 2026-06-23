package main

import (
	"strings"
	"testing"
)

// TestSetupOptOut covers the gate + the "don't ask again" persistence: a fresh
// machine is offered the wizard, answering "n" records the opt-out, and after
// that the wizard is never auto-offered again.
func TestSetupOptOut(t *testing.T) {
	dir := t.TempDir()

	if !shouldOfferSetup(dir) {
		t.Fatal("a fresh (unjoined) machine should be offered the wizard")
	}

	var out strings.Builder
	ran, err := offerSetup(strings.NewReader("n\n"), &out, dir)
	if err != nil {
		t.Fatalf("offerSetup: %v", err)
	}
	if ran {
		t.Fatal(`answering "n" must NOT run the wizard`)
	}
	if shouldOfferSetup(dir) {
		t.Fatal("after opt-out, the wizard must never be auto-offered again")
	}
}
