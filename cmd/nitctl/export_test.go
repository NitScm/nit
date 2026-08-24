package main

import (
	"testing"
	"time"
)

// The window's end is the safety property. Ids become visible at commit, so a
// window that runs up to "now" can step past a transaction that started earlier
// and has not committed — an export that looks complete and is not, which is
// the failure an audit trail exists to not have.
func TestAWindowThatHasNotSettledIsRefused(t *testing.T) {
	settle := time.Minute

	for _, until := range []string{
		time.Now().UTC().Format(time.RFC3339),
		time.Now().UTC().Add(-30 * time.Second).Format(time.RFC3339),
		time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
	} {
		if _, err := resolveWindow("24h", until, settle); err == nil {
			t.Errorf("-until %s was accepted, though a transaction may still be in flight", until)
		}
	}
}

func TestASettledWindowIsAccepted(t *testing.T) {
	until := time.Now().UTC().Add(-5 * time.Minute).Format(time.RFC3339)

	window, err := resolveWindow("24h", until, time.Minute)
	if err != nil {
		t.Fatalf("resolveWindow: %v", err)
	}

	if window.until.After(time.Now().UTC().Add(-time.Minute)) {
		t.Errorf("until = %s, which is inside the settle period", window.until)
	}

	if !window.since.Before(window.until) {
		t.Errorf("since = %s is not before until = %s", window.since, window.until)
	}
}

// Omitting -until must not mean "up to now": the default has to be inside the
// settled part of the trail, or the safe default is the unsafe one.
func TestTheDefaultEndIsAlreadySettled(t *testing.T) {
	const settle = 90 * time.Second

	window, err := resolveWindow("24h", "", settle)
	if err != nil {
		t.Fatalf("resolveWindow: %v", err)
	}

	if window.until.After(time.Now().UTC().Add(-settle + time.Second)) {
		t.Errorf("the default end is %s, which is not %s in the past", window.until, settle)
	}
}

func TestAWindowThatEndsBeforeItStartsIsRefused(t *testing.T) {
	since := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	until := time.Now().UTC().Add(-2 * time.Hour).Format(time.RFC3339)

	if _, err := resolveWindow(since, until, time.Minute); err == nil {
		t.Error("a window ending before it starts was accepted")
	}
}

func TestParseWhenReadsTheThreeFormsAnOperatorTypes(t *testing.T) {
	now := time.Now().UTC()

	cases := []struct {
		in   string
		want func(time.Time) bool
	}{
		{"2026-01-15", func(got time.Time) bool {
			return got.Year() == 2026 && got.Month() == time.January && got.Day() == 15
		}},
		{"2026-01-15T09:26:53Z", func(got time.Time) bool {
			return got.Hour() == 9 && got.Minute() == 26 && got.Second() == 53
		}},
		{"24h", func(got time.Time) bool {
			delta := now.Sub(got)
			return delta > 23*time.Hour && delta < 25*time.Hour
		}},
		// A duration written as a negative still means "back from now". An
		// operator who typed -since=-24h meant the same window.
		{"-24h", func(got time.Time) bool {
			delta := now.Sub(got)
			return delta > 23*time.Hour && delta < 25*time.Hour
		}},
	}

	for _, c := range cases {
		got, err := parseWhen(c.in)
		if err != nil {
			t.Errorf("parseWhen(%q): %v", c.in, err)
			continue
		}

		if !c.want(got) {
			t.Errorf("parseWhen(%q) = %s, which is not what that means", c.in, got)
		}
	}

	if _, err := parseWhen("last tuesday"); err == nil {
		t.Error("parseWhen accepted something it cannot have understood")
	}
}
