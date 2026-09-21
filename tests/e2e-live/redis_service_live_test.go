//go:build e2e_live

// L3b: the blocking gate's SERVICE subject — a published service brought to a working
// state, and then operated (NIM-876).
//
// WHAT THESE TWO REPLACE. NIM-871 cut `examples/service/redis` out of the engine, and with
// it the six gate tests that created a live Redis and ran a day-2 scenario against it. The
// gate went from nine tests to three, and the three that remained prove a MODULE is
// delivered — fetched, Sigil-verified, hot-registered — which is a different claim from a
// SERVICE being operated. Between the two claims sits everything a service is: a
// state_schema with declared secrets, a vars ladder, a destiny brick, a plugin addressed
// per object, a run whose passages are ordered by register edges. A green gate has to mean
// that works.
//
// WHY THE SUBJECT IS OUT OF TREE. A service is its own repository — bundling one is what
// made the engine's gate depend on a service's lifecycle in the first place. So the subject
// is `github.com/soul-stack-services/redis` at a commit pinned in
// harness/servicecatalog.go, fetched once into a cache and extracted to a path that carries
// the commit. Neither half of that is decoration: without the pin the gate proves whatever
// is in some directory, and without the cache its verdict depends on github.com being up.
//
// ★ WHAT THESE TESTS DO NOT PROVE, said here because a green run must not be read as more
// than it is: the service's own `create` scenario raises MACHINES — libvirt VMs through a
// keeper-side provider, an SSH host CA, cloud-init, a token redeemed over `core.ssh.run`.
// None of that is reachable from a docker tier whose souls are already onboarded, so these
// tests drive `create_from_souls`, which is the same rollout onto a roster that already
// exists. The machine half is proven by a live run on a workstation stand (the service's
// README records what that costs) and by nothing automated. That is a real hole and it is
// smaller than the one it replaces: what left with NIM-871 was the rollout AND the day-2
// operation, and both are back.
//
// ⚠ The run needs egress: the service installs Redis from packages.redis.io, as the six
// tests it replaces did. The tier has never been hermetic — the nginx smoke pulls from
// Debian's own mirror — and the one class of network dependency that was measured making
// this gate's verdict random (release tarballs through `core.url`, NIM-542/NIM-406) is
// held out by a guard in harness/upstreamfetch.go.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

const (
	// redisServiceAlias — the catalogued pin AND the id the service is registered under.
	// One name: the stand's registry id is what a scenario's secret paths are derived
	// from, so a second spelling would move every path.
	redisServiceAlias = "redis"

	// redisServiceInc — the incarnation. Kebab and short: it is segment 3 of every
	// derived Vault path.
	redisServiceInc = "redis-live"

	// redisAdminUser — the service account the service creates for itself and connects
	// as. Declared in its vars/30-acl-users.yaml; asserted here because the ACL and PING
	// steps authenticate as it, so a rename in the service turns into an auth failure
	// three steps from the cause.
	redisAdminUser = "default_admin"

	// redisOperatorUser — the operator account `create_from_souls` is given.
	redisOperatorUser = "app"

	// redisDay2User — the account add_user adds. Deliberately NOT in the create input:
	// the day-2 run is what has to bring it into existence, in state, in users.acl and on
	// the live instance.
	redisDay2User = "reporter"

	// redisVersion — from the service's closed input enum, all of which the apt
	// repository it names publishes.
	redisVersion = "8.10.1"
)

// redisServiceStack brings up the stand the two tests below share: one soul container, the
// redis plugin delivered through the regular channel, the destiny bricks materialized, and
// the pinned service registered.
//
// Not a shared Stack — a shared SHAPE. Each test gets its own stand: these are the two
// halves of a service's life and a day-2 test that inherited a create's containers would
// pass or fail partly on what the other test left behind.
func redisServiceStack(t *testing.T) *harness.Stack {
	t.Helper()

	// Before NewStack: the source URL goes into keeper.yml's plugin catalog.
	pluginRepo := harness.BuildRedisPlugin(t)

	stack := harness.NewStack(t, harness.Config{
		ServiceRepo: redisServiceAlias,
		ServiceName: redisServiceAlias,
		Souls:       1,
		SoulModules: []harness.SoulModuleEntry{
			{Name: harness.RedisAlias, Source: pluginRepo, Ref: harness.RedisPluginRef},
		},
	})
	t.Cleanup(stack.Cleanup)

	// ref v1.0.0 — the pin in the service's own `destiny[]`. A floating ref here would
	// mean the gate and the service disagree about which brick is under test.
	stack.MaterializeDestinies(t, "v1.0.0", "redis")
	// Ref-pinned allow is mandatory (ADR-065): auto-deps synthesise the install with
	// ref=v1.0.0, and an allow on `main` would not cover it.
	stack.AllowSoulModule(t, harness.RedisAlias, pluginRepo, harness.RedisPluginRef)

	// The render reads `soulprint.hosts` (the service's roster assert) and the destiny's
	// redis.conf binds `soulprint.self.network.primary_ip`. POST /v1/incarnations binds
	// the roster and starts the run in one call, so the wait has to happen BEFORE it —
	// otherwise the first facts arrive mid-render and the roster assert answers about an
	// empty composition.
	stack.WaitSoulprintReported(t, 0, 60)
	return stack
}

// createFromSoulsInput — the request. `hosts` is the roster-declaring field
// (`source: { roster: true }`, ADR-0081): keeper binds those SIDs into
// incarnation_membership after inserting the row and before starting the run.
func createFromSoulsInput(sid string) map[string]any {
	return map[string]any{
		"owners":           []any{"team-cache"},
		"environment":      "dev",
		"version":          redisVersion,
		"hosts":            []any{sid},
		"persistence":      "rdb",
		"maxmemory_policy": "volatile-lru",
		"memory_mb":        1024,
		"users": []any{
			map[string]any{
				"name":  redisOperatorUser,
				"perms": "~app:* +@read +@write -@dangerous",
				"state": "on",
			},
		},
	}
}

// TestL3bRedisServiceLive_CreateFromSouls — a published service is brought to a working
// state: request → running Redis → the credential in Vault is the one it answers to.
//
// The claim is deliberately not "the run went green". A create that installed nothing
// reaches success if every step is a no-op, and a create that installed everything but got
// the ACL wrong reaches success too. So the assertions land on the instance: the package,
// the unit, the rendered conf, a directive that only exists because the scenario computed
// it, and an AUTH as the account the service minted for itself with a password nobody
// passed in.
func TestL3bRedisServiceLive_CreateFromSouls(t *testing.T) {
	stack := redisServiceStack(t)

	sid := stack.SoulContainers[0].SID
	inc, applyID := stack.CreateIncarnationWithApplyScenario(t, redisServiceInc,
		redisServiceAlias+"@main", "create_from_souls", createFromSoulsInput(sid))

	// 600s: apt-get update against packages.redis.io, install redis-server, render
	// redis.conf and users.acl, systemd start, then the PING gate's bounded retry.
	stack.WaitApplySuccess(t, applyID, 600)
	// apply_runs success is a per-host terminal; the state captures that stand after the
	// host work commit later ([ADR-0084]). Reading state before ready is the NIM-45 race.
	stack.WaitIncarnationReady(t, inc, 300)

	// (a) The incarnation records what it was asked for and what it found. `machine_count`
	// is the size of the roster the request carried — this scenario has no such input, it
	// counts `hosts` — and `operational_status: running` is a claim about a FACT: its write
	// refers to the PING register, which is what puts it in a passage after the probe.
	// Together they are the difference between "the run finished" and "a Redis answered".
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_version":      redisVersion,
		"operational_status": "running",
		"machine_count":      1,
		"persistence":        "rdb",
		"environment":        "dev",
	})

	// (b) On the box: the package, the unit, and the conf carrying a directive the scenario
	// COMPUTED rather than copied. maxmemory is memory_mb × vars.memory_reserve_percent
	// (1024 × 75% = 768mb), so this line fails if the vars ladder, the compute or the
	// destiny's config merge drops out — each of which is otherwise invisible in a green run.
	stack.AssertHostPkgInstalled(t, 0, "redis-server")
	stack.AssertHostServiceActive(t, 0, "redis-server")
	stack.AssertRedisConfFileDirective(t, 0, "/etc/redis/redis.conf", "maxmemory", "768mb")
	stack.AssertRedisConfFileDirective(t, 0, "/etc/redis/redis.conf", "maxmemory-policy", "volatile-lru")

	// (c) ★ THE MINTED CREDENTIAL IS THE LIVE ONE. The password is in no input and in no
	// fixture: `core.state.present` over a `type: secret` field generated it into Vault at
	// a path keeper derived from (service, incarnation, state field, name). Reading it back
	// and authenticating with it is what proves the whole secret path end to end — a
	// pre-seeded value would also connect, and would prove only that this test can write
	// to Vault.
	adminPass := harness.ReadVaultKV(t, stack,
		redisServiceAlias+"/"+inc+"/system_acl_users/"+redisAdminUser, "password")
	conn := harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 6379, User: redisAdminUser, Pass: adminPass}
	stack.AssertRedisConfigGet(t, conn, "maxmemory-policy", "volatile-lru")

	// (d) And the operator's account is on the instance, not just in the file: the create
	// renders users.acl AND applies each user through the plugin, because a running Redis
	// does not re-read the file it was just handed. Then the account authenticates AS
	// ITSELF — its own minted password, its own key pattern — which is the half `ACL LIST`
	// through the admin cannot see.
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, redisAdminUser, adminPass, redisOperatorUser)
	appPass := harness.ReadVaultKV(t, stack,
		redisServiceAlias+"/"+inc+"/redis_users/"+redisOperatorUser, "password")
	stack.AssertRedisUserAuthenticates(t,
		harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 6379, User: redisOperatorUser, Pass: appPass},
		"app:probe")

	// (e) The service account set is in state under its own field, by name only — the
	// password is `type: secret` and lives in Vault, never in the incarnation row.
	stack.AssertIncarnationState(t, inc, map[string]any{
		"system_acl_users": []any{
			map[string]any{"name": redisAdminUser},
			map[string]any{"name": "monitoring"},
		},
	})

	// (f) POST /v1/incarnations started this run itself, so the audit event is `created`
	// rather than `scenario_started`. That distinction is the one piece of coverage the
	// roster-input path adds over the harness's seed-and-bind helper: the API's own create
	// path, with a roster inside the request.
	stack.AssertAuditEvent(t, "incarnation.created", map[string]any{
		"service": redisServiceAlias,
	})
}

// TestL3bRedisServiceLive_Day2AddUser — the service is OPERATED: a day-2 scenario changes a
// running incarnation and the change reaches the live instance.
//
// This is the claim NIM-871 left with. A create proves a service can be stood up; only a
// day-2 run proves the state it wrote can be read back, extended and re-applied — the
// upsert over `state.redis_users`, a second mint at a derived path, a re-render of the whole
// users.acl, and a plugin verb against the live ACL. Nothing else in this tier does that.
func TestL3bRedisServiceLive_Day2AddUser(t *testing.T) {
	stack := redisServiceStack(t)

	sid := stack.SoulContainers[0].SID
	inc, createApply := stack.CreateIncarnationWithApplyScenario(t, redisServiceInc,
		redisServiceAlias+"@main", "create_from_souls", createFromSoulsInput(sid))
	stack.WaitApplySuccess(t, createApply, 600)
	stack.WaitIncarnationReady(t, inc, 300)

	adminPass := harness.ReadVaultKV(t, stack,
		redisServiceAlias+"/"+inc+"/system_acl_users/"+redisAdminUser, "password")
	// Read BEFORE the day-2 run, compared after: `add_user` passes every other account
	// without a `password` property precisely so their credentials are required rather than
	// reissued, and a rotation is invisible in state — the password was never in state.
	appPassBefore := harness.ReadVaultKV(t, stack,
		redisServiceAlias+"/"+inc+"/redis_users/"+redisOperatorUser, "password")

	// The day-2 run. The new account's password is in neither the input nor Vault yet:
	// add_user mints it, and that is the half of this test a state assertion cannot see.
	addApply := stack.RunScenario(t, inc, "add_user", map[string]any{
		"user": map[string]any{
			"name":  redisDay2User,
			"perms": "~metrics:* +@read",
			"state": "on",
		},
	})
	// 300s: no install this time — the destiny render is idempotent — but the apt branch
	// still runs its repo/pkg steps before the ACL work.
	stack.WaitApplySuccess(t, addApply, 300)
	stack.WaitIncarnationReady(t, inc, 120)

	// (a) State carries BOTH accounts, the existing one first: the scenario filters the
	// same name out and appends, so a lost filter shows up as a duplicate and a lost append
	// as a replacement. Passwords are not here — they are declared secrets.
	stack.AssertIncarnationState(t, inc, map[string]any{
		"redis_users": []any{
			map[string]any{"name": redisOperatorUser, "perms": "~app:* +@read +@write -@dangerous", "state": "on"},
			map[string]any{"name": redisDay2User, "perms": "~metrics:* +@read", "state": "on"},
		},
	})

	// (b) ★ THE LIVE EFFECT, which is the point of a day-2 test: the account exists on the
	// instance after ACL SETUSER, with the rights the request asked for, and it exists as
	// ITSELF — a password add_user minted into Vault during this run authenticates against
	// the running server. The perms are read through the ADMIN connection: `ACL GETUSER` is
	// an admin command, and asking a `+@read` account for it would be a NOPERM about this
	// test rather than about the service.
	adminConn := harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 6379, User: redisAdminUser, Pass: adminPass}
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, redisAdminUser, adminPass, redisDay2User)
	stack.AssertRedisACLUserPerms(t, adminConn, redisDay2User, "~metrics:*")
	day2Pass := harness.ReadVaultKV(t, stack,
		redisServiceAlias+"/"+inc+"/redis_users/"+redisDay2User, "password")
	stack.AssertRedisUserAuthenticates(t,
		harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 6379, User: redisDay2User, Pass: day2Pass},
		"metrics:probe")

	// (c) ★ THE ACCOUNT THAT WAS ALREADY THERE STILL WORKS, under the credential it was
	// given at create. add_user re-renders users.acl WHOLE, which invites two mistakes this
	// catches and nothing else does: dropping the service accounts, and reissuing a password
	// a live client already holds. Same value in Vault, and that same value still accepted
	// by the server — "kept" against "rotated", which state cannot distinguish because the
	// password was never in state.
	appPassAfter := harness.ReadVaultKV(t, stack,
		redisServiceAlias+"/"+inc+"/redis_users/"+redisOperatorUser, "password")
	if appPassAfter != appPassBefore {
		t.Errorf("add_user rotated %s's password: a day-2 run that touches one account must not "+
			"reissue a credential the clients of another are holding", redisOperatorUser)
	}
	stack.AssertRedisUserAuthenticates(t,
		harness.RedisConn{SoulIdx: 0, Host: "127.0.0.1", Port: 6379, User: redisOperatorUser, Pass: appPassAfter},
		"app:probe")
	// And the service's own account survived the whole-file render — without it the next
	// restart leaves this service unable to authenticate to the instance it built.
	stack.AssertRedisACLUser(t, 0, "127.0.0.1", 6379, redisAdminUser, adminPass, redisAdminUser)

	// (d) users.acl on disk carries the new account too. Without this the test would pass on
	// a scenario that only spoke to the running instance, and the account would vanish on
	// the next restart — a failure nobody sees until a reboot.
	stack.AssertHostFileContent(t, 0, "/etc/redis/users.acl", "user "+redisDay2User)

	stack.AssertAuditEvent(t, "incarnation.scenario_started", map[string]any{
		"scenario": "add_user",
		"apply_id": addApply,
	})
}
