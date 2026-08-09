//go:build e2e_live

// L3b, NOT in the gate: the create path with the release tarballs coming from the
// real GitHub Releases, the way an operator's first run does (NIM-542).
//
// Every other L3b test now takes those tarballs from the harness's local mirror, so
// the gate is hermetic and its "three runs on an unchanged slice agree" holds. That
// hermetization moved a real dependency out of the gate, and a dependency nobody
// exercises is one that rots: the upstream URL shape, the pinned versions and the
// digests are all still load-bearing the moment a user runs examples/service/redis
// without an internal mirror. This test is where that keeps being checked.
//
// Deliberately absent from E2E_GATE_TESTS. It is the one test in the tier allowed to
// fail for a reason outside the repository, and a blocking gate must never be. It
// runs under the full `make e2e-live` (nightly), where a red is read by a human.
package e2e_live_test

import (
	"testing"

	"github.com/souls-guild/soul-stack/tests/e2e-live/harness"
)

// TestL3bRedisLiveUpstream_ArtifactsFromGitHub — one create, artifacts from github.com,
// asserting the three binaries landed and their services came up.
//
// The environment-vs-defect line (NIM-542 acceptance (c)) is drawn twice, because the
// network can go away at two different moments and the two read differently:
//
//   - Before the stand: RequireUpstreamArtifacts probes the exact three URLs and skips,
//     naming the host. Nothing of this repository has run yet, so a failure here cannot
//     be about this repository — and a 25-minute stand is not spent to learn that.
//   - During the run: a create that dies mid-fetch is a `--- FAIL` like any other, and
//     ReportUpstreamIfItWentAway re-probes and prints one decisive line under it. If
//     upstream is gone, the line says so; if upstream is fine, the line says THAT, which
//     is the more valuable half — it removes the excuse and leaves a real finding.
func TestL3bRedisLiveUpstream_ArtifactsFromGitHub(t *testing.T) {
	harness.RequireUpstreamArtifacts(t)
	defer harness.ReportUpstreamIfItWentAway(t)

	stack, inc, adminPass := setupRedisStandaloneWith(t, true, "rdb", "noeviction", 1024)
	_ = inc

	// The three tarballs are fetched and extracted by three separate destinies —
	// two of them checksummed, node-exporter deliberately not (it declares no
	// `sha256` input; `core.url` falls back to content-hash idempotency). Which is
	// why the assertion is on the extracted binary rather than on the fetch step's
	// exit code: for node-exporter that is the only thing standing between "the
	// bytes arrived" and "the step returned 0".
	stack.AssertHostFileExists(t, 0, "/usr/local/bin/node_exporter")
	stack.AssertHostFileExists(t, 0, "/usr/local/bin/redis_exporter")
	stack.AssertHostFileExists(t, 0, "/usr/local/bin/vector")

	// And that they are the real thing, not a truncated download that happened to be
	// written: each unit only stays active if its binary starts and keeps running.
	stack.AssertHostServiceActive(t, 0, "node_exporter")
	stack.AssertHostServiceActive(t, 0, "redis_exporter")
	stack.AssertHostServiceActive(t, 0, "vector")

	// Redis itself is untouched by the artifact source; one cheap probe that the run
	// this test rode in on was a real create and not an empty one.
	stack.AssertRedisRole(t, plainConn(adminPass), "master")
}
