package core

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestStopMessage(t *testing.T) {
	for in, want := range map[string]string{
		"":                      "Stopped from BackTrackarr",
		"   \n\t ":              "Stopped from BackTrackarr",
		"Dinner is ready":       "Dinner is ready",
		"  Bed\n\ntime\t now  ": "Bed time now",
		"Back\x00 in\x07 5 min": "Back in 5 min",
	} {
		if got := stopMessage(in); got != want {
			t.Errorf("stopMessage(%q) = %q, want %q", in, got, want)
		}
	}
	long := stopMessage(strings.Repeat("é", MaxStopMessage+50))
	if n := utf8.RuneCountInString(long); n != MaxStopMessage || !utf8.ValidString(long) {
		t.Fatalf("long message: %d characters, valid=%v", n, utf8.ValidString(long))
	}
}
