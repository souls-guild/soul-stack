package audit

import "testing"

func TestSource_RequiresArchonAID(t *testing.T) {
	cases := []struct {
		s    Source
		want bool
	}{
		{SourceAPI, true},
		{SourceMCP, true},
		{SourceSignal, false},
		{SourceKeeperInternal, false},
		{SourceSoulGRPC, false},
		{SourceBackground, false},
		{SourceConfigBootstrap, false},
	}
	for _, c := range cases {
		t.Run(string(c.s), func(t *testing.T) {
			if got := c.s.RequiresArchonAID(); got != c.want {
				t.Errorf("Source(%q).RequiresArchonAID() = %v, want %v", c.s, got, c.want)
			}
		})
	}
}
