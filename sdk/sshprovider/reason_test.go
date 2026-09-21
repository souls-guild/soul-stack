package sshprovider

import (
	"errors"
	"strings"
	"testing"
)

func TestDenyMessage(t *testing.T) {
	cases := []struct {
		reason DenyReason
		detail string
		want   string
	}{
		{DenyExplicitDeny, "root", "explicit_deny: root"},
		{DenyNotInAllowlist, "ops@web-1", "not_in_allowlist: ops@web-1"},
		{DenyPolicy, "", "policy"},
	}
	for _, c := range cases {
		if got := DenyMessage(c.reason, c.detail); got != c.want {
			t.Errorf("DenyMessage(%q,%q)=%q want %q", c.reason, c.detail, got, c.want)
		}
	}
}

func TestSignError(t *testing.T) {
	base := errors.New("open key: permission denied")
	err := SignError(SignFailReadKey, base)
	if !errors.Is(err, base) {
		t.Fatalf("SignError must wrap base error for errors.Is")
	}
	if !strings.HasPrefix(err.Error(), string(SignFailReadKey)+": ") {
		t.Errorf("SignError prefix=%q want %q-prefix", err.Error(), SignFailReadKey)
	}
}
