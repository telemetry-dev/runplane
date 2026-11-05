package api

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestCloseReasonKeepsWholeCharacters(t *testing.T) {
	// Byte 120 falls inside the 60th "é", so a cut there leaves half of it.
	msg := "a" + strings.Repeat("é", 100)
	got := closeReason(msg)
	if want := "a" + strings.Repeat("é", 59) + "..."; got != want {
		t.Errorf("closeReason = %q, want %q", got, want)
	}
	if !utf8.ValidString(got) || len(got) > 123 {
		t.Errorf("closeReason = %q (%d bytes), want valid UTF-8 of at most 123 bytes", got, len(got))
	}
}
