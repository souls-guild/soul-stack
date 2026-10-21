package soulforget

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestOwnedBy(t *testing.T) {
	cases := []struct {
		name    string
		ties    Ties
		ok      bool
		mention string
	}{
		{"member of this incarnation only", Ties{MemberOf: []string{"inc-one"}}, true, ""},
		{"with a Voice in its own Choir", Ties{MemberOf: []string{"inc-one"}, VoiceIn: []string{"inc-one"}}, true, ""},
		{"member of no incarnation", Ties{}, false, "member of no incarnation"},
		{"member of another incarnation", Ties{MemberOf: []string{"inc-two"}}, false, "member of inc-two, not of inc-one"},
		{"shared", Ties{MemberOf: []string{"inc-one", "inc-two"}}, false, "also a member of inc-two"},
		// A host unbound from inc-two that still holds a Voice in its Choir: the
		// cascade off `souls` would delete that Voice.
		{"Voice in another incarnation's Choir", Ties{MemberOf: []string{"inc-one"}, VoiceIn: []string{"inc-one", "inc-two"}}, false, "holds a Choir Voice in inc-two"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := OwnedBy("host1.example.com", "inc-one", c.ties)
			if c.ok {
				if err != nil {
					t.Fatalf("OwnedBy = %v, want nil", err)
				}
				return
			}
			if !errors.Is(err, ErrNotOwned) {
				t.Fatalf("OwnedBy = %v, want ErrNotOwned", err)
			}
			if !strings.Contains(err.Error(), c.mention) {
				t.Errorf("message %q does not mention %q", err.Error(), c.mention)
			}
		})
	}
}

// TestEraseOwned_RefusesEmptyIncarnation — an empty incarnation would make the
// guard compare against "", which no membership row carries; it is refused before
// the cluster is told anything.
func TestEraseOwned_RefusesEmptyIncarnation(t *testing.T) {
	td := &scriptedTeardown{}
	if _, err := EraseOwned(context.Background(), &refusingPool{t: t}, td, "host1.example.com", "", "reason"); err == nil {
		t.Fatal("EraseOwned accepted an empty incarnation")
	}
	if td.broadcastCalls != 0 {
		t.Errorf("teardown notice sent for a refused call: %d", td.broadcastCalls)
	}
}
