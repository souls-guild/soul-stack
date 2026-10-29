package maintenance

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestWindowSpecNormalize(t *testing.T) {
	ok := WindowSpec{Days: []string{"thu", "mon"}, StartTime: "02:00", DurationMinutes: 180, Timezone: "Europe/Berlin"}
	got, err := ok.Normalize()
	if err != nil {
		t.Fatalf("Normalize(valid) = %v", err)
	}
	if !reflect.DeepEqual(got.Days, []string{"mon", "thu"}) {
		t.Errorf("Days = %v, want calendar order [mon thu]", got.Days)
	}

	cases := []struct {
		name  string
		edit  func(*WindowSpec)
		field string
	}{
		{"no days", func(s *WindowSpec) { s.Days = nil }, "days"},
		{"unknown day", func(s *WindowSpec) { s.Days = []string{"monday"} }, "days"},
		{"a day twice", func(s *WindowSpec) { s.Days = []string{"mon", "mon"} }, "days"},
		{"hour 24", func(s *WindowSpec) { s.StartTime = "24:00" }, "start_time"},
		{"one-digit hour", func(s *WindowSpec) { s.StartTime = "2:00" }, "start_time"},
		{"seconds", func(s *WindowSpec) { s.StartTime = "02:00:00" }, "start_time"},
		{"zero duration", func(s *WindowSpec) { s.DurationMinutes = 0 }, "duration_minutes"},
		{"over a day", func(s *WindowSpec) { s.DurationMinutes = MaxDurationMinutes + 1 }, "duration_minutes"},
		{"no zone", func(s *WindowSpec) { s.Timezone = "" }, "timezone"},
		{"Local", func(s *WindowSpec) { s.Timezone = "Local" }, "timezone"},
		{"host localtime", func(s *WindowSpec) { s.Timezone = "localtime" }, "timezone"},
		{"host posixrules", func(s *WindowSpec) { s.Timezone = "posixrules" }, "timezone"},
		{"host posix tree", func(s *WindowSpec) { s.Timezone = "posix/Europe/Berlin" }, "timezone"},
		{"NUL in comment", func(s *WindowSpec) { s.Comment = "a\x00b" }, "comment"},
		{"unknown zone", func(s *WindowSpec) { s.Timezone = "Mars/Olympus" }, "timezone"},
		{"long comment", func(s *WindowSpec) { s.Comment = strings.Repeat("é", MaxCommentLength+1) }, "comment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := ok
			s.Days = append([]string(nil), ok.Days...)
			tc.edit(&s)
			_, err := s.Normalize()
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.field {
				t.Errorf("Normalize = %v, want a ValidationError on %q", err, tc.field)
			}
		})
	}

	edge := ok
	edge.DurationMinutes = MaxDurationMinutes
	edge.Comment = strings.Repeat("é", MaxCommentLength)
	edge.Timezone = "UTC"
	if _, err := edge.Normalize(); err != nil {
		t.Errorf("Normalize at the bounds = %v, want nil", err)
	}
}

func TestFreezeSpecValidate(t *testing.T) {
	start := time.Date(2026, 12, 10, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		spec  FreezeSpec
		field string
	}{
		{"valid", FreezeSpec{StartsAt: start, EndsAt: start.Add(time.Hour)}, ""},
		{"no start", FreezeSpec{EndsAt: start}, "starts_at"},
		{"no end", FreezeSpec{StartsAt: start}, "ends_at"},
		{"empty interval", FreezeSpec{StartsAt: start, EndsAt: start}, "ends_at"},
		{"reversed", FreezeSpec{StartsAt: start, EndsAt: start.Add(-time.Minute)}, "ends_at"},
		{"empty at the stored microsecond", FreezeSpec{StartsAt: start.Add(100 * time.Nanosecond), EndsAt: start.Add(900 * time.Nanosecond)}, "ends_at"},
		{"long comment", FreezeSpec{StartsAt: start, EndsAt: start.Add(time.Hour), Comment: strings.Repeat("x", MaxCommentLength+1)}, "comment"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate()
			if tc.field == "" {
				if err != nil {
					t.Errorf("Validate = %v, want nil", err)
				}
				return
			}
			var ve *ValidationError
			if !errors.As(err, &ve) || ve.Field != tc.field {
				t.Errorf("Validate = %v, want a ValidationError on %q", err, tc.field)
			}
		})
	}
}

func TestValidID(t *testing.T) {
	if !ValidID("0b9f7c1e-3d2a-4c5b-9e8f-1a2b3c4d5e6f") {
		t.Error("a lower-case UUID must be valid")
	}
	for _, bad := range []string{"", "1", "0B9F7C1E-3D2A-4C5B-9E8F-1A2B3C4D5E6F", "0b9f7c1e3d2a4c5b9e8f1a2b3c4d5e6f", "../etc"} {
		if ValidID(bad) {
			t.Errorf("ValidID(%q) = true", bad)
		}
	}
}
