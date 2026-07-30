package redis

import (
	"testing"

	"github.com/redis/go-redis/v9"
)

// Guard for NIM-398: the ACL username has to reach the driver in every mode.
//
// This is not cosmetic. A Redis with ACLs enabled rejects the one-argument
// `AUTH <password>` form with `-WRONGPASS`, because that form means user
// `default`. Measured against a live cluster: `AUTH <password>` → -WRONGPASS,
// `AUTH keeper <password>` → +OK. So dropping the username on any branch is
// not "logs the wrong identity", it is "cannot connect at all", and the
// symptom surfaces far away — as a red /readyz on the redis check.
//
// Each mode is asserted separately because each builds a different go-redis
// options struct, and a future edit is far more likely to add a field to one
// branch than to all three.
func TestBuild_ACLUsernameReachesDriver(t *testing.T) {
	const (
		user     = "keeper"
		pass     = "s3cret"
		sentUser = "sentinel-keeper"
		sentPass = "s3ntinel"
	)

	t.Run("standalone", func(t *testing.T) {
		c, err := build(Config{
			Mode:     ModeStandalone,
			Addr:     "redis:6379",
			Username: user,
		}, pass, "")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })

		rc, ok := c.(*redis.Client)
		if !ok {
			t.Fatalf("standalone built %T, want *redis.Client", c)
		}
		if got := rc.Options().Username; got != user {
			t.Errorf("Options().Username = %q, want %q", got, user)
		}
		if got := rc.Options().Password; got != pass {
			t.Errorf("Options().Password = %q, want %q", got, pass)
		}
	})

	t.Run("cluster", func(t *testing.T) {
		c, err := build(Config{
			Mode:     ModeCluster,
			Nodes:    []string{"n1:6379", "n2:6379"},
			Username: user,
		}, pass, "")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })

		cc, ok := c.(*redis.ClusterClient)
		if !ok {
			t.Fatalf("cluster built %T, want *redis.ClusterClient", c)
		}
		if got := cc.Options().Username; got != user {
			t.Errorf("Options().Username = %q, want %q", got, user)
		}
	})

	// Sentinel carries TWO identities and is asserted on the options struct:
	// NewFailoverClient returns a *redis.Client whose Options() shows the
	// data-node pair only, so the sentinel pair would otherwise be invisible
	// to any test.
	t.Run("sentinel carries both identities", func(t *testing.T) {
		opts := failoverOptions(Config{
			Mode:             ModeSentinel,
			MasterName:       "master",
			Sentinels:        []string{"s1:26379"},
			Username:         user,
			SentinelUsername: sentUser,
		}, pass, sentPass)

		if opts.Username != user {
			t.Errorf("Username = %q, want %q", opts.Username, user)
		}
		if opts.Password != pass {
			t.Errorf("Password = %q, want %q", opts.Password, pass)
		}
		if opts.SentinelUsername != sentUser {
			t.Errorf("SentinelUsername = %q, want %q", opts.SentinelUsername, sentUser)
		}
		if opts.SentinelPassword != sentPass {
			t.Errorf("SentinelPassword = %q, want %q", opts.SentinelPassword, sentPass)
		}
	})

	// Omitting the username must keep the pre-ACL behavior rather than
	// inventing a default: go-redis with an empty Username emits the
	// one-argument AUTH, which is exactly what a non-ACL server expects.
	t.Run("empty username stays empty", func(t *testing.T) {
		c, err := build(Config{Mode: ModeStandalone, Addr: "redis:6379"}, pass, "")
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		t.Cleanup(func() { _ = c.Close() })

		if got := c.(*redis.Client).Options().Username; got != "" {
			t.Errorf("Options().Username = %q, want empty", got)
		}
	})
}
