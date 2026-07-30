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

Nothing is ever downgraded to a pass: the caller's exit code is untouched, and
UNCLEAR is to be treated as a finding until a solitary rerun says otherwise.

Usage: classify-l1-failure.py <go-test-output-file>
"""

import re
import sys

# Signatures only the container/daemon layer produces. An assertion in this
# repository does not print any of these.
INFRA = [
    "wait until ready: context deadline exceeded",
    "failed to start container",
    "container startup",
    "Error response from daemon",
    "Cannot connect to the Docker daemon",
    "error during connect",
    "docker-credential-",
    "reaper failed",
    "could not start reaper",
    "Reaper: failed",
    "port not found",
    "failed to get mapped port",
    "no such host",
    "image pull",
    "manifest unknown",
    "toomanyrequests",
    "device or resource busy",
    "no space left on device",
]

# Signatures either layer could produce. Named separately on purpose — see the
# module docstring on why these must not be called INFRA.
UNCLEAR = [
    "connection refused",
    "CLUSTERDOWN",
    "i/o timeout",
    "EOF",
    "context deadline exceeded",
]

FAIL_PKG = re.compile(r"^(?:FAIL|ok|---)\s")
FAIL_LINE = re.compile(r"^FAIL\s+(\S+)")
TEST_FAIL = re.compile(r"^\s*--- FAIL: (\S+)")


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


def main() -> int:
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
        blob = "\n".join(out)
        infra_hits = [s for s in INFRA if s in blob]
        unclear_hits = [s for s in UNCLEAR if s in blob]
        tests = TEST_FAIL.findall(blob)

        if tests and not infra_hits and not unclear_hits:
            verdict = "REGRESSION"
        elif infra_hits:
            verdict = "INFRA"
        elif unclear_hits:
            verdict = "UNCLEAR"
        else:
            verdict = "REGRESSION"

        print()
        print(f"  {verdict:<11}{path}")
        if tests:
            shown = ", ".join(tests[:4]) + (" …" if len(tests) > 4 else "")
            print(f"{'':13}failed test(s): {shown}")
        if verdict == "INFRA":
            print(f"{'':13}matched: {', '.join(repr(s) for s in infra_hits[:3])}")
        elif verdict == "UNCLEAR":
            print(f"{'':13}matched: {', '.join(repr(s) for s in unclear_hits[:3])}"
                  " — either layer can print these")

        pkg = import_path_to_pkg(path)
        if verdict == "REGRESSION":
            print(f"{'':13}An assertion caught something. Fix it; rerunning changes nothing.")
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
