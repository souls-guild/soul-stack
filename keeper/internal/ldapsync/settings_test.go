package ldapsync

import (
	"errors"
	"testing"
	"time"
)

func TestParseSettings(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		want time.Duration
		ok   bool
	}{
		{`{"interval":"15m"}`, 15 * time.Minute, true},
		{`{"interval":"1d"}`, 24 * time.Hour, true},
		{`{"interval":"1m"}`, time.Minute, true},
		{`{"interval":"59s"}`, 0, false},
		{`{"interval":"25h"}`, 0, false},
		{`{"interval":""}`, 0, false},
		{`{}`, 0, false},
		{`{"interval":"15m","schedule":"*/5 * * * *"}`, 15 * time.Minute, true},
		{`{"schedule":"*/5 * * * *"}`, 0, false},
		{`{"interval":"15m"} {}`, 0, false},
		{`15m`, 0, false},
	} {
		got, err := ParseSettings(tc.raw)
		if tc.ok != (err == nil) {
			t.Errorf("ParseSettings(%s) err = %v, want ok=%v", tc.raw, err, tc.ok)
			continue
		}
		if err != nil && !errors.Is(err, ErrInvalidSettings) {
			t.Errorf("ParseSettings(%s) err = %v, want ErrInvalidSettings", tc.raw, err)
		}
		if tc.ok && got.Interval != tc.want {
			t.Errorf("ParseSettings(%s) = %s, want %s", tc.raw, got.Interval, tc.want)
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	for _, d := range []time.Duration{time.Minute, 15 * time.Minute, 90 * time.Minute, 24 * time.Hour} {
		s := Settings{Interval: d}
		got, err := ParseSettings(s.Encode())
		if err != nil || got != s {
			t.Errorf("round trip of %s via %s = %+v, %v", d, s.Encode(), got, err)
		}
	}
	if got := DefaultSettings().Encode(); got != `{"interval":"15m"}` {
		t.Errorf("default encodes as %s", got)
	}
}
