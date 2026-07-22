package push

import (
	"context"
	"errors"
	"testing"

	"github.com/souls-guild/soul-stack/shared/config"
)

func TestConfigTargetResolver_HappyPath(t *testing.T) {
	r := NewConfigTargetResolver([]config.KeeperPushTarget{
		{SID: "soul-a.example.com", SSHPort: 2222, SSHUser: "deploy", SoulPath: "/opt/soul/bin/soul"},
		{SID: "soul-b.example.com"}, // defaults
	})

	a, err := r.Resolve(context.Background(), "soul-a.example.com")
	if err != nil {
		t.Fatalf("Resolve(a): %v", err)
	}
	if a.Host != "soul-a.example.com" || a.Port != 2222 || a.User != "deploy" || a.SoulPath != "/opt/soul/bin/soul" {
		t.Errorf("a = %+v, want explicit values", a)
	}

	b, err := r.Resolve(context.Background(), "soul-b.example.com")
	if err != nil {
		t.Fatalf("Resolve(b): %v", err)
	}
	if b.Host != "soul-b.example.com" || b.Port != defaultSSHPort || b.User != defaultSSHUser || b.SoulPath != defaultSoulPath {
		t.Errorf("b = %+v, want defaults", b)
	}
}

func TestConfigTargetResolver_NotFound(t *testing.T) {
	r := NewConfigTargetResolver([]config.KeeperPushTarget{
		{SID: "known.example.com"},
	})
	_, err := r.Resolve(context.Background(), "unknown.example.com")
	if !errors.Is(err, ErrTargetNotConfigured) {
		t.Errorf("Resolve(unknown) err = %v, want ErrTargetNotConfigured", err)
	}
}

func TestConfigTargetResolver_EmptySIDSkipped(t *testing.T) {
	// Defense in depth: the schema phase rejects an empty SID, but if one
	// slips through, the constructor silently skips the entry (doesn't
	// index it), avoiding an empty "" key in the map.
	r := NewConfigTargetResolver([]config.KeeperPushTarget{
		{SID: ""},
		{SID: "valid.example.com"},
	})
	if _, err := r.Resolve(context.Background(), ""); !errors.Is(err, ErrTargetNotConfigured) {
		t.Errorf("Resolve(\"\") must return ErrTargetNotConfigured, got %v", err)
	}
	if _, err := r.Resolve(context.Background(), "valid.example.com"); err != nil {
		t.Errorf("valid.example.com: %v", err)
	}
}

func TestConfigTargetResolver_Empty(t *testing.T) {
	r := NewConfigTargetResolver(nil)
	_, err := r.Resolve(context.Background(), "any.example.com")
	if !errors.Is(err, ErrTargetNotConfigured) {
		t.Errorf("Resolve(nil-targets) err = %v, want ErrTargetNotConfigured", err)
	}
}
