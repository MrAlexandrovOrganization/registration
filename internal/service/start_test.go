package service

import (
	"strings"
	"testing"
)

func TestStartSource(t *testing.T) {
	for _, tc := range []struct {
		text, source string
		start        bool
	}{
		{"/start", "", true},
		{"/start channel-A_1", "channel-A_1", true},
		{"/start@fixture_bot website", "website", true},
		{"/start\nqr", "qr", true},
		{"/start " + strings.Repeat("a", 64), strings.Repeat("a", 64), true},
		{"/start " + strings.Repeat("a", 65), "", true},
		{"/start a b", "", true},
		{"/start <tag>", "", true},
		{"/start метка", "", true},
		{"/start :unknown", "", true},
		{"/starting tag", "", false},
		{"/start@", "", false},
		{"answer /start tag", "", false},
		{"", "", false},
	} {
		t.Run(tc.text, func(t *testing.T) {
			source, start := startSource(tc.text)
			if source != tc.source || start != tc.start {
				t.Fatalf("got (%q, %v), want (%q, %v)", source, start, tc.source, tc.start)
			}
		})
	}
}
