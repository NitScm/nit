package main

import (
	"strings"
	"testing"
	"time"
)

// The cutoff is where a typo becomes a deletion, so every way of getting one
// wrong is refused rather than interpreted.
func TestResolveCutoff(t *testing.T) {
	for _, tc := range []struct {
		name     string
		before   string
		keepDays int
		wantErr  string
		want     time.Time
	}{
		{
			name:   "a plain date",
			before: "2026-01-15",
			want:   time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC),
		},
		{
			name:   "an RFC3339 timestamp",
			before: "2026-01-15T13:45:00Z",
			want:   time.Date(2026, 1, 15, 13, 45, 0, 0, time.UTC),
		},
		{
			name:   "an RFC3339 timestamp with an offset is normalised to UTC",
			before: "2026-01-15T13:45:00+02:00",
			want:   time.Date(2026, 1, 15, 11, 45, 0, 0, time.UTC),
		},
		{
			name:    "neither flag",
			wantErr: "-before",
		},
		{
			name:     "both flags",
			before:   "2026-01-15",
			keepDays: 90,
			wantErr:  "not both",
		},
		{
			name:    "a date in the future",
			before:  "2099-01-01",
			wantErr: "in the future",
		},
		{
			name:    "an American date",
			before:  "01/15/2026",
			wantErr: "cannot read",
		},
		{
			name:    "a duration, which this flag does not take",
			before:  "90d",
			wantErr: "cannot read",
		},
		{
			name:    "an empty string with a zero day count",
			before:  "",
			wantErr: "-before",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveCutoff(tc.before, tc.keepDays)

			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveCutoff(%q, %d) = %s, want an error containing %q",
						tc.before, tc.keepDays, got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Errorf("error = %v, want it to contain %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("resolveCutoff(%q, %d): %v", tc.before, tc.keepDays, err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("cutoff = %s, want %s", got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
			}
			if got.Location() != time.UTC {
				t.Errorf("cutoff is in %s; the column stores UTC", got.Location())
			}
		})
	}
}

// -keep-days is the form a retention policy is written in, so it has to mean
// what a calendar means — 90 days back, not 90 × 24 hours, which drifts across
// a daylight-saving boundary.
func TestKeepDaysCountsCalendarDays(t *testing.T) {
	got, err := resolveCutoff("", 90)
	if err != nil {
		t.Fatalf("resolveCutoff: %v", err)
	}

	want := time.Now().UTC().AddDate(0, 0, -90)

	if diff := got.Sub(want); diff > time.Second || diff < -time.Second {
		t.Errorf("cutoff = %s, want about %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if !got.Before(time.Now().UTC()) {
		t.Error("a cutoff computed from -keep-days must be in the past")
	}
}
