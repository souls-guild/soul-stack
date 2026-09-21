#!/usr/bin/env python3
"""classify-l3a-failure.py — after L3a fails, say of each failing test whether
its stand came up, which recurring shape it is, and whether anything was
asserted at all.

Why this exists (NIM-469). L3a's signature is not "a test broke": every test is
green on its own, and a full suite drops two or three of them from three
different shapes, a different test each time. Read as a list of `--- FAIL:`
lines those are indistinguishable — an assertion that caught a regression and a
Vault container that never answered print exactly the same thing — so the run
gets called flaky and rerun, and a suite nobody believes certifies nothing. R5
finishes on trust in the verification rather than on features, which makes an
illegible red L3a a release blocker in its own right.

This is the e2e-live classifier's mechanism (scripts/classify-e2e-live-failure.py,
NIM-406) one tier down, and deliberately a sibling rather than a variation. In
particular it inherits the property that matters most: **no signature lists**.
There is no "connection refused means infra" here. The harness declares its own
bring-up failures (standSetupMarker in tests/e2e/harness/setupdecl.go, read out
of that file rather than copied), so guessing infrastructure from library text
would only add a way to be wrong — and the way it is wrong is the one that
retires a real regression. The residual error is one-directional: an entry point
that forgets to declare reads as TEST-FAILURE, which costs a person one look.

That is the deliberate difference from classify-l1-failure.py, whose signature
lists exist because one of its 39 suites can still fail without declaring. Its
three-way REGRESSION / INFRA / UNCLEAR split is a consequence of that guessing —
UNCLEAR is the bucket for text either layer could print. With nothing inferred
from text there is nothing to be unclear about, so the verdicts here are the
e2e-live ones, plus one this tier needs:

  STAND-SETUP   the harness declared it. Bring-up died before the test body, so
                no assertion in that test ever ran. (L1 would say INFRA.)
  TEST-FAILURE  a failure with no declaration. The stand was up and the code was
                exercised. A finding. (L1 would say REGRESSION.)
  TIMEOUT       the binary hit -timeout and the runtime killed it. Its own
                verdict because the watchdog panics without unwinding: no defer
                runs, so the marker is PHYSICALLY IMPOSSIBLE here and its absence
                proves nothing. Calling this TEST-FAILURE sends someone to debug
                whichever test merely held the clock.
  NOT-RUN       skipped, or the binary died before it ever ran. Not a pass.

Alongside the verdict, and never overriding it, each failing test gets a FAMILY
— which of NIM-469's recurring shapes this is, so three failures in one run can
be told apart at a glance instead of by reading 600s of log. A family annotates:
it says what to read next, not whether the code is at fault.

Nothing is ever downgraded to a pass: the caller's exit code is untouched.

Usage: classify-l3a-failure.py <go-test-output-file>
       classify-l3a-failure.py --self-test
"""

import pathlib
import re
import subprocess
import sys
import tempfile

REPO_ROOT = pathlib.Path(__file__).resolve().parent.parent
MARKER_SOURCE = REPO_ROOT / "tests" / "e2e" / "harness" / "setupdecl.go"
MARKER_DECL = re.compile(r'standSetupMarker\s*=\s*"([^"]+)"')

# L1's classifier and the pattern it keys on, read rather than copied for the
# same reason the marker is: a copy would agree with itself forever.
L1_CLASSIFIER = REPO_ROOT / "scripts" / "classify-l1-failure.py"
L1_PATTERN_DECL = re.compile(r"SETUP_DECLARED\s*=\s*re\.compile\(\s*r\"([^\"]+)\"")

# NIM-490's pre-flight refuses a keeper binary that is not this tree, and it does
# so INSIDE the declared bring-up region, so its refusal arrives here as
# STAND-SETUP. The verdict is right — nothing was asserted — but the advice that
# follows it is not: "rerun this one alone" reproduces a stale binary exactly,
# forever. This needle is what lets the family say so.
PROVENANCE_SOURCE = REPO_ROOT / "tests" / "e2e" / "harness" / "provenance.go"
STALE_NEEDLE = "the keeper binary is STALE"

# Whose block the following lines belong to. Go's chatty printer moves
# attribution with three lines and only three: `=== RUN` when a test starts,
# `=== CONT` when a paused one resumes, `=== NAME` when output returns to a test
# already running. Subtests (`TestX/sub`) fold into their parent, because on this
# tier a subtest shares its parent's Stack and a verdict per subtest would split
# one stand's failure across several labels.
#
# Reading all three rather than just `=== RUN` is what keeps this correct if the
# suite ever stops being serial. `make e2e` passes `-p 1`, which bounds how many
# PACKAGES run at once and says nothing about one package's binary; what would
# interleave these is t.Parallel(), which no L3a test calls today. That is a
# property of the tests, not of the flags, and not one this tool should depend on
# anybody noticing when it changes.
#
# BUT those three lines exist only under `-v`, and `make e2e` does not pass it.
# Without -v Go prints nothing while a test runs and flushes the failed test's
# buffered output AFTER its `--- FAIL:` header, indented four spaces — the
# opposite order. Keyed on the switch lines alone, a non-verbose log moves
# attribution never: each test's block holds its own header line and nothing
# else, every marker lands in the unattributed prologue, and EVERY declared
# bring-up failure reads TEST-FAILURE. That is the precise inversion this tool
# exists to prevent, on the one format the Makefile actually produces.
#
# So a result line also claims what follows it, and gives the claim up at the
# first line back in column 0. Indentation is the discriminator because it is
# the difference itself: buffered test output is indented, and everything that
# follows a result line in a VERBOSE log — the next `=== RUN`, the `FAIL <pkg>`
# trailer, a panic banner — starts in column 0. Both formats fall out of the one
# rule, rather than the tool having to detect which it is reading.
TOP_SWITCH = re.compile(r"^=== (?:RUN|CONT|NAME)\s+(Test\S+)\s*$")
TOP_RESULT = re.compile(r"^\s*--- (PASS|FAIL|SKIP): (Test\S+)")

# The Go runtime's own words, not a library's. Both abort the process, so both
# are facts about the run rather than guesses about a layer.
TIMEOUT_PANIC = "panic: test timed out after"
LOG_AFTER_TEST = re.compile(r"(?:Log|Logf) in goroutine after (Test\S+) has completed")

# What has to appear in the unattributed text before it is worth reporting as a
# block of its own.
#
# Every run leaves SOMETHING outside a test — the `FAIL <pkg> 508.672s` trailer,
# `ok` lines for the docker-free packages, make's own banner. Reporting all of
# that as an unnamed failure would put a verdict on the package footer of every
# single run, and a tool that cries wolf on every run is one people stop reading.
#
# But dropping it outright is worse, because the three things below are the cases
# where the log's only account of what happened is written nowhere else: a
# compile error names no test at all, and a panic that kills the binary lands
# after the last test's result line whenever it fires between tests — which is
# exactly when a leftover goroutine from the test that just ended gets to run.
UNATTRIBUTED_MATTERS = (
    "panic:",
    "[build failed]",
    "[setup failed]",
)

# family -> (signature, what the label means for the reader). Order matters: the
# first match wins, so a shape that describes the RUN precedes one that describes
# a request, which it can cause.
#
# These are annotations, never verdicts. Every signature below is either the Go
# runtime's own text or a string this repo owns (a constraint name, an assert's
# wording) — never a third-party library's phrasing, which is the thing NIM-406
# established must not be guessed from.
FAMILIES: list[tuple[str, list[str], str]] = [
    (
        "LOG_AFTER_TEST",
        ["Log in goroutine after", "Logf in goroutine after"],
        "A harness goroutine wrote into a test that had already finished, and Go\n"
        "answers that by panicking the whole binary. The test named on the FAIL\n"
        "line is the bystander that happened to be running; the culprit is the\n"
        "test named INSIDE the panic, whose keeper sub-process outlived its own\n"
        "cleanup. Everything queued behind this point never ran.",
    ),
    (
        "APPLY_FK",
        ["apply_task_register_apply_run_fk", "UpsertTaskRegister:"],
        "The apply_task_register/apply_run write-ordering under load. That is\n"
        "NIM-366 and it is tracked there — record the occurrence, do not\n"
        "re-diagnose it here. Still a finding, just not a new one.",
    ),
    (
        # Only the first needle has ever done the matching, and a mutation run
        # confirmed it: deleting the other three changes no verdict, because
        # every harness failure that prints a response body prints `status %d`
        # beside it. They are kept for shapes that do not do that — not because
        # anything here pins them. Do not read their presence as evidence they
        # fire.
        "AUTH_401",
        ["status 401", '"detail":"invalid token"', "token issuer not trusted",
         "token expired"],
        "An operator call was rejected. This stack minted that token from its own\n"
        "keeper seconds earlier and ttl_bootstrap is 720h, so expiry is not the\n"
        "explanation — and `token expired` is a separate detail that would say so.\n"
        "`invalid token` means the SIGNATURE did not verify, and since every stack\n"
        "draws its own random signing key into its own Vault, that means the\n"
        "request reached ANOTHER stack's keeper — a wrong endpoint, not an auth\n"
        "regression. Do not wait for `issuer not trusted` to tell you so: the\n"
        "signature is checked before `iss`, so a foreign keeper never gets that\n"
        "far. NewStack now refuses to hand back a stack pointed at a keeper that\n"
        "is not its own (assertOwnKeeper), so a 401 surviving to here is either a\n"
        "keeper that changed identity mid-test or a real authorization defect.",
    ),
    (
        "STALE_BINARY",
        [STALE_NEEDLE],
        "Not the machine — the harness refused before bringing anything up,\n"
        "because keeper/bin/keeper does not carry the code in the tree (NIM-490).\n"
        "Docker, Vault and Postgres are not implicated and nothing here is flaky.\n"
        "The lines above name the mismatch and the fix; `make build` is the fix in\n"
        "every case, and a rerun without it reproduces this exactly.",
    ),
    (
        "STATE_SUBSET",
        ["state does not contain subset"],
        "An incarnation-state assert missed. The assert prints both sides — read\n"
        "them: `actual` EMPTY means the writes landed in a database this test is\n"
        "not reading, while `actual` populated-but-different is drift and a\n"
        "straightforward finding.",
    ),
]


def read_marker() -> str:
    """The marker, read out of the Go source that emits it.

    Not a copy. If someone rewords standSetupMarker, a copy here would keep
    matching nothing while every verdict quietly became TEST-FAILURE — the tool
    would still run, still print, and still be wrong, which is the failure mode
    this whole ticket is about.

    Reading it removes that failure mode and does NOT gate the rewording, which
    is a distinction the first version of this docstring got wrong: the fixtures
    below carry a `@@MARKER@@` placeholder filled from this same read, so both
    sides move together and a mutation that reworded the marker left the
    self-test green. What a rewording really breaks is L1 compatibility, and
    check_l1_compatibility is what holds that.
    """
    try:
        src = MARKER_SOURCE.read_text(encoding="utf-8")
    except OSError as exc:
        raise SystemExit(
            f"classify-l3a-failure: cannot read {MARKER_SOURCE}: {exc}\n"
            "The marker is defined there and is not duplicated here on purpose."
        )
    m = MARKER_DECL.search(src)
    if not m:
        raise SystemExit(
            f'classify-l3a-failure: no `standSetupMarker = "…"` in {MARKER_SOURCE}.\n'
            "Either it was renamed or the declaration is gone. Until it is back, every\n"
            "bring-up failure in an L3a log reads as an assertion failure."
        )
    return m.group(1)


def check_l1_compatibility(marker: str) -> str:
    """The marker must still be legible to L1's classifier. "" if it is.

    Two tools read the same string. This one reads it out of the Go source and
    therefore tracks any rewording; scripts/classify-l1-failure.py matches a
    fixed shape, `integration: .*setup failed`, which 38 of the 39 L1 suites
    print and which an assertion cannot produce. A rewording that leaves that
    shape costs nothing here and quietly makes every declared L3a bring-up
    failure unreadable to the other tool — an L3a log handed to it, or an L3a
    suite folded into an L1-shaped report, goes back to being a list of
    identical FAIL lines.

    So the check is here rather than there: L1 has no reason to know about this
    tier. The pattern is read out of L1's source for the same reason the marker
    is read out of the Go source — a copy would agree with itself forever, which
    is the whole defect this function exists to close.
    """
    try:
        src = L1_CLASSIFIER.read_text(encoding="utf-8")
    except OSError as exc:
        return (
            f"cannot read {L1_CLASSIFIER}: {exc}\n"
            "     The pattern lives there and is deliberately not copied here."
        )
    m = L1_PATTERN_DECL.search(src)
    if not m:
        return (
            f'no `SETUP_DECLARED = re.compile(r"…")` in {L1_CLASSIFIER.name}.\n'
            "     Either it was renamed or its shape changed. Until this reads it again,\n"
            "     nothing checks that the two tiers still declare failures the same way."
        )
    pattern = m.group(1)
    if re.search(pattern, marker, re.IGNORECASE):
        return ""
    return (
        f"the marker {marker!r} does not match L1's SETUP_DECLARED, {pattern!r}.\n"
        f"     Read from {L1_CLASSIFIER.name}, not copied. This tool would still be right —\n"
        "     it reads the constant — but classify-l1-failure.py would stop recognising a\n"
        "     declared L3a bring-up failure and label it a test failure instead.\n"
        "     Keep the `<suite> integration: … setup failed` shape, or change both tiers."
    )


def check_marker_came_from_the_source(marker: str) -> str:
    """The returned marker really is the constant in setupdecl.go. "" if it is.

    NIM-550. read_marker() raises SystemExit when the file will not read or the
    declaration will not match — but both of those sit AFTER the point an early
    `return` would leave from, and nothing compared the RESULT against the file.
    A mutant returned a literal, skipped MARKER_DECL entirely, and reworded the
    Go constant in the same move: check_l1_compatibility saw a literal that still
    matched L1's shape, the fixtures filled @@MARKER@@ from the same literal and
    agreed with themselves, and the tool went green while the constant it claims
    to read had moved. "Nothing here is inferred from someone else's wording" is
    this tool's central claim, and it was the one claim not being checked.

    Deliberately NOT via MARKER_DECL: re-running the same regex would only prove
    the regex is deterministic. This scans lines and splits on quotes, so the two
    readings share nothing but the file.

    It also insists the file is the one that PRINTS the marker. `MARKER_SOURCE`
    pointed at a path constant, and a path constant can be moved to a file that
    happens to carry a similar declaration; requiring declareStandSetupFailure in
    the same file ties the constant to its emitter rather than to a name.
    """
    try:
        lines = MARKER_SOURCE.read_text(encoding="utf-8").splitlines()
    except OSError as exc:
        return f"cannot read {MARKER_SOURCE}: {exc}"

    found = [
        ln.split('"')[1]
        for ln in lines
        if ln.split()[:2] == ["const", "standSetupMarker"] and ln.count('"') >= 2
    ]
    if len(found) != 1:
        return (
            f"expected exactly one `const standSetupMarker = \"…\"` in "
            f"{MARKER_SOURCE.name}, found {len(found)}.\n"
            "     Read by a plain line scan, not by MARKER_DECL — the two must agree,\n"
            "     and if there is no single declaration they cannot."
        )
    if found[0] != marker:
        return (
            f"read_marker() returned {marker!r}, but {MARKER_SOURCE.name} declares "
            f"{found[0]!r}.\n"
            "     The marker is supposed to come OUT of the product, not out of this\n"
            "     file. Whatever is returning that string, it is not reading the constant,\n"
            "     and every verdict this tool prints now rests on a copy that agrees with\n"
            "     itself."
        )
    if not any("func declareStandSetupFailure" in ln for ln in lines):
        return (
            f"{MARKER_SOURCE.name} declares the marker but does not define\n"
            "     declareStandSetupFailure. MARKER_SOURCE has been pointed at a file that\n"
            "     merely carries a similar constant; the string that reaches the log comes\n"
            "     from somewhere else and is no longer being read at all."
        )
    return ""


def next_step(test: str, family: str | None) -> tuple[str, str]:
    """The line a reader actually acts on: headline, then the command to run.

    A pure function for the same reason classify() is one — this is the only
    output of the whole tool that tells someone to do something, and getting it
    wrong costs more than a mislabelled verdict does. Inline in the report loop it
    was reachable only by rendering a log, so the fixtures could not see it and a
    branch could be deleted without one of them going red.

    STALE_BINARY is the case that forces the branch. Its verdict is STAND-SETUP,
    and everything STAND-SETUP normally means — the machine, contention, try it
    alone — is false here: the binary is from another build and a rerun without
    `make build` reproduces the refusal identically, every time, which is the one
    shape where the generic advice sends a reader into a loop.
    """
    if family == "STALE_BINARY":
        return (
            "Rebuild FIRST — rerunning alone changes nothing:",
            f"make build && cd tests/e2e && go test -tags=e2e -count=1 -run '^{test}$' -v .",
        )
    return (
        "Rerun this one alone (no company, no contention):",
        f"cd tests/e2e && go test -tags=e2e -count=1 -run '^{test}$' -v .",
    )


def check_stale_needle_matches_the_harness() -> str:
    """STALE_NEEDLE still appears in the message provenance.go emits. "" if it does.

    The marker is READ out of the Go source; this needle cannot be, because it is
    a fragment of a format string rather than a constant, and hoisting it into one
    would put the same fragment in three tiers' provenance.go for one tier's
    classifier. So it is a copy — and an unchecked copy is precisely the failure
    this whole tool is built against: reword the Go message and the family stops
    firing, in silence, leaving the wrong advice back in place with no test red.

    Both stale paths are required to carry it, not just one. They are two separate
    format strings (version mismatch, uncommitted-source freshness) and the
    freshness one is the shape a developer mid-edit actually hits; a needle
    matching only the other would cover the rarer half and read as covering both.
    """
    try:
        src = PROVENANCE_SOURCE.read_text(encoding="utf-8")
    except OSError as exc:
        return f"cannot read {PROVENANCE_SOURCE}: {exc}"

    hits = src.count(STALE_NEEDLE)
    if hits < 2:
        return (
            f"{STALE_NEEDLE!r} appears {hits}x in {PROVENANCE_SOURCE.name}, want both\n"
            "     stale paths (version mismatch AND uncommitted-source freshness).\n"
            "     The STALE_BINARY family keys on this string; where it does not appear,\n"
            "     a refusal is reported as a bring-up failure and the reader is told to\n"
            "     rerun the test alone, which reproduces a stale binary forever."
        )
    return ""


def check_next_step_branches() -> str:
    """next_step() still distinguishes a stale binary from everything else. "" if it does.

    The verdict fixtures compare (test, verdict, family) and stop there, so they
    would stay green with this branch collapsed to its generic half — and that
    collapse is invisible in review, because the remaining line reads perfectly
    well. What it says is just wrong for the one family that reaches it: the
    rebuild is the fix, and a bare rerun re-runs the same binary.

    Both directions are asserted. Only checking the stale branch would pass a
    next_step() that prescribed `make build` for every failure, which quietly
    doubles the cost of the flaky-container case this tool exists to triage.
    """
    stale_head, stale_cmd = next_step("TestSomething", "STALE_BINARY")
    plain_head, plain_cmd = next_step("TestSomething", None)

    if "make build" not in stale_cmd:
        return (
            f"STALE_BINARY is told {stale_cmd!r}, which does not rebuild.\n"
            "     That command re-runs the very binary the harness just refused, so the\n"
            "     reader gets the identical failure and no new information."
        )
    if "make build" in plain_cmd:
        return (
            f"a non-stale failure is told {plain_cmd!r}.\n"
            "     A rebuild is not the fix for a container flake; prescribing it for every\n"
            "     family makes the advice noise and hides the case where it is the point."
        )
    if stale_head == plain_head:
        return (
            f"both families get the same headline {stale_head!r}.\n"
            "     The commands differ but nothing says why, and the headline is the part\n"
            "     that gets read."
        )
    for name, cmd in (("STALE_BINARY", stale_cmd), ("generic", plain_cmd)):
        if "TestSomething" not in cmd:
            return f"the {name} command does not name the test: {cmd!r}"
    return ""


def check_next_step_is_joined() -> str:
    """The report reaches next_step() with the classified family. "" if it does.

    A different failure from the one above, and the one that survives it. That
    check proves next_step() branches correctly; this proves the branch is
    reached with the right argument. A perfectly-branching pure function called
    as next_step(test, verdict) — or with a family variable from an outer scope,
    or with None — is green on every unit assertion and wrong in every report,
    and nothing else here would notice: the verdict fixtures stop at
    (test, verdict, family) and never render the advice.

    L3b's classifier extracts the whole report into a pure report_lines() a
    fixture can call. That refactor is right there and wrong here: this render
    loop carries a dozen family-specific branches, and moving it wholesale to buy
    one assertion would be a large edit with its own risk in a ticket about not
    trusting things you have not examined. So the SHAPE of the join is pinned
    where it lives, by reading this file's own AST.

    Note what that buys and what it does not. An identifier is not a value: this
    check sees `next_step(test, family)` and is satisfied, so
    `family = family_of("")` one line up passes it while emptying every report
    printed. Extracting report_lines() does not close that either — it lifts the
    join one level, from "which family" to "which blob", and L3b's fixtures call
    report_lines() directly, so the same hole is open there. Only rendering a
    real log through the real entry point closes it, in either script, which is
    what check_report_renders_the_join does below.
    """
    import ast

    src = pathlib.Path(__file__).read_text(encoding="utf-8")
    tree = ast.parse(src)

    main_fn = next((n for n in ast.walk(tree)
                    if isinstance(n, ast.FunctionDef) and n.name == "main"), None)
    if main_fn is None:
        return ("there is no main() in this file to inspect. If the report moved, move "
                "this check with it — a guard that cannot find its subject gates nothing.")

    calls = [n for n in ast.walk(main_fn)
             if isinstance(n, ast.Call)
             and isinstance(n.func, ast.Name) and n.func.id == "next_step"]
    if not calls:
        return ("main() never calls next_step(). Either the advice is no longer printed, "
                "or it moved out of main() and this check now walks a function that does "
                "not contain it — which passes by vacuum.")

    for call in calls:
        if len(call.args) != 2:
            return (f"next_step() is called with {len(call.args)} positional argument(s); "
                    "this check knows the (test, family) shape and cannot vouch for another.")
        second = call.args[1]
        if not isinstance(second, ast.Name) or second.id != "family":
            got = ast.unparse(second)
            return (f"next_step() is passed {got!r} as its family. The function branches on "
                    "STALE_BINARY, so anything but the classified family sends every reader "
                    "the generic advice — green on every assertion above, wrong in every "
                    "report printed.")
    return ""


def check_report_renders_the_join() -> str:
    """A real log through the real entry point still carries family and fix. "" if it does.

    Every other check in this file calls one function directly with arguments it
    chose itself. That is what leaves the join uncovered, and the join is its own
    defect class: a pure function reached with an empty or adjacent argument
    returns confidently, and the caller prints the confident answer. The AST
    check above pins the join's shape; this pins its VALUE, which is the half an
    identifier cannot vouch for.

    The known-bad is not hypothetical. `family_of(out)` -> `family_of("")` in
    main() leaves the identifier named `family`, satisfies the AST check, keeps
    all twenty-odd fixtures green, and deletes `family: STALE_BINARY` and the
    rebuild advice from every report the tool will ever print — which is the
    whole of what NIM-490 added here. A subprocess is the price of seeing it:
    calling main() in-process would need stdout capture and would still share
    this module's globals, so it would not exercise argv, the file read, or
    read_marker() the way CI does.

    The headlines are asked of next_step() rather than written out here, and the
    difference is which way this check is allowed to fail. Rewording a headline
    must not redden it — the wording is not the property. The two printed lines
    going missing must, and nothing else here would notice: `make build` also
    appears in the family paragraph above them, so a report that computes the
    advice and never prints it still carries that needle.
    """
    script = pathlib.Path(__file__).resolve()
    marker = read_marker()
    declared = f"    panic.go:694: {marker} — the stand's infrastructure never came up."
    stale_head, _ = next_step("TestSomething", "STALE_BINARY")
    plain_head, _ = next_step("TestSomething", None)
    cases = (
        (
            "stale binary",
            "--- FAIL: TestSomething (0.22s)\n"
            f"    stack.go:181: L3a: {STALE_NEEDLE} - this run would test code that is "
            "not in the tree.\n" + declared + "\n",
            ("family: STALE_BINARY", "make build", stale_head),
            (plain_head,),
        ),
        (
            "ordinary bring-up failure",
            "--- FAIL: TestSomething (61.35s)\n"
            "    stack.go:187: NewStack: postgres: postgres container: run postgres: "
            "context deadline exceeded\n" + declared + "\n",
            (plain_head,),
            ("family: STALE_BINARY", "make build", stale_head),
        ),
    )

    for name, blob, want, unwanted in cases:
        with tempfile.TemporaryDirectory() as tmp:
            log = pathlib.Path(tmp) / "l3a.log"
            log.write_text(blob, encoding="utf-8")
            proc = subprocess.run(
                [sys.executable, str(script), str(log)],
                capture_output=True, text=True,
            )
        report = proc.stdout
        indented = "\n".join(f"       {line}" for line in report.splitlines())
        for needle in want:
            if needle not in report:
                return (f"the report rendered for a {name} does not contain {needle!r}.\n"
                        "     next_step() and family_of() can both be correct and this still\n"
                        "     fail: what is broken is what main() hands them, and no other\n"
                        f"     check here renders a log at all.\n{indented}")
        for needle in unwanted:
            if needle in report:
                return (f"the report rendered for an {name} contains {needle!r}, so the\n"
                        "     stale-binary advice is reaching blocks that are not stale — a\n"
                        "     reader is told to rebuild when rebuilding will not help.\n"
                        f"{indented}")
    return ""


def partition(lines: list[str]) -> list[tuple[str, str, str]]:
    """(test name, result, its output) for each top-level test in the log.

    `result` is PASS/FAIL/SKIP, or "" when the block never closed — what a
    timeout or a panic mid-test leaves behind.

    One buffer per test rather than one buffer for "the current test", so a log
    that switches back and forth still gives each test its own lines. A result
    line is attributed by the name ON IT, not by whichever block was open,
    because those are the same thing only when nothing interleaves.

    Output before the first `=== RUN` — the panic banner a crashed binary prints,
    testcontainers noise from a stand that died early — is kept under a synthetic
    `<run>` entry rather than dropped: on this tier that prologue is frequently
    the only place the real cause is written.
    """
    blocks: dict[str, list[str]] = {"<run>": []}
    results: dict[str, str] = {}
    name = "<run>"
    # A result line was just seen and nothing has taken attribution back yet, so
    # indented lines are this test's buffered output (the non-verbose shape).
    trailing = False

    for line in lines:
        switch = TOP_SWITCH.match(line)
        if switch:
            name, trailing = switch.group(1).split("/", 1)[0], False
            blocks.setdefault(name, [])
            continue
        res = TOP_RESULT.match(line)
        if res:
            top = res.group(2).split("/", 1)[0]
            blocks.setdefault(top, []).append(line)
            # A subtest's own `--- FAIL:` does not close its parent, and must not
            # overwrite a result the parent already reported — nor claim what
            # follows, which is the parent's either way.
            if top == res.group(2):
                results[top] = res.group(1)
                name, trailing = top, True
            continue
        if trailing and not line.startswith(" "):
            # Back in column 0: the buffered output is over. In a verbose log
            # this fires immediately — on the trailer, or on the panic banner of
            # a goroutine that outlived the test that just passed, which is
            # nobody's block and has to stay out of it.
            name, trailing = "<run>", False
        blocks[name].append(line)

    out = [(t, results.get(t, ""), "\n".join(buf)) for t, buf in blocks.items() if t != "<run>"]
    unattributed = "\n".join(blocks["<run>"])
    if any(s in unattributed for s in UNATTRIBUTED_MATTERS):
        out.append(("<run>", "", unattributed))
    return out


def classify(result: str, blob: str, marker: str) -> str:
    """The verdict for one test's block. Shared by main() and --self-test so the
    guard cannot drift away from what the tool actually does."""
    if result == "PASS":
        return "PASS"
    if result == "SKIP":
        return "NOT-RUN"
    if TIMEOUT_PANIC in blob:
        # Checked BEFORE the marker, and it has to be: the watchdog panics
        # without unwinding, so no defer ran and no declaration could have been
        # printed. Reading "no marker" as "the stand was fine" is exactly the
        # inference this verdict exists to prevent.
        return "TIMEOUT"
    if marker in blob:
        return "STAND-SETUP"
    if result == "FAIL":
        return "TEST-FAILURE"
    # Started and never finished: a panic elsewhere took the binary down. Nothing
    # says which layer died, and the safe reading is the one that keeps a person
    # looking.
    return "TEST-FAILURE"


def family_of(blob: str) -> tuple[str, str] | tuple[None, None]:
    """Which recurring shape this is, or (None, None). Annotation only — the
    verdict is decided by classify() and this never touches it."""
    for name, signatures, detail in FAMILIES:
        if any(s in blob for s in signatures):
            return name, detail
    return None, None


# Fixtures for --self-test, each an excerpt of a real L3a failure shape. They
# exist because this logic is the one piece of the ticket `make test` cannot see
# — it is not Go — and a classifier that is quietly wrong is strictly worse than
# none: it puts a confident label on the guess someone was going to make anyway.
#
# `@@MARKER@@` is substituted with the constant read from setupdecl.go, which
# keeps the fixtures honest about the PARSING — they exercise whatever the
# harness actually prints — but is tautological about the WORDING, and cannot
# catch a reworded marker. check_l1_compatibility covers that.
SELF_TEST: list[tuple[str, list[tuple[str, str, str | None]], str]] = [
    (
        "NON-VERBOSE (what `make e2e` prints): declared bring-up -> STAND-SETUP",
        # Verbatim from an NIM-469 run, and the case that exposed the bug this
        # fixture now pins. `make e2e` runs `go test` with no `-v`, so there is
        # not one `=== RUN` in the whole log and each test's output comes AFTER
        # its own header. Every other fixture here was verbose, which is how a
        # 16-case self-test stayed green while the tool inverted the verdict on
        # every real log it was ever pointed at: both of these say the marker in
        # plain text and both were reported "the stand was up, this is a finding
        # about the code".
        [
            ("TestE2EServiceCovenProbe_Create", "STAND-SETUP", None),
            ("TestE2EServiceRedis_CreateCluster", "STAND-SETUP", None),
        ],
        "--- FAIL: TestE2EServiceCovenProbe_Create (61.35s)\n"
        "    coven_probe_test.go:18: NewStack: postgres: postgres container: run postgres: "
        "generic container: create container: reaper: new reaper: run container: started hook: "
        "wait until ready: external check: check target: retries: 523 address: localhost:32954: "
        'get state: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.54/containers/dc7e7019b783/json": '
        "context deadline exceeded: could not start container\n"
        "    panic.go:694: @@MARKER@@ — the stand's infrastructure never came up, so nothing "
        "above is a finding about the code.\n"
        "--- FAIL: TestE2EServiceRedis_CreateCluster (14.70s)\n"
        "    redis_cluster_test.go:43: NewStack: redis: redis container: run redis: "
        "generic container: start container: started hook: wait until ready: external check: "
        "check target: retries: 84 address: localhost:32996: get state: context deadline exceeded\n"
        "    panic.go:694: @@MARKER@@ — the stand's infrastructure never came up, so nothing "
        "above is a finding about the code.\n"
        "FAIL\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e\t574.986s\n"
        "ok  \tgithub.com/souls-guild/soul-stack/tests/e2e/harness\t0.046s\n"
        "FAIL\n",
    ),
    (
        "NON-VERBOSE: consecutive blocks do not bleed into each other",
        # The other half. With output attributed by the header above it, the risk
        # moves from "the marker reaches nobody" to "the marker reaches everybody
        # after it" — which would label the genuine regression below STAND-SETUP
        # and retire it. The boundary is the next header line, so this pins two
        # adjacent blocks landing on opposite verdicts.
        [
            ("TestHelloWorld_CreateIncarnation", "STAND-SETUP", None),
            ("TestScenarioApply_StateChanges", "TEST-FAILURE", "STATE_SUBSET"),
        ],
        "--- FAIL: TestHelloWorld_CreateIncarnation (60.41s)\n"
        "    stack.go:159: NewStack: postgres: postgres container: context deadline exceeded\n"
        "    panic.go:694: @@MARKER@@ — the stand's infrastructure never came up\n"
        "--- FAIL: TestScenarioApply_StateChanges (31.08s)\n"
        "    asserts.go:145: AssertIncarnationState test-hello: state does not contain subset\n"
        "        actual=map[]\n"
        "        expected_subset=map[replicas:2]\n"
        "FAIL\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e\t520.10s\n",
    ),
    (
        "declared bring-up failure -> STAND-SETUP",
        [("TestSmokeNginx_Apply", "STAND-SETUP", None)],
        "=== RUN   TestSmokeNginx_Apply\n"
        "    stack.go:166: NewStack: vault: vault container: could not start container\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "--- FAIL: TestSmokeNginx_Apply (4.02s)\n",
    ),
    (
        "assertion failure, no declaration -> TEST-FAILURE",
        [("TestCovenProbe_Where", "TEST-FAILURE", None)],
        "=== RUN   TestCovenProbe_Where\n"
        "    coven_probe_test.go:88: probe selected 3 hosts, want 2\n"
        "--- FAIL: TestCovenProbe_Where (18.31s)\n",
    ),
    (
        "one stand failure and one real regression in the SAME run -> both, separately",
        # The case a per-package verdict gets wrong. Whichever single label it
        # picked here, it would speak for the other test too — and this is the
        # ordinary L3a run, not an exotic one: two or three failures of different
        # shapes is the ticket's own description of the symptom.
        [
            ("TestHelloWorld_CreateIncarnation", "STAND-SETUP", None),
            ("TestScenarioApply_StateChanges", "TEST-FAILURE", "STATE_SUBSET"),
        ],
        "=== RUN   TestHelloWorld_CreateIncarnation\n"
        "    stack.go:159: NewStack: postgres: postgres container: context deadline exceeded\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "--- FAIL: TestHelloWorld_CreateIncarnation (60.41s)\n"
        "=== RUN   TestScenarioApply_StateChanges\n"
        "    asserts.go:145: AssertIncarnationState test-hello: state does not contain subset\n"
        "        actual=map[]\n"
        "        expected_subset=map[replicas:2]\n"
        "--- FAIL: TestScenarioApply_StateChanges (31.08s)\n",
    ),
    (
        "interleaved output -> each label follows its own test, not the open block",
        # Pinned because getting it wrong is not a near-miss — it INVERTS both
        # labels. Keyed on `=== RUN` alone, everything after the second RUN lands
        # on the second test, marker included: the tool would print TEST-FAILURE
        # on the bring-up failure (a person hunts a defect that is not there) and
        # STAND-SETUP on the real regression (a person reruns and moves on). The
        # second is how a regression gets retired by a tool built to prevent it.
        [
            ("TestSmokeNginx_Apply", "STAND-SETUP", None),
            ("TestVoyageReclaim_AfterCrash", "TEST-FAILURE", None),
        ],
        "=== RUN   TestSmokeNginx_Apply\n"
        "=== PAUSE TestSmokeNginx_Apply\n"
        "=== RUN   TestVoyageReclaim_AfterCrash\n"
        "=== PAUSE TestVoyageReclaim_AfterCrash\n"
        "=== CONT  TestSmokeNginx_Apply\n"
        "    stack.go:166: NewStack: vault: vault container: wait for reaper\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "--- FAIL: TestSmokeNginx_Apply (4.02s)\n"
        "=== CONT  TestVoyageReclaim_AfterCrash\n"
        "    voyage_reclaim_test.go:141: claimed_by_kid still keeper-mk-01 after 30s\n"
        "--- FAIL: TestVoyageReclaim_AfterCrash (44.90s)\n",
    ),
    (
        "subtest output belongs to its parent -> the name is the parent's",
        # TOP_SWITCH matches `Test\S+`, so subtest lines reach the switch branch;
        # reporting `TestX/sub` as its own test would invent a name and leave the
        # parent's block empty. Splitting on `/` prevents that.
        #
        # NIM-550: this fixture used to be NAMED for the second half of the
        # invariant — "and its FAIL line does not close it" — and did not check
        # it. A mutant removed `if top == res.group(2)` from partition() and this
        # stayed green, because nothing here distinguishes the two behaviours.
        # That is worse than a gap: the next reader takes the name for coverage.
        # The half it does hold is above; the half it did not is below.
        [("TestRedisCluster_Day2", "STAND-SETUP", None)],
        "=== RUN   TestRedisCluster_Day2\n"
        "=== RUN   TestRedisCluster_Day2/add_user\n"
        "=== CONT  TestRedisCluster_Day2/add_user\n"
        "    --- FAIL: TestRedisCluster_Day2/add_user (0.51s)\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "--- FAIL: TestRedisCluster_Day2 (3.94s)\n",
    ),
    (
        "a subtest's FAIL line does not close its parent's block",
        # NIM-550, and the layout the fixture above was named for. A subtest fails
        # EARLY, the parent keeps running, and the parent's own declaration is
        # printed afterwards.
        #
        # The load-bearing line is the unindented testcontainers one in the
        # middle. `--- FAIL: TestX/sub` must leave attribution alone; if it takes
        # it — setting the parent as the current test AND arming the
        # trailing-output mode — then the very next column-0 line reads as "the
        # buffered output is over", the block is cut short there, and the marker
        # after it lands in nobody's block. The verdict flips to TEST-FAILURE:
        # a bring-up failure presented as a defect in the code, which is the one
        # direction this tool must never be wrong in.
        #
        # testcontainers logs to stderr at column 0 in every L3a run, so this is
        # the ordinary shape of a test that tears a container down mid-run, not a
        # constructed one.
        [("TestRedisCluster_Day2", "STAND-SETUP", None)],
        "=== RUN   TestRedisCluster_Day2\n"
        "=== RUN   TestRedisCluster_Day2/add_user\n"
        "    --- FAIL: TestRedisCluster_Day2/add_user (0.51s)\n"
        "        redis_day2_test.go:88: apply: rpc error: code = Unavailable\n"
        "2026/08/07 12:04:11 🐳 Terminating container: 4f1c9ab0c2de\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "--- FAIL: TestRedisCluster_Day2 (3.94s)\n",
    ),
    (
        "container text WITHOUT a declaration stays TEST-FAILURE",
        # Isolates the marker from any temptation to sniff library text: this
        # carries testcontainers' own wording verbatim and is still not
        # STAND-SETUP, because nothing declared it. A container that dies mid-test
        # — during a keeper restart, say — is a finding about the code that killed
        # it.
        [("TestKeeperRestart_ResumesApply", "TEST-FAILURE", None)],
        "=== RUN   TestKeeperRestart_ResumesApply\n"
        "    keeper_restart_test.go:96: post-restart ping: could not start container: "
        "Error response from daemon: driver failed\n"
        "--- FAIL: TestKeeperRestart_ResumesApply (74.10s)\n",
    ),
    (
        "`connection refused` mid-test WITHOUT a declaration stays TEST-FAILURE",
        # The signature it is most tempting to shortcut on, pinned as forbidden.
        # A keeper that died during an apply prints exactly this, and there the
        # code WAS exercised — that is the defect, not the weather.
        [("TestErrandShell_ConsoleGate", "TEST-FAILURE", None)],
        "=== RUN   TestErrandShell_ConsoleGate\n"
        "    errand_shell_test.go:204: POST /v1/errands: dial tcp 127.0.0.1:39117: "
        "connect: connection refused\n"
        "--- FAIL: TestErrandShell_ConsoleGate (31.77s)\n",
    ),
    (
        "suite cut off -> TIMEOUT, and the marker's absence proves nothing",
        # The watchdog panic runs no defers, so no declaration could exist here.
        # Folding this into TEST-FAILURE would send someone to debug whichever
        # test merely held the clock — and this is not hypothetical: `make e2e`
        # carried a 10m budget against a ~666s run, so the tier was being killed
        # mid-flight and the victim rotated with the ordering.
        [("TestStagedFailover_Promote", "TIMEOUT", None)],
        "=== RUN   TestStagedFailover_Promote\n"
        "panic: test timed out after 10m0s\n"
        "\trunning tests:\n"
        "\t\tTestStagedFailover_Promote (2m14s)\n",
    ),
    (
        "marker AND timeout in one block -> TIMEOUT wins, the declaration does not",
        # NIM-550. classify() checks TIMEOUT_PANIC before the marker and the
        # comment there explains why, but until this fixture no case combined the
        # two — a mutant swapped the branches and all eighteen fixtures stayed
        # green, because each of them had only one of the two signals. An argument
        # that lives only in prose is not a guard.
        #
        # The combination is not contrived; it is NIM-533's own signature. The
        # stand fails, the deferred declaration prints, and then teardown WEDGES
        # against a daemon that has stopped answering `docker inspect` — so the
        # test never returns and the watchdog takes the process down with the
        # marker already in the block.
        #
        # TIMEOUT is the right verdict because of what each label tells the reader
        # to do next. STAND-SETUP says "a fact about the machine, rerun this test
        # alone"; the bigger fact here is that every test after this one never ran
        # at all, and the run certifies nothing. Reading the marker first would
        # hide a truncated suite behind a per-test infrastructure note.
        [("TestRedisCluster_Day2", "TIMEOUT", None)],
        "=== RUN   TestRedisCluster_Day2\n"
        "    stack.go:166: NewStack: redis: redis container: "
        "context deadline exceeded\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "    stack.go:298: [teardown] postgres container did not terminate: "
        "context deadline exceeded\n"
        "panic: test timed out after 30m0s\n"
        "\trunning tests:\n"
        "\t\tTestRedisCluster_Day2 (29m58s)\n",
    ),
    (
        "the same block WITHOUT the timeout -> STAND-SETUP (the control for the pair above)",
        # Same declaration, same wedged teardown, no watchdog. Its only job is to
        # make the fixture above about the ORDER of the two checks rather than
        # about the marker being ignored: if the marker branch were dead, this one
        # goes red and that one does not.
        [("TestRedisCluster_Day2", "STAND-SETUP", None)],
        "=== RUN   TestRedisCluster_Day2\n"
        "    stack.go:166: NewStack: redis: redis container: "
        "context deadline exceeded\n"
        "    setupdecl.go:92: @@MARKER@@ — the stand's infrastructure never came up\n"
        "    stack.go:298: [teardown] postgres container did not terminate: "
        "context deadline exceeded\n"
        "--- FAIL: TestRedisCluster_Day2 (124.03s)\n",
    ),
    (
        "log-after-test panic mid-test -> the open test is the bystander, the panic names the culprit",
        # The shape this actually takes, which is worth being precise about. The
        # write comes from the keeper subprocess's io.Copy goroutine, NOT from the
        # test's own goroutine, so tRunner never recovers it and no `--- FAIL:`
        # line is printed for anybody. The test that happened to be open when a
        # PREVIOUS test's leftover goroutine fired is left unfinished, and the
        # only name in the log that means anything is the one inside the panic.
        [("TestNoop_Apply", "TEST-FAILURE", "LOG_AFTER_TEST")],
        "=== RUN   TestNoop_Apply\n"
        "panic: Log in goroutine after TestLongRunner_Apply has completed: "
        "[keeper-stderr] shutting down\n"
        "\ngoroutine 219 [running]:\n"
        "testing.(*common).logDepth(0xc000183a00, {0xc0004a2000, 0x2b}, 0x3)\n",
    ),
    (
        "log-after-test panic BETWEEN tests -> the culprit's own result line still says PASS",
        # The nastier variant, and the reason unattributed text is kept at all.
        # The goroutine fires after its own test's result line has printed, so the
        # panic belongs to no open block — and the test named in it is recorded
        # PASS, truthfully: it passed, and then killed the binary. Drop the
        # unattributed text and the run's only explanation of why 30 tests never
        # ran disappears with it.
        [
            ("TestLongRunner_Apply", "PASS", None),
            ("<run>", "TEST-FAILURE", "LOG_AFTER_TEST"),
        ],
        "=== RUN   TestLongRunner_Apply\n"
        "--- PASS: TestLongRunner_Apply (30.02s)\n"
        "panic: Log in goroutine after TestLongRunner_Apply has completed: "
        "[keeper-stderr] shutting down\n",
    ),
    (
        "the ordinary package trailer is NOT a finding",
        # The other half of keeping unattributed text: `FAIL <pkg> 508.672s` and
        # the `ok` lines are printed by every run, including this one. Report them
        # and every single L3a failure grows a second, unnamed verdict — and a
        # tool that flags something on every run is one people stop reading.
        [("TestCovenProbe_Where", "TEST-FAILURE", None)],
        "=== RUN   TestCovenProbe_Where\n"
        "    coven_probe_test.go:88: probe selected 3 hosts, want 2\n"
        "--- FAIL: TestCovenProbe_Where (18.31s)\n"
        "FAIL\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e\t508.672s\n"
        "ok  \tgithub.com/souls-guild/soul-stack/tests/e2e/harness\t0.026s\n"
        "?   \tgithub.com/souls-guild/soul-stack/tests/e2e/internal/soulstub\t[no test files]\n"
        "FAIL\n",
    ),
    (
        "known FK ordering -> TEST-FAILURE / APPLY_FK, a finding but not a fresh one",
        # Isolates the family axis from the verdict axis. Without the family this
        # reads as an unlabelled TEST-FAILURE and someone re-diagnoses a ticket
        # that already exists; with a family that could set the verdict, a known
        # shape would be a way to downgrade a red run.
        [("TestScenarioApply_Drift", "TEST-FAILURE", "APPLY_FK")],
        "=== RUN   TestScenarioApply_Drift\n"
        "    scenario_apply_test.go:236: UpsertTaskRegister: FK violation on "
        "apply_task_register_apply_run_fk (SQLSTATE 23503)\n"
        "--- FAIL: TestScenarioApply_Drift (80.60s)\n",
    ),
    (
        # Verbatim from the first run recorded in NIM-469. The generic detail is
        # the shape this family actually arrives in — an earlier fixture here
        # invented `token issuer not trusted`, which a foreign keeper cannot
        # produce.
        "a 401 is a finding, and the generic detail is the shape really seen",
        [("TestE2EServiceRedis_Create", "TEST-FAILURE", "AUTH_401")],
        "=== RUN   TestE2EServiceRedis_Create\n"
        '    git.go:73: RegisterService redis: status 401, '
        'body={"status":401,"detail":"invalid token"}\n'
        "--- FAIL: TestE2EServiceRedis_Create (12.44s)\n",
    ),
    (
        "a skip is NOT-RUN, never a pass",
        # NewStack skips when the keeper binary is missing, and a skip that reads
        # as green is how a tier reports success for work it never did.
        [("TestSmokeNginx_Apply", "NOT-RUN", None)],
        "=== RUN   TestSmokeNginx_Apply\n"
        "    stack.go:144: L3a: keeper binary not found; export KEEPER_BIN or run `make build`\n"
        "--- SKIP: TestSmokeNginx_Apply (0.00s)\n",
    ),
    (
        "a pass stays a pass",
        [("TestSmokeNginx_Apply", "PASS", None)],
        "=== RUN   TestSmokeNginx_Apply\n"
        "--- PASS: TestSmokeNginx_Apply (61.20s)\n",
    ),
    (
        "a build failure names no test -> the prologue is kept, not dropped",
        # `go test` prints a compile error with no `=== RUN` anywhere. Reporting
        # nothing would be the worst answer available: the log says plainly what
        # happened and the tool would stay silent about a run in which no test
        # existed to fail.
        [("<run>", "TEST-FAILURE", None)],
        "# github.com/souls-guild/soul-stack/tests/e2e [build failed]\n"
        "tests/e2e/harness/stack.go:214:2: undefined: declareStandSetupFailure\n"
        "FAIL\tgithub.com/souls-guild/soul-stack/tests/e2e [build failed]\n",
    ),
    (
        "a stale binary is STAND-SETUP, but family STALE_BINARY -> rebuild, not rerun",
        # Verbatim from an NIM-490 run: a real uncommitted edit to
        # keeper/internal/errand/soulcompat.go, no rebuild, one real L3a test. The
        # pre-flight sits inside the declared region, so STAND-SETUP is the right
        # verdict — nothing was asserted. The family is what stops the reader
        # acting on the generic advice that follows it, which here would send them
        # to look at docker and then rerun a binary that cannot pass.
        [("TestE2EServiceNoop_Create", "STAND-SETUP", "STALE_BINARY")],
        "=== RUN   TestE2EServiceNoop_Create\n"
        "    noop_test.go:17: L3a: the keeper binary is STALE - an uncommitted source "
        "file is younger than it is.\n"
        "          binary:        /repo/keeper/bin/keeper  (built 2026-08-09T01:24:59-03:00)\n"
        "          newer source:  keeper/internal/errand/soulcompat.go  "
        "(edited 2026-08-09T01:30:12-03:00)\n"
        "        Rebuild and re-run: `make build`.\n"
        "    panic.go:694: @@MARKER@@ — the stand's infrastructure never came up, so "
        "nothing above is a finding about the code.\n"
        "--- FAIL: TestE2EServiceNoop_Create (0.05s)\n",
    ),
]


def self_test() -> int:
    marker = read_marker()
    print(f"classify-l3a-failure: marker read from {MARKER_SOURCE.name}: {marker!r}")
    bad = 0

    # First, because everything below trusts this string. The fixtures fill
    # @@MARKER@@ from the same read, so they agree with a literal just as
    # happily as with the constant; check_l1_compatibility only compares it
    # against L1's pattern, which a stale literal can still satisfy.
    if why := check_marker_came_from_the_source(marker):
        print(f"classify-l3a-failure: FAIL marker-provenance: {why}")
        bad += 1
    else:
        print("classify-l3a-failure: ok   marker-provenance "
              "(the string really is setupdecl.go's constant, read twice, two ways)")

    # Before the fixtures, because they cannot see this: they fill @@MARKER@@
    # from the same read and so agree with any wording at all.
    if why := check_l1_compatibility(marker):
        print(f"classify-l3a-failure: FAIL l1-compatibility: {why}")
        bad += 1
    else:
        print("classify-l3a-failure: ok   l1-compatibility "
              "(classify-l1-failure.py still recognises this marker)")

    # Also before the fixtures: the STALE_BINARY fixture below carries the needle
    # verbatim, so it agrees with a reworded harness just as happily.
    if why := check_stale_needle_matches_the_harness():
        print(f"classify-l3a-failure: FAIL stale-needle: {why}")
        bad += 1
    else:
        print("classify-l3a-failure: ok   stale-needle "
              "(both of provenance.go's refusals still say it)")

    # The verdict fixtures below compare (test, verdict, family) and never look at
    # what the tool tells the reader to DO, so without this the STALE_BINARY branch
    # could be deleted with every one of them still green.
    if why := check_next_step_branches():
        print(f"classify-l3a-failure: FAIL next-step: {why}")
        bad += 1
    else:
        print("classify-l3a-failure: ok   next-step "
              "(a stale binary is told to rebuild, everything else to rerun alone)")

    # And that the branch is reached with the right argument. The check above
    # proves the function branches; this one proves the caller feeds it the
    # family it classified.
    if why := check_next_step_is_joined():
        print(f"classify-l3a-failure: FAIL next-step-join: {why}")
        bad += 1
    else:
        print("classify-l3a-failure: ok   next-step-join "
              "(the report passes next_step the classified family, not something adjacent)")

    # And that the value behind that identifier is the classified one. The check
    # above reads the AST and cannot tell `family_of(out)` from `family_of("")`;
    # this renders a real log through the real entry point, which can.
    if why := check_report_renders_the_join():
        print(f"classify-l3a-failure: FAIL rendered-join: {why}")
        bad += 1
    else:
        print("classify-l3a-failure: ok   rendered-join "
              "(a real log rendered end to end still carries the family and its fix)")

    for name, want, blob in SELF_TEST:
        got = [
            (test, classify(result, out, marker), family_of(out)[0])
            for test, result, out in partition(blob.replace("@@MARKER@@", marker).splitlines())
        ]
        if got == want:
            print(f"classify-l3a-failure: ok   {name}")
        else:
            print(f"classify-l3a-failure: FAIL {name}:\n     got  {got}\n     want {want}")
            bad += 1
    if bad:
        print(f"classify-l3a-failure: {bad} check(s) failed. A wrong label is worse than")
        print("classify-l3a-failure: none — fix the logic or the marker, not the fixture.")
        return 1
    print("classify-l3a-failure: self-test passed — the verdicts and families still")
    print("classify-l3a-failure: mean what they say")
    return 0


def main() -> int:
    if len(sys.argv) == 2 and sys.argv[1] == "--self-test":
        return self_test()
    if len(sys.argv) != 2:
        print(__doc__.strip().splitlines()[-2].strip(), file=sys.stderr)
        return 2

    marker = read_marker()
    try:
        lines = pathlib.Path(sys.argv[1]).read_text(
            encoding="utf-8", errors="replace"
        ).splitlines()
    except OSError as exc:
        print(f"classify-l3a-failure: cannot read the log: {exc}", file=sys.stderr)
        return 2

    judged = [
        (test, classify(result, out, marker), family_of(out), out)
        for test, result, out in partition(lines)
    ]

    bar = "=" * 72
    print(bar)
    print("L3a failed. Per test: did the stand come up, and which shape is this?")
    print(bar)

    interesting = [(t, v, f, out) for t, v, f, out in judged if v != "PASS"]
    if not interesting:
        print()
        print("  Every test that reported a result reported PASS, yet the tier failed.")
        print("  That is `make e2e`'s own guards talking — an empty package set, or the")
        print("  binary dying outside a test. Read the lines above this block.")
        print()
        print(bar)
        return 0

    families_seen: list[str] = []
    for test, verdict, (family, detail), blob in interesting:
        if family and family not in families_seen:
            families_seen.append(family)

        label = test if test != "<run>" else "<no test named>"
        print()
        print(f"  {verdict:<14}{label}" + (f"  family: {family}" if family else ""))

        if verdict == "STAND-SETUP":
            print(f"{'':16}The harness declared it: bring-up died before the test body, so")
            print(f"{'':16}nothing was asserted and there is no finding about the code here.")
        elif verdict == "TIMEOUT":
            print(f"{'':16}The binary hit -timeout and the runtime killed it. This test held")
            print(f"{'':16}the clock; it is the victim, not necessarily the cause, and every")
            print(f"{'':16}test queued behind it never ran — so this run measured nothing")
            print(f"{'':16}about them. Check the budget against the real runtime first.")
        elif verdict == "NOT-RUN":
            print(f"{'':16}It reported no failure and no pass. Not a pass: either it skipped,")
            print(f"{'':16}or an earlier test took the binary down before it started.")
        else:
            print(f"{'':16}No bring-up declaration, so the stand was up and the code was")
            print(f"{'':16}exercised. A finding — and if a solitary rerun clears it, that is")
            print(f"{'':16}a finding too: a test that only fails in company is the very")
            print(f"{'':16}defect NIM-469 is about. Either way it is not nothing.")

        if detail:
            for line in detail.splitlines():
                print(f"{'':16}{line}")
        if family == "LOG_AFTER_TEST":
            m = LOG_AFTER_TEST.search(blob)
            if m:
                print(f"{'':16}Culprit named by the panic: {m.group(1)}")

        subs = sorted({
            s for s in re.findall(r"--- FAIL: (Test\S+/\S+)", blob)
        })
        if subs:
            shown = ", ".join(subs[:4]) + (" …" if len(subs) > 4 else "")
            print(f"{'':16}failed subtest(s): {shown}")

        if test != "<run>":
            headline, command = next_step(test, family)
            print(f"{'':16}{headline}")
            print(f"{'':18}{command}")

    findings = [t for t, v, _, _ in interesting if v == "TEST-FAILURE"]
    print()
    if len(families_seen) > 1 or len(findings) > 1:
        print(f"  {len(findings)} test(s) failed with the stand up"
              + (f", across {len(families_seen)} families: {', '.join(families_seen)}." if families_seen else "."))
        print("  Several shapes in ONE run is the NIM-469 signature itself. Do not chase")
        print("  them separately before ruling out a single shared cause: they are far")
        print("  more often one mechanism seen from three angles than three independent")
        print("  defects that happened to land together.")
        print()
    print("  Nothing above was downgraded: L3a failed and the caller still exits")
    print("  non-zero. This says WHICH failure is WHICH, never whether to care.")
    print(bar)
    return 0


if __name__ == "__main__":
    sys.exit(main())
