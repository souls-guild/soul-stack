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
the e2e-live gate is one package running several tests, so a package-level verdict
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
import subprocess
import sys
import tempfile

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
MARKER_SOURCE = REPO_ROOT / "tests" / "e2e-live" / "harness" / "setupdecl.go"
MARKER_DECL = re.compile(r'standSetupMarker\s*=\s*"([^"]+)"')

# NIM-490's pre-flight refuses a keeper binary that is not this tree, and it
# does so INSIDE the declared bring-up region, so its refusal arrives here as
# STAND-SETUP. The verdict is right — nothing was asserted — but this tool's
# standing advice for STAND-SETUP is not: "rerun it alone" reproduces a stale
# binary exactly, forever. This needle is what lets the advice say so.
#
# It is not a verdict signature and does not become one: no verdict below reads
# it. The docstring's "no signature lists" holds where it matters, which is the
# label; what this changes is the sentence after the label.
PROVENANCE_SOURCE = REPO_ROOT / "tests" / "e2e-live" / "harness" / "provenance.go"
STALE_NEEDLE = "the keeper binary is STALE"

# Whose block the following lines belong to. Go's chatty printer moves
# attribution with three lines and only three: `=== RUN` when a test starts,
# `=== CONT` when a paused one resumes, `=== NAME` when output goes back to a
# test already running. Subtests (`TestX/sub`) fold into their parent.
#
# Reading all three rather than just `=== RUN` is what makes this correct when
# tests run concurrently, and the reason is not a hypothetical. The gate passes
# `-p 1`, which bounds how many PACKAGES run at once and says nothing about one
# package's binary — `-parallel` and t.Parallel() are what would interleave
# these, and the gate sets neither. So today the suite is serial by the fact
# that no e2e-live test calls t.Parallel(), not by any flag. Keying only on
# `=== RUN` made a whole block's output land on its neighbour the moment that
# stopped being true, and the marker lands with it: SELF_TEST pins the
# interleaving that used to print STAND-SETUP on the assertion failure and
# TEST-FAILURE on the bring-up one — both labels wrong, one of them in the
# direction this tool exists to never be wrong in.
TOP_SWITCH = re.compile(r"^=== (?:RUN|CONT|NAME)\s+(Test\S+)\s*$")
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

    One buffer per test rather than one buffer for "the current test", so a log
    that switches back and forth between tests still gives each its own lines.
    A result line is attributed by the name ON IT, not by whichever block was
    open, because those are the same thing only when nothing interleaves.
    """
    blocks: dict[str, list[str]] = {}
    results: dict[str, str] = {}
    name = ""

    for line in lines:
        switch = TOP_SWITCH.match(line)
        if switch:
            name = switch.group(1).split("/", 1)[0]
            blocks.setdefault(name, [])
            continue
        res = TOP_RESULT.match(line)
        if res:
            blocks.setdefault(res.group(2), []).append(line)
            results[res.group(2)] = res.group(1)
            # The test is closed; what follows is the package trailer until
            # something claims it.
            name = ""
            continue
        if name:
            blocks[name].append(line)

    return [(test, results.get(test, ""), "\n".join(buf)) for test, buf in blocks.items()]


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


def next_step(test: str, stale: bool) -> tuple[str, str]:
    """What a reader of a STAND-SETUP verdict should do: headline, then command.

    A pure function for the same reason classify() is one — this is the only
    output of the whole tool that tells someone to DO something, and getting it
    wrong costs more than a mislabelled verdict. Inline in the report loop it was
    reachable only by rendering a log, so no fixture could see it and a branch
    could be deleted without one of them going red.

    The stale binary is the case that forces the branch. Its verdict is
    STAND-SETUP and everything STAND-SETUP normally means — the daemon, the
    contention, try it alone — is false there: the binary is from another build,
    so a rerun without `make build` reproduces the refusal identically, every
    time. That is the one shape where the generic advice is a loop.
    """
    if stale:
        return (
            "Rebuild FIRST — rerunning alone changes nothing:",
            f"make build && make e2e-live-gate   # or -run '^{test}$' for just this one",
        )
    return (
        "Rerun it alone — one docker daemon, far less contention:",
        f"make e2e-live-gate   # or -run '^{test}$' for just this one",
    )


def report_lines(test: str, verdict: str, blob: str) -> list[str]:
    """Everything printed under one test's verdict line, as text rather than as
    side effects.

    Pure for the same reason next_step() is, and it exists as well as next_step()
    because the JOIN is its own way to be wrong: next_step can branch perfectly
    while the caller passes the wrong argument, and nothing that only looks at
    next_step would notice. Here the blob goes in and the advice comes out, which
    is the whole path a fixture needs to reach.

    What that does NOT do is close the join, and the first version of this
    docstring claimed it did. Extracting the render moves the join up a level
    rather than removing it: the fixtures now choose the blob themselves, so
    `report_lines(test, verdict, "")` in main() keeps every one of them green
    while deleting the stale advice from every report the tool prints. Pinning
    the value, not just the branch, needs a real log through the real entry
    point — check_report_renders_the_join below.
    """
    pad, deep = " " * 16, " " * 18
    if verdict == "STAND-SETUP":
        stale = STALE_NEEDLE in blob
        out = [
            f"{pad}The harness declared it: bring-up died before the test body,",
            f"{pad}so nothing was asserted and there is no finding about the code",
            f"{pad}here.",
        ]
        if stale:
            out += [
                f"{pad}Not the machine, though — the harness refused before bringing",
                f"{pad}anything up, because keeper/bin/keeper does not carry the code",
                f"{pad}in the tree (NIM-490). Docker and Vault are not implicated and",
                f"{pad}nothing here is flaky. The lines above name the mismatch.",
            ]
        headline, command = next_step(test, stale)
        return out + [f"{pad}{headline}", f"{deep}{command}"]
    if verdict == "NOT-RUN":
        return [
            f"{pad}The mask named it and it never reported a result. Not a pass:",
            f"{pad}an earlier test may have aborted the binary, or the name drifted.",
        ]
    return [
        f"{pad}No bring-up declaration, so the test body is what failed. A",
        f"{pad}finding — and if a solitary rerun clears it, that is a finding",
        f"{pad}too: a test that only fails under load is exactly the defect",
        f"{pad}NIM-406 is about. Either way it is not nothing.",
    ]


def check_stale_needle_matches_the_harness() -> str:
    """STALE_NEEDLE still appears in the message provenance.go emits. "" if it does.

    The marker is READ out of the Go source; this needle cannot be, because it is
    a fragment of a format string rather than a constant, and hoisting it into
    one would put the same fragment in three tiers' provenance.go for one tier's
    classifier. So it is a copy — and an unchecked copy is precisely the failure
    this tool is built against: reword the Go message and the branch stops firing,
    in silence, leaving the wrong advice back in place with no test red.

    Both stale paths are required to carry it, not just one. They are two separate
    format strings (version mismatch, uncommitted-source freshness) and the
    freshness one is the shape a developer mid-edit actually hits; a needle
    matching only the other would cover the rarer half and read as covering both.

    Counted in string LITERALS, not in the file's text. A count over the raw
    source cannot tell the message the harness prints from a comment that quotes
    it, so rewording both format strings while a doc comment kept the old wording
    would leave this green — the guard satisfied by something adjacent to its
    subject, which is the ticket's own shape.
    """
    try:
        src = PROVENANCE_SOURCE.read_text(encoding="utf-8")
    except OSError as exc:
        return f"cannot read {PROVENANCE_SOURCE}: {exc}"

    # The scanner's own known-bad, and it has to be the realistic shape. A plain
    # comment does not distinguish stepping over a comment from walking through
    # it — there is nothing in it for the scanner to mistake for a literal. A
    # comment that QUOTES the message does, in either of Go's two styles, and
    # that is what a reworded message actually leaves behind.
    probe = (f"package p\n"
             f'// prose quoting `{STALE_NEEDLE}` and again "{STALE_NEEDLE}"\n'
             f'/* and `{STALE_NEEDLE}` in a block comment */\n'
             f'var s = "{STALE_NEEDLE} in a literal"\n')
    if sum(lit.count(STALE_NEEDLE) for lit in go_string_literals(probe)) != 1:
        return (
            "the literal scanner counts prose, so this check can be satisfied by a comment\n"
            "     that quotes a message the harness no longer prints. Fix go_string_literals\n"
            "     before trusting the count below."
        )

    hits = sum(lit.count(STALE_NEEDLE) for lit in go_string_literals(src))
    if hits < 2:
        return (
            f"{STALE_NEEDLE!r} appears in {hits} string literal(s) of "
            f"{PROVENANCE_SOURCE.name},\n"
            "     want both stale paths (version mismatch AND uncommitted-source freshness).\n"
            "     The rebuild-first advice keys on this string; where it does not appear,\n"
            "     a refusal is reported as an ordinary bring-up failure and the reader is\n"
            "     told to rerun the test alone, which reproduces a stale binary forever."
        )
    return ""


def go_string_literals(src: str) -> list[str]:
    """Every string literal in a Go source, with comments and runes stepped over.

    Twenty lines rather than a regex because the two halves define each other:
    stripping comments needs to know where strings are (a URL in a literal holds
    `//`), and finding strings needs to know where comments are (a comment can
    hold a quote). One pass with a position that only ever moves forward settles
    both.

    Escapes are consumed, not decoded — the needle is plain ASCII, and decoding
    would mean reimplementing Go's escape rules for no gain. What matters is that
    a `\\"` does not end the literal early and hand the rest of the line back to
    the scanner as code.
    """
    out: list[str] = []
    i, n = 0, len(src)
    while i < n:
        c = src[i]
        if c == "/" and i + 1 < n and src[i + 1] == "/":
            nl = src.find("\n", i)
            i = n if nl < 0 else nl + 1
        elif c == "/" and i + 1 < n and src[i + 1] == "*":
            end = src.find("*/", i + 2)
            i = n if end < 0 else end + 2
        elif c == "`":
            end = src.find("`", i + 1)
            if end < 0:
                break
            out.append(src[i + 1:end])
            i = end + 1
        elif c in "\"'":
            j, buf = i + 1, []
            while j < n and src[j] != c and src[j] != "\n":
                if src[j] == "\\":
                    j += 1
                else:
                    buf.append(src[j])
                j += 1
            if c == '"':
                out.append("".join(buf))
            i = j + 1
        else:
            i += 1
    return out


def check_next_step_branches() -> str:
    """next_step() still distinguishes a stale binary from everything else. "" if so.

    The verdict fixtures compare (test, verdict) and stop there, so they would
    stay green with this branch collapsed to its generic half — and that collapse
    is invisible in review, because the remaining line reads perfectly well. What
    it says is just wrong for the one case that reaches it.

    Both directions are asserted. Checking only the stale branch would pass a
    next_step() that prescribed `make build` for every failure, which is a lie
    about the NIM-406 contention case this tool exists to triage.
    """
    stale_head, stale_cmd = next_step("TestSomething", True)
    plain_head, plain_cmd = next_step("TestSomething", False)

    if "make build" not in stale_cmd:
        return (
            f"a stale binary is told {stale_cmd!r}, which does not rebuild.\n"
            "     That command re-runs the very binary the harness just refused."
        )
    if "make build" in plain_cmd:
        return (
            f"the generic advice is {plain_cmd!r}. A rebuild is not the answer to a\n"
            "     container that lost a race, and prescribing it everywhere teaches\n"
            "     readers to skip the sentence."
        )
    if stale_head == plain_head:
        return (
            f"both branches print the same headline {stale_head!r}. The command differs\n"
            "     and the sentence above it does not, so the reader is told to rerun\n"
            "     while being handed a rebuild."
        )
    for name, cmd in (("stale", stale_cmd), ("generic", plain_cmd)):
        if "TestSomething" not in cmd:
            return (
                f"the {name} command {cmd!r} does not name the test. Both are meant to\n"
                "     be copy-pasteable for the ONE test that failed."
            )

    # And the join, which is a separate way to be wrong: a next_step() that
    # branches correctly is worth nothing if the report hands it the wrong
    # argument. Same verdict, same test name, blobs that differ only in the
    # harness's own words.
    stale_report = "\n".join(report_lines(
        "TestSomething", "STAND-SETUP",
        f"    provenance.go:131: e2e-live: {STALE_NEEDLE} - an uncommitted source file",
    ))
    plain_report = "\n".join(report_lines(
        "TestSomething", "STAND-SETUP",
        "    stack.go:187: NewStack: vault: dial tcp 127.0.0.1:33242: connection refused",
    ))
    if "make build" not in stale_report:
        return (
            "next_step() branches, but the report does not reach the branch: a stale\n"
            "     block is still told to rerun. Whatever computes the flag from the block\n"
            "     is not doing it."
        )
    if "make build" in plain_report:
        return (
            "the report prescribes a rebuild for a block with no staleness in it. The\n"
            "     flag is being set from something other than the harness's message."
        )
    return ""


def check_report_renders_the_join() -> str:
    """A real log through the real entry point still carries the fix. "" if it does.

    check_next_step_branches calls report_lines() with a blob it wrote itself, so
    it proves the render BRANCHES on a stale blob and says nothing about which
    blob main() hands it. That is the join, and it is its own defect class: a
    correct function reached with an empty argument returns confidently, and the
    caller prints the confident answer.

    The known-bad is not hypothetical. `blob_of.get(test, "")` -> `""` in main()
    keeps every fixture in this file green while deleting the "Not the machine"
    paragraph and the `make build` command from every report the tool will ever
    print — which is the whole of what NIM-490 added here. A subprocess is the
    price of seeing it: calling main() in-process would need stdout capture and
    would still share this module's globals, so it would not exercise argv, the
    file read or read_marker() the way the gate does.

    The log below is in the VERBOSE shape (`=== RUN` before the body) because
    that is the shape partition() attributes output in: after a result line it
    hands attribution back, so a body written under `--- FAIL:` would reach
    classify() as an empty blob and this check would pass for the wrong reason.
    """
    script = pathlib.Path(__file__).resolve()
    marker = read_marker()
    declared = f"    panic.go:694: {marker} — the stand never came up."
    stale_advice = ("Not the machine", "make build")
    cases = (
        (
            "stale binary",
            "=== RUN   TestSomething\n"
            f"    provenance.go:131: e2e-live: {STALE_NEEDLE} - an uncommitted source "
            "file is newer than it: keeper/internal/api/server.go.\n"
            + declared + "\n--- FAIL: TestSomething (0.19s)\n",
            stale_advice,
            (),
        ),
        (
            "ordinary bring-up failure",
            "=== RUN   TestSomething\n"
            "    stack.go:187: NewStack: vault: dial tcp 127.0.0.1:33242: "
            "connect: connection refused\n"
            + declared + "\n--- FAIL: TestSomething (44.10s)\n",
            (),
            stale_advice,
        ),
    )

    for name, blob, want, unwanted in cases:
        with tempfile.TemporaryDirectory() as tmp:
            log = pathlib.Path(tmp) / "l3b.log"
            log.write_text(blob, encoding="utf-8")
            proc = subprocess.run(
                [sys.executable, str(script), str(log), "TestSomething"],
                capture_output=True, text=True,
            )
        report = proc.stdout
        indented = "\n".join(f"       {line}" for line in report.splitlines())
        if "STAND-SETUP" not in report:
            return (f"the {name} log did not even render as STAND-SETUP, so this check\n"
                    "     is not looking at the branch it claims to.\n"
                    f"{indented}")
        for needle in want:
            if needle not in report:
                return (f"the report rendered for a {name} does not contain {needle!r}.\n"
                        "     report_lines() and next_step() can both be correct and this\n"
                        "     still fail: what is broken is the blob main() hands them, and\n"
                        f"     no other check here renders a log at all.\n{indented}")
        for needle in unwanted:
            if needle in report:
                return (f"the report rendered for an {name} contains {needle!r}, so the\n"
                        "     stale-binary advice is reaching blocks that are not stale — a\n"
                        "     reader whose stand simply died is told to rebuild.\n"
                        f"{indented}")
    return ""


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
        "interleaved output -> each label follows its own test, not the open block",
        # The parallel shape, pinned because getting it wrong is not a near-miss
        # — it INVERTS both labels. Keyed on `=== RUN` alone, everything after
        # the second RUN lands on the second test: the marker included. The tool
        # then printed TEST-FAILURE on the bring-up failure (a person hunts a
        # defect that is not there) and STAND-SETUP on the real regression (a
        # person reruns and moves on). The second is how a regression is retired
        # by a tool built to prevent exactly that.
        #
        # Nothing in the suite calls t.Parallel() today. That is a property of
        # the tests, not of the gate's flags, and it is not one this tool should
        # depend on being noticed if it changes.
        [
            ("TestL3bSmokeNginxLive_InstallAndStart", "STAND-SETUP"),
            ("TestL3bRedisLive_Day2AddUser", "TEST-FAILURE"),
        ],
        "=== RUN   TestL3bSmokeNginxLive_InstallAndStart\n"
        "=== PAUSE TestL3bSmokeNginxLive_InstallAndStart\n"
        "=== RUN   TestL3bRedisLive_Day2AddUser\n"
        "=== PAUSE TestL3bRedisLive_Day2AddUser\n"
        "=== CONT  TestL3bSmokeNginxLive_InstallAndStart\n"
        "    stack.go:187: NewStack: vault: InitVaultTestSecrets: enable pki mount: "
        "Put \"http://127.0.0.1:33242/v1/sys/mounts/pki\": dial tcp 127.0.0.1:33242: "
        "connect: connection refused\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "--- FAIL: TestL3bSmokeNginxLive_InstallAndStart (4.02s)\n"
        "=== CONT  TestL3bRedisLive_Day2AddUser\n"
        "    redis_ops_adduser_live_test.go:118: ACL GETUSER alice: got \"\", want the "
        "created user\n"
        "--- FAIL: TestL3bRedisLive_Day2AddUser (288.10s)\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e-live\t292.20s\n",
    ),
    (
        "subtest output belongs to its parent's block",
        # This one guards a risk the fix above introduced rather than a defect it
        # found. TOP_SWITCH matches `Test\S+` where the old pattern stopped at
        # the slash, so subtest lines now reach the switch branch — and reporting
        # `TestX/sub` as its own test would hand main() a name the mask never
        # named, which then also makes the parent's `--- FAIL` land in an empty
        # block. Splitting on `/` is what prevents that, and this pins it.
        [("TestL3bRedisLive_Day2Restart", "STAND-SETUP")],
        "=== RUN   TestL3bRedisLive_Day2Restart\n"
        "=== RUN   TestL3bRedisLive_Day2Restart/sentinel\n"
        "=== CONT  TestL3bRedisLive_Day2Restart/sentinel\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "    --- FAIL: TestL3bRedisLive_Day2Restart/sentinel (0.51s)\n"
        "--- FAIL: TestL3bRedisLive_Day2Restart (3.94s)\n",
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
        "a stale binary is STAND-SETUP too — the verdict is the same, the advice is not",
        # NIM-490's refusal, declared like any other bring-up failure and so
        # labelled like one. The verdict is correct and stays; what must not stay
        # is "rerun it alone", which re-runs the same binary. next_step() is what
        # separates them, and check_next_step_branches() is what pins it.
        [("TestL3bSmokeNginxLive_InstallAndStart", "STAND-SETUP")],
        "=== RUN   TestL3bSmokeNginxLive_InstallAndStart\n"
        "    provenance.go:131: e2e-live: the keeper binary is STALE - an uncommitted "
        "source file is younger than it is.\n"
        "    setupdecl.go:57: @@MARKER@@ — no assertion in this test ever ran\n"
        "--- FAIL: TestL3bSmokeNginxLive_InstallAndStart (0.04s)\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e-live\t0.112s\n",
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

    # Before the fixtures, because both of these are about text no fixture can
    # reach: one is a copy of the harness's wording, the other is the sentence
    # the reader acts on.
    if why := check_stale_needle_matches_the_harness():
        print(f"classify-e2e-live-failure: FAIL stale-needle: {why}")
        bad += 1
    else:
        print(f"classify-e2e-live-failure: ok   stale-needle ({STALE_NEEDLE!r} still in "
              f"both stale paths of {PROVENANCE_SOURCE.name})")
    if why := check_next_step_branches():
        print(f"classify-e2e-live-failure: FAIL next-step: {why}")
        bad += 1
    else:
        print("classify-e2e-live-failure: ok   next-step (a stale binary is told to "
              "rebuild, everything else to rerun alone)")
    # And that main() hands the render the blob it classified. The check above
    # picks its own blob, so it cannot tell that argument from an empty one;
    # this renders a real log through the real entry point, which can.
    if why := check_report_renders_the_join():
        print(f"classify-e2e-live-failure: FAIL rendered-join: {why}")
        bad += 1
    else:
        print("classify-e2e-live-failure: ok   rendered-join (a real log rendered end "
              "to end still carries the rebuild advice)")

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

    blocks = partition(lines)
    seen = {test: classify(result, out, marker) for test, result, out in blocks}
    blob_of = {test: out for test, _result, out in blocks}
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
        for line in report_lines(test, verdict, blob_of.get(test, "")):
            print(line)

    print()
    print("  Nothing above was downgraded: the gate failed and the caller still exits")
    print("  non-zero. This says WHERE to look, never whether to care.")
    print(bar)
    return 0


if __name__ == "__main__":
    sys.exit(main())
