#!/usr/bin/env python3
"""classify-l1-failure.py — after L1 fails, say of each failing package whether
it failed at the container layer or on an assertion.

Why this exists (NIM-349). `CLUSTERDOWN`, `connection refused` against a mapped
port and `wait until ready: context deadline exceeded` all arrive as `--- FAIL`,
formatted exactly like an assertion that caught a regression. They are not the
same event and they do not have the same answer: one is rerun-that-package, the
other is fix-the-code. Told apart by eye, every red L1 run costs a person an hour
of reading container logs to decide which one happened, and the cheap way out of
that hour — "L1 is flaky, rerun it" — is how a real regression gets dismissed.

So this does NOT reduce flakes and does NOT touch a readiness wait; weakening a
wait would trade a loud infra failure for a quiet one, which is worse and is
already argued against in the `check-all` target. It only labels, and it labels
into THREE classes rather than two:

  REGRESSION  an assertion failed. A finding.
  INFRA       a signature only the container/daemon layer can produce. Nothing
              was asserted, so there is nothing to conclude about the code.
  UNCLEAR     a signature either layer could produce (`connection refused`,
              `CLUSTERDOWN`). Deliberately not folded into INFRA: a false INFRA
              label is the failure mode that matters here, because it is the one
              that makes a regression disappear.
  TIMEOUT     the package binary hit `-timeout` and dumped every goroutine. It
              names no failing test and prints no error string, so without its
              own verdict it lands in UNCLEAR and reads as "nothing identifies
              the layer" under several hundred lines of stack — which is how a
              hang comes to look like noise.

Nothing is ever downgraded to a pass: the caller's exit code is untouched, and
UNCLEAR is to be treated as a finding until a solitary rerun says otherwise.

Usage: classify-l1-failure.py <go-test-output-file>
"""

import re
import sys

# The strongest signal available, and it is not a guess about library text: 38 of
# the 39 integration suites print exactly this when their own container setup
# fails, e.g. "toll integration: container setup failed (REQUIRE_DOCKER): …". The
# suite is *stating* that it died before running a test. Nothing else in the tree
# prints it, and an assertion cannot.
SETUP_DECLARED = re.compile(r"integration: .*setup failed", re.IGNORECASE)

# Container/daemon-layer text, for the suites and libraries that fail without the
# marker above. These come from testcontainers, not from any assertion here.
#
# This list grows from failures actually observed, never from imagination. The
# first version of it was guessed — it carried "reaper failed" while testcontainers
# prints "wait for reaper" — and the result was two container failures labelled
# REGRESSION, which is the mistake that sends someone hunting a defect that does
# not exist. If a real failure is misfiled, add its text here; do not invent
# entries for failures nobody has seen.
INFRA = [
    "could not start container",
    "failed to start container",
    "unexpected container status",
    "wait for reaper",
    "new reaper",
    "could not start reaper",
    "generic container:",
    "started hook:",
    "check target: retries:",
    "container startup",
    "Error response from daemon",
    "Cannot connect to the Docker daemon",
    "error during connect",
    "docker-credential-",
    "port not found",
    "failed to get mapped port",
    "no such host",
    "manifest unknown",
    "toomanyrequests",
    "device or resource busy",
    "no space left on device",
    # A client that speaks Redis received an HTTP response, so the mapped port it
    # was handed belonged to a different container. Observed as
    # `redis: ping (mode=standalone): redis: can't parse map reply:
    # "HTTP/1.1 400 Bad Request"` inside a test body — which is why it reaches this
    # list rather than the declared-setup marker: the collision surfaces after
    # TestMain, dressed as an assertion. No assertion here produces it.
    "can't parse map reply",
    'HTTP/1.1 400 Bad Request',
]

# Signatures either layer could produce. Named separately on purpose — see the
# module docstring on why these must not be called INFRA.
UNCLEAR = [
    "connection refused",
    "CLUSTERDOWN",
    "i/o timeout",
    "context deadline exceeded",
]

FAIL_LINE = re.compile(r"^FAIL\s+(\S+)")
# MULTILINE is load-bearing, not decoration. Without it `^` anchors to the start
# of the whole blob, so a `--- FAIL:` line was only ever found when it happened to
# be the first thing after the previous package's `ok` line -- and under `-p 4` the
# packages interleave, so usually it was not. The visible symptom was a package
# with nineteen failing tests reported as UNCLEAR "no test reported a failure":
# the verdict that says "fix the code" was unreachable for most real logs, and the
# verdict that replaced it says the opposite.
TEST_FAIL = re.compile(r"^\s*--- FAIL: (\S+)", re.MULTILINE)

# `go test` kills the binary and the binary panics itself, so this text is the
# testing package's, not the tool's.
TIMEOUT_PANIC = "panic: test timed out"
RUNNING_TEST = re.compile(r"^\s*(\S+) \(\d+[hms]")

# The NIM-569 retry logs a recovered attempt, and that log line carries the
# testcontainers error verbatim — `check target: retries:` (INFRA) and `context
# deadline exceeded` (UNCLEAR) — into the blob of a package that then went on to
# run its tests. Left in, a package that recovered from a blip and THEN failed an
# assertion is reported UNCLEAR instead of REGRESSION: the retry would erode the
# one verdict that says "fix the code", precisely in the runs where it did its
# job. The line is the tool talking about itself, so it is not evidence about
# either layer; a bring-up that genuinely failed is unaffected, because the suite
# declares that in its own words (SETUP_DECLARED) and the joined error survives.
RETRY_LOG = re.compile(r"^.*\bintegrationenv: .*$", re.MULTILINE)


def denoise(blob: str) -> str:
    """The package's output with this tool's own retry chatter removed."""
    return RETRY_LOG.sub("", blob)


def import_path_to_pkg(path: str) -> str | None:
    """`.../soul-stack/keeper/internal/redis` → `./internal/redis/`.

    Returns None when the shape is not recognised, so the hint is omitted rather
    than printed wrong.
    """
    marker = "/soul-stack/"
    if marker not in path:
        return None
    rest = path.split(marker, 1)[1].split("/")
    if len(rest) < 2:
        return None
    return "./" + "/".join(rest[1:]) + "/"


# Fixtures for --self-test, each an excerpt of a real `go test` failure this
# script has been wrong about or has had to get right. They exist because the
# classification logic has no other guard: it is not Go, so `make test` never sees
# it, and its first version mislabelled two container failures as REGRESSION
# without anything going red. A wrong label is worse than no label — it sends
# someone hunting a defect that does not exist — so the mapping is pinned here and
# checked by `make check`.
SELF_TEST = [
    (
        "a failing test BELOW other output -> still REGRESSION (needs MULTILINE)",
        "REGRESSION",
        "2026/08/26 22:45:15 INFO artifact: snapshot materialized service name=noop\n"
        "--- FAIL: TestIntegration_Capture_SurvivesTheSuccessTerminal (0.23s)\n"
        "    capture_midrun_integration_test.go:130: incarnation status = \"error_locked\", "
        "want \"ready\"\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/scenario\t91.100s\n",
    ),
    (
        "retry noise on a package that then failed an assertion -> REGRESSION",
        "REGRESSION",
        "2026/08/26 22:45:14 integrationenv: postgres did not come up on attempt 1/3, "
        "starting a NEW container: create container: wait until ready: external check: "
        "check target: retries: 447 address: localhost:32814: context deadline exceeded\n"
        "2026/08/26 22:45:20 integrationenv: postgres came up on attempt 2/3\n"
        "--- FAIL: TestIntegration_ApplyRun_Idempotent (1.20s)\n"
        "    apply_integration_test.go:88: changed = true, want false\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/scenario\t91.004s\n",
    ),
    (
        "every attempt failed -> still INFRA, the joined error survives denoising",
        "INFRA",
        "2026/08/26 22:45:14 integrationenv: postgres did not come up on attempt 1/3, "
        "starting a NEW container: boom\n"
        "2026/08/26 22:46:20 scenario integration: container setup failed (REQUIRE_DOCKER): "
        "postgres: no container became reachable in 3 attempt(s): attempt 1/3: create "
        "container: wait until ready: external check: check target: retries: 447\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/scenario\t272.400s\n",
    ),
    (
        "a killed binary -> TIMEOUT, not UNCLEAR under a goroutine dump",
        "TIMEOUT",
        "panic: test timed out after 15m0s\n"
        "\trunning tests:\n"
        "\tTestIntegration_ApplyRun_Serial (14m59s)\n\n"
        "goroutine 1 [running]:\n"
        "testing.(*M).startAlarm.func1()\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/api\t900.021s\n",
    ),
    (
        "declared setup failure -> INFRA",
        "INFRA",
        '2026/07/30 04:35:58 toll integration: container setup failed (REQUIRE_DOCKER): '
        'create container: reaper: from container "0a0c7998": wait for reaper 0a0c7998: '
        'external check: check target: retries: 440 address: localhost:32916: '
        'unexpected container status "removing"\n'
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/toll\t56.129s\n",
    ),
    (
        "named failing test, no infra text -> REGRESSION",
        "REGRESSION",
        "--- FAIL: TestIntegration_ModuleInstallSynthesis (0.68s)\n"
        "    module_installs_integration_test.go:236: UpsertTaskRegister: FK violation on "
        "apply_task_register_apply_run_fk (SQLSTATE 23503)\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/scenario\t80.606s\n",
    ),
    (
        "named failing test PLUS ambiguous transport text -> UNCLEAR, never REGRESSION",
        "UNCLEAR",
        "--- FAIL: TestIntegration_MarkDisconnected_LeaseAware (0.02s)\n"
        '    integration_test.go:607: redis.NewClient: redis: ping (mode=standalone): '
        'redis: can\'t parse map reply: "HTTP/1.1 400 Bad Request"\n'
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/reaper\t14.933s\n",
    ),
    (
        "no named test at all -> UNCLEAR, because that shape means TestMain died",
        "UNCLEAR",
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/topology\t42.468s\n",
    ),
    (
        "declared marker ALONE -> INFRA (isolates the marker from the fallback list)",
        "INFRA",
        # Deliberately carries the suite's own marker and NOTHING from the INFRA
        # text list. Without this case the marker path is untested: a real log
        # usually holds both, so breaking the regex still yields INFRA through the
        # fallback and the guard stays green. Found by mutating the regex and
        # watching nothing go red.
        "2026/07/30 04:35:58 soulseed integration: setup failed (REQUIRE_DOCKER): "
        "postgres did not come up\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/keeper/internal/soulseed\t62.043s\n",
    ),
]


def classify_blob(blob: str) -> str:
    """The verdict for one package's output. Shared by main() and --self-test so
    the guard cannot drift away from what the tool actually does."""
    if TIMEOUT_PANIC in blob:
        return "TIMEOUT"
    blob = denoise(blob)
    declared = SETUP_DECLARED.search(blob)
    infra_hits = [s for s in INFRA if s in blob]
    unclear_hits = [s for s in UNCLEAR if s in blob]
    tests = TEST_FAIL.findall(blob)
    if declared:
        return "INFRA"
    if tests and not infra_hits and not unclear_hits:
        return "REGRESSION"
    if tests:
        return "UNCLEAR"
    if infra_hits:
        return "INFRA"
    return "UNCLEAR"


def self_test() -> int:
    bad = 0
    for name, want, blob in SELF_TEST:
        got = classify_blob(blob)
        if got == want:
            print(f"classify-l1-failure: ok   {name}")
        else:
            print(f"classify-l1-failure: FAIL {name}: got {got}, want {want}")
            bad += 1
    if bad:
        print(f"classify-l1-failure: {bad} case(s) misclassified. A wrong label is worse than")
        print("classify-l1-failure: none — fix the logic, do not relax the expectation.")
        return 1
    print("classify-l1-failure: self-test passed — the four verdicts still mean what they say")
    return 0


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        return self_test()
    if len(sys.argv) != 2:
        print(__doc__.strip().splitlines()[-1], file=sys.stderr)
        return 2
    try:
        with open(sys.argv[1], encoding="utf-8", errors="replace") as fh:
            lines = fh.read().splitlines()
    except OSError as exc:
        print(f"classify-l1-failure: cannot read the log: {exc}", file=sys.stderr)
        return 2

    # Partition the log by failing package. `go test` prints a package's output
    # before its `FAIL <path>` line, so we accumulate and flush on that line.
    failing: list[tuple[str, list[str]]] = []
    buf: list[str] = []
    for line in lines:
        m = FAIL_LINE.match(line)
        if m:
            failing.append((m.group(1), buf))
            buf = []
            continue
        if line.startswith("ok  \t") or line.startswith("ok\t"):
            buf = []
            continue
        buf.append(line)

    bar = "=" * 72
    print(bar)
    print("L1 failed. Per failing package: container layer, or a regression?")
    print(bar)

    if not failing:
        print()
        print("  No `FAIL <package>` line in the output, yet the run failed. That is")
        print("  usually a build or vet error under `-tags=integration` — the suites")
        print("  never started, so nothing was asserted anywhere. Read the log above.")
        print()
        print(bar)
        return 0

    for path, out in failing:
        raw = "\n".join(out)
        blob = denoise(raw)
        declared = SETUP_DECLARED.search(blob)
        infra_hits = [s for s in INFRA if s in blob]
        unclear_hits = [s for s in UNCLEAR if s in blob]
        tests = TEST_FAIL.findall(blob)
        # What the binary was still running when the alarm went off is the whole
        # of the diagnosis a timeout offers, and it is printed once, above the
        # dump.
        hung = []
        if TIMEOUT_PANIC in raw:
            after = raw.split("running tests:", 1)
            if len(after) == 2:
                for line in after[1].splitlines():
                    if not line.strip():
                        if hung:
                            break
                        continue
                    m = RUNNING_TEST.match(line)
                    if not m:
                        break
                    hung.append(m.group(1))

        # The rule itself lives in classify_blob so --self-test exercises exactly
        # what a real run does. Its ordering is the design: a named
        # `--- FAIL: Test…` line is the only thing that can make this a REGRESSION,
        # required rather than assumed, because a suite dying in TestMain prints
        # `FAIL <pkg>` with no test name — and defaulting that to REGRESSION would
        # report "fix the code" about code that was never exercised. That mistake is
        # not symmetrical with the other one: it sends someone hunting a defect that
        # does not exist, and after twice it teaches them to disbelieve the label.
        verdict = classify_blob(raw)

        print()
        print(f"  {verdict:<11}{path}")
        if verdict == "TIMEOUT" and hung:
            shown = ", ".join(hung[:4]) + (" …" if len(hung) > 4 else "")
            print(f"{'':13}still running when the alarm fired: {shown}")
        if tests:
            shown = ", ".join(tests[:4]) + (" …" if len(tests) > 4 else "")
            print(f"{'':13}failed test(s): {shown}")
        if verdict == "INFRA" and declared:
            print(f"{'':13}the suite declared it: {declared.group(0)[:90]!r}")
        elif verdict == "INFRA":
            print(f"{'':13}matched: {', '.join(repr(s) for s in infra_hits[:3])}")
        elif verdict == "UNCLEAR" and (unclear_hits or infra_hits):
            hits = (unclear_hits + infra_hits)[:3]
            print(f"{'':13}matched: {', '.join(repr(s) for s in hits)}"
                  " — either layer can print these")
        elif verdict == "UNCLEAR":
            print(f"{'':13}no test reported a failure, so nothing here identifies the layer")

        pkg = import_path_to_pkg(path)
        if verdict == "TIMEOUT":
            print(f"{'':13}The binary was killed at INTEGRATION_TIMEOUT and dumped every")
            print(f"{'':13}goroutine, so it named no failing test. Read the dump for the")
            print(f"{'':13}test above, not for an error string — there is none. A hang is")
            print(f"{'':13}not the container layer and not an assertion; it is its own")
            print(f"{'':13}finding.")
            if pkg:
                print(f"{'':17}make test-integration PKG={pkg}")
        elif verdict == "REGRESSION":
            print(f"{'':13}An assertion failed, so the code WAS exercised. A finding —")
            print(f"{'':13}unless a solitary rerun clears it, and if it does, that is a")
            print(f"{'':13}finding too: a test that only fails under load is the very")
            print(f"{'':13}defect this ticket is about. Either way it is not nothing.")
            if pkg:
                print(f"{'':17}make test-integration PKG={pkg}")
        elif verdict == "INFRA":
            print(f"{'':13}The container layer never came up, so nothing was asserted here.")
            if pkg:
                print(f"{'':13}Rerun it alone (one docker daemon, far less contention):")
                print(f"{'':17}make test-integration PKG={pkg}")
        else:
            print(f"{'':13}Treat as a finding until a solitary rerun says otherwise:")
            if pkg:
                print(f"{'':17}make test-integration PKG={pkg}")

    print()
    print("  Nothing above was downgraded: L1 failed and the caller still exits")
    print("  non-zero. This says WHERE to look, never whether to care. A failure")
    print("  that survives a solitary rerun is a finding regardless of its label.")
    print(bar)
    return 0


if __name__ == "__main__":
    sys.exit(main())
