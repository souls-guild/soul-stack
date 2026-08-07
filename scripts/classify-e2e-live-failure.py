#!/usr/bin/env python3
"""classify-e2e-live-failure.py — after the e2e-live gate goes red, say of each
gate test whether its stand failed to come up or the test itself failed.

Why this exists (NIM-406). A container that is not serving its mapped port yet
makes the harness die inside NewStack, and that arrives as `--- FAIL: TestX`
formatted exactly like an assertion that caught a regression. Four gate failures
were read as regressions while being bring-up failures — the tell was that they
died in ~4 seconds, which no real apply does, and reading it required someone to
know that. The gate is a blocking pre-tag step (RELEASING.md step e); a blocking
step whose red is not legible is a step people learn to rerun until green, and
that habit is what silently retires a regression.

This does NOT rerun anything, does NOT relax a wait, and does NOT touch the
caller's exit code. It labels, and it labels per TEST rather than per package:
the e2e-live gate is one package running nine tests, so a package-level verdict
would let one stand failure speak for a real assertion failure in the same run.
That case is fixture-pinned below, because it is the whole reason this is not
just `classify-l1-failure.py` pointed at a different log.

Three verdicts, and only the first is inferred from anything:

  STAND-SETUP    the harness declared it — see standSetupMarker in
                 tests/e2e-live/harness/setupdecl.go. Bring-up died, so no
                 assertion in that test ever ran.
  TEST-FAILURE   `--- FAIL` with no declaration. Something the test body did
                 failed. A finding.
  NOT-RUN        the mask named it and the log never shows it running, or it
                 skipped. Not a pass — `make e2e-live-gate` already treats a
                 missing `--- PASS` as false-green (NIM-45).

Note there is no signature list here — no "connection refused means infra". That
is the deliberate difference from the L1 classifier, which needs one because a
suite there may fail without declaring. Here every path that runs before the
test body declares itself, so guessing from library text would only add a way to
be wrong. And the residual error is one-directional: an entry point that forgets
to declare shows up as TEST-FAILURE, costing someone a look, never as
STAND-SETUP on a real regression.

Usage: classify-e2e-live-failure.py <gate-log> [test-name ...]
       classify-e2e-live-failure.py --self-test
"""

import pathlib
import re
import sys

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
MARKER_SOURCE = REPO_ROOT / "tests" / "e2e-live" / "harness" / "setupdecl.go"
MARKER_DECL = re.compile(r'standSetupMarker\s*=\s*"([^"]+)"')

# Top-level tests only: subtests arrive as `TestX/sub` and belong to their
# parent's block. `-p 1` in the gate makes the package serial, so a top-level
# `=== RUN` reliably opens the next block.
TOP_RUN = re.compile(r"^=== RUN\s+(Test[^/\s]+)\s*$")
TOP_RESULT = re.compile(r"^--- (PASS|FAIL|SKIP): (Test[^/\s]+)")


def read_marker() -> str:
    """The marker, read out of the Go source that emits it.

    Not a copy. If someone rewords standSetupMarker, a copy here would keep
    matching nothing while every verdict quietly became TEST-FAILURE — the
    classifier would still run, still print, and still be wrong, which is the
    failure mode this whole ticket is about. Reading the constant means that
    edit breaks --self-test in `make check` instead.
    """
    try:
        src = MARKER_SOURCE.read_text(encoding="utf-8")
    except OSError as exc:
        raise SystemExit(
            f"classify-e2e-live-failure: cannot read {MARKER_SOURCE}: {exc}\n"
            "The marker is defined there and is not duplicated here on purpose."
        )
    m = MARKER_DECL.search(src)
    if not m:
        raise SystemExit(
            f"classify-e2e-live-failure: no `standSetupMarker = \"…\"` in {MARKER_SOURCE}.\n"
            "Either it was renamed or the declaration is gone. Until it is back, every\n"
            "stand-setup failure in the gate log reads as an assertion failure."
        )
    return m.group(1)


def partition(lines: list[str]) -> list[tuple[str, str, str]]:
    """(test name, result, its output) for each top-level test in the log.

    `result` is PASS/FAIL/SKIP, or "" when the block never closed — which is
    what a timeout or a panic mid-test leaves behind.
    """
    out: list[tuple[str, str, str]] = []
    name = ""
    buf: list[str] = []

    def flush(result: str) -> None:
        nonlocal name, buf
        if name:
            out.append((name, result, "\n".join(buf)))
        name, buf = "", []

    for line in lines:
        run = TOP_RUN.match(line)
        if run:
            flush("")
            name = run.group(1)
            continue
        res = TOP_RESULT.match(line)
        if res and res.group(2) == name:
            buf.append(line)
            flush(res.group(1))
            continue
        if name:
            buf.append(line)
    flush("")
    return out


def classify(result: str, blob: str, marker: str) -> str:
    """The verdict for one test's block. Shared by main() and --self-test so the
    guard cannot drift away from what the tool actually does."""
    if result == "PASS":
        return "PASS"
    if result == "SKIP":
        return "NOT-RUN"
    if marker in blob:
        return "STAND-SETUP"
    if result == "FAIL":
        return "TEST-FAILURE"
    # No result line: the test started and never finished — the shape a
    # `panic: test timed out` leaves. Nothing says which layer died, and the
    # safe reading is the one that keeps a person looking.
    return "TEST-FAILURE"


# Fixtures for --self-test. Every failure text below is real: the three
# transport errors are the ones quoted in NIM-406, one per container, each from
# a different hole in a different wait strategy.
#
# They exist because this logic is the one piece of the ticket `make test`
# cannot see — it is not Go — and a classifier that is quietly wrong is strictly
# worse than none: it puts a confident label on the guess someone was going to
# make anyway.
SELF_TEST: list[tuple[str, list[tuple[str, str]], str]] = [
    (
        "declared bring-up failure -> STAND-SETUP",
        [("TestL3bSmokeNginxLive_InstallAndStart", "STAND-SETUP")],
        "=== RUN   TestL3bSmokeNginxLive_InstallAndStart\n"
        "    stack.go:187: NewStack: vault: InitVaultTestSecrets: enable pki mount: "
        "Put \"http://127.0.0.1:33242/v1/sys/mounts/pki\": dial tcp 127.0.0.1:33242: "
        "connect: connection refused\n"
        "    setupdecl.go:57: @@MARKER@@ — no assertion in this test ever ran\n"
        "--- FAIL: TestL3bSmokeNginxLive_InstallAndStart (4.02s)\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e-live\t4.115s\n",
    ),
    (
        "assertion failure, no declaration -> TEST-FAILURE",
        [("TestL3bModuleDeliveryLive_SynthesisFetchHotRegister", "TEST-FAILURE")],
        "=== RUN   TestL3bModuleDeliveryLive_SynthesisFetchHotRegister\n"
        "    module_delivery_live_test.go:212: apply run finished with status=failed, "
        "want succeeded\n"
        "--- FAIL: TestL3bModuleDeliveryLive_SynthesisFetchHotRegister (312.44s)\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e-live\t312.51s\n",
    ),
    (
        "one stand failure and one real regression in the SAME run -> both, separately",
        # The case a per-package verdict gets wrong, and the reason this is not
        # classify-l1-failure.py pointed at another log. Whichever single label
        # that tool picked here, it would speak for the other test too.
        [
            ("TestL3bPluginChannel_CatalogAndAllow", "STAND-SETUP"),
            ("TestL3bRedisLive_Day2AddUser", "TEST-FAILURE"),
        ],
        "=== RUN   TestL3bPluginChannel_CatalogAndAllow\n"
        "    stack.go:181: NewStack: postgres: keeper init: pg ping: "
        "dial tcp 127.0.0.1:34492: connect: connection refused\n"
        "    setupdecl.go:57: @@MARKER@@ — no assertion in this test ever ran\n"
        "--- FAIL: TestL3bPluginChannel_CatalogAndAllow (3.88s)\n"
        "=== RUN   TestL3bRedisLive_Day2AddUser\n"
        "    redis_ops_adduser_live_test.go:118: ACL GETUSER alice: got \"\", want the "
        "created user\n"
        "--- FAIL: TestL3bRedisLive_Day2AddUser (288.10s)\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e-live\t291.99s\n",
    ),
    (
        "transport text WITHOUT a declaration stays TEST-FAILURE",
        # Isolates the marker from any temptation to sniff library text: this
        # carries the redis wait-strategy failure verbatim, and it is still not
        # STAND-SETUP, because nothing declared it.
        [("TestL3bRedisLive_Day2Restart", "TEST-FAILURE")],
        "=== RUN   TestL3bRedisLive_Day2Restart\n"
        "    redis_ops_common_test.go:88: sentinel probe: wait until ready: external "
        "check: get state: context deadline exceeded\n"
        "--- FAIL: TestL3bRedisLive_Day2Restart (61.30s)\n",
    ),
    (
        "`connection refused` mid-test WITHOUT a declaration stays TEST-FAILURE",
        # The one signature it is most tempting to shortcut on — NIM-406's own
        # write-up proposed calling it infra — pinned as forbidden. A soul that
        # drops its connection during an apply prints exactly this, and there
        # the code WAS exercised. Teaching classify() to match the text instead
        # of the declaration makes this case go red, which is why the fixture
        # above it is not enough: that one carries different text, so the
        # tempting shortcut slipped past it unnoticed.
        [("TestL3bRedisLive_Day2UpdateUsers", "TEST-FAILURE")],
        "=== RUN   TestL3bRedisLive_Day2UpdateUsers\n"
        "    redis_ops_common_test.go:204: post-apply ACL check: dial tcp "
        "127.0.0.1:39117: connect: connection refused\n"
        "--- FAIL: TestL3bRedisLive_Day2UpdateUsers (274.61s)\n",
    ),
    (
        "started and never finished (timeout panic) -> TEST-FAILURE, not swallowed",
        [("TestL3bRedisLive_Day2Destroy", "TEST-FAILURE")],
        "=== RUN   TestL3bRedisLive_Day2Destroy\n"
        "panic: test timed out after 45m0s\n",
    ),
    (
        "a skip is NOT-RUN, never a pass",
        # NewStack skips when the keeper binary is missing, and NIM-45 exists
        # because a skip that reads as green is how a gate reports success for
        # work it never did. Nothing asserted, so it cannot be a pass here
        # either.
        [("TestL3bPluginChannel_CatalogAndAllow", "NOT-RUN")],
        "=== RUN   TestL3bPluginChannel_CatalogAndAllow\n"
        "    stack.go:158: L3b: keeper binary not found (stat keeper/bin/keeper: no such "
        "file or directory); export KEEPER_BIN or run `make build`\n"
        "--- SKIP: TestL3bPluginChannel_CatalogAndAllow (0.00s)\n",
    ),
    (
        "a pass stays a pass",
        [("TestL3bSmokeNginxLive_InstallAndStart", "PASS")],
        "=== RUN   TestL3bSmokeNginxLive_InstallAndStart\n"
        "--- PASS: TestL3bSmokeNginxLive_InstallAndStart (241.02s)\n",
    ),
]


def self_test() -> int:
    marker = read_marker()
    print(f"classify-e2e-live-failure: marker read from {MARKER_SOURCE.name}: {marker!r}")
    bad = 0
    for name, want, blob in SELF_TEST:
        got = [
            (test, classify(result, out, marker))
            for test, result, out in partition(blob.replace("@@MARKER@@", marker).splitlines())
        ]
        if got == want:
            print(f"classify-e2e-live-failure: ok   {name}")
        else:
            print(f"classify-e2e-live-failure: FAIL {name}:\n     got  {got}\n     want {want}")
            bad += 1
    if bad:
        print(f"classify-e2e-live-failure: {bad} case(s) misclassified. A wrong label is worse")
        print("classify-e2e-live-failure: than none — fix the logic, do not relax the fixture.")
        return 1
    print("classify-e2e-live-failure: self-test passed — the verdicts still mean what they say")
    return 0


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        return self_test()
    if len(sys.argv) < 2:
        print(__doc__.strip().splitlines()[-2].strip(), file=sys.stderr)
        return 2

    marker = read_marker()
    expected = sys.argv[2:]
    try:
        lines = pathlib.Path(sys.argv[1]).read_text(
            encoding="utf-8", errors="replace"
        ).splitlines()
    except OSError as exc:
        print(f"classify-e2e-live-failure: cannot read the log: {exc}", file=sys.stderr)
        return 2

    seen = {
        test: classify(result, out, marker) for test, result, out in partition(lines)
    }
    for test in expected:
        seen.setdefault(test, "NOT-RUN")

    bar = "=" * 72
    print(bar)
    print("e2e-live gate failed. Per test: did the stand come up, or did the test fail?")
    print(bar)

    interesting = {t: v for t, v in seen.items() if v != "PASS"}
    if not interesting:
        print()
        print("  Every named test reported PASS, yet the gate failed. That is the gate's")
        print("  own guards talking — a `(cached)` summary or a test the mask never ran.")
        print("  Read the lines `e2e-live-gate:` printed above.")
        print()
        print(bar)
        return 0

    for test, verdict in interesting.items():
        print()
        print(f"  {verdict:<14}{test}")
        if verdict == "STAND-SETUP":
            print(f"{'':16}The harness declared it: bring-up died before the test body,")
            print(f"{'':16}so nothing was asserted and there is no finding about the code")
            print(f"{'':16}here. Rerun it alone — one docker daemon, far less contention:")
            print(f"{'':18}make e2e-live-gate   # or -run '^{test}$' for just this one")
        elif verdict == "NOT-RUN":
            print(f"{'':16}The mask named it and it never reported a result. Not a pass:")
            print(f"{'':16}an earlier test may have aborted the binary, or the name drifted.")
        else:
            print(f"{'':16}No bring-up declaration, so the test body is what failed. A")
            print(f"{'':16}finding — and if a solitary rerun clears it, that is a finding")
            print(f"{'':16}too: a test that only fails under load is exactly the defect")
            print(f"{'':16}NIM-406 is about. Either way it is not nothing.")

    print()
    print("  Nothing above was downgraded: the gate failed and the caller still exits")
    print("  non-zero. This says WHERE to look, never whether to care.")
    print(bar)
    return 0


if __name__ == "__main__":
    sys.exit(main())
