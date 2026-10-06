#!/usr/bin/env python3
"""Refuses a term this project has banned, in every file git would commit (NIM-893).

Why a guard and not a sweep. The DevOps shorthand for the operations after `create` was
swept out once (NIM-89, 2026-07-09) and was back at 497 hits in 116 files eleven weeks
later. A sweep restores zero; only a check keeps it there. The replacement is fixed:
"advanced scenarios" (docs/naming-rules.md, "Advanced scenario").

The term is never spelled in this file or in the allowlist, so the guard cannot match its
own source: the pattern is written as character classes and escapes, and every self-test
fixture is built by concatenation at the boundary the pattern needs.

What is scanned: tracked files plus untracked ones that are not ignored — what the next
commit can carry, so a new file is refused here and not first in CI. Each file's PATH is
matched (reported as line 0), then its content line by line; a file with a NUL in its first
8 KiB is binary and only its path is matched.

What is matched, case-insensitively: the word for the day, then any run of spaces, tabs or
no-break spaces around at most one hyphen, underscore or dash from U+2010 to U+2015, then
the digit or the spelled-out number — and the Cyrillic equivalent. The left edge is a
word start or a camelCase hump, so an identifier carrying it (`redisXy2User` shapes) is
caught and `today two` is not.

Ordinary text that happens to put the day word right before the number IS flagged: a
duration in days and hours written with digits, a numbered pair of `dayN` variables, prose
that says "the day" and then "two" of something, and an ALL-CAPS longer word after the day
word (the word-end check looks for lower case only, so that a camelCase identifier stays
caught). Reword it. A matcher that tried to tell intent from coincidence would be the one
that lets the term back in.

What is NOT matched, by design of a line scanner over text:
  - the term split across a line break;
  - an ALL-CAPS run that hides the left edge (`FOOXY2` shapes, with no `_` before it);
  - a Roman numeral in place of the digit;
  - an invisible or look-alike character between the parts: a zero-width space, a soft
    hyphen, a narrow no-break space, a minus sign, a full-width digit;
  - the content of a binary file, and anything in an ignored one;
  - git history, commit messages, branch names and tracker items — they are not files
    in the tree.

Exceptions are explicit and counted: scripts/banned-terms-allowlist.txt holds
`path:line:count  # reason`, line 0 meaning the path itself. The line must carry exactly
`count` matches — one more is a new hit hiding behind an old exception, one fewer means
the entry has outlived part of its reason — and both are errors.

Usage:
    scripts/check-banned-terms.py             # check this tree
    scripts/check-banned-terms.py --self-test # check the checker
"""

from __future__ import annotations

import os
import pathlib
import re
import shutil
import subprocess
import sys
import tempfile

ALLOWLIST = "scripts/banned-terms-allowlist.txt"
SELF = "scripts/check-banned-terms.py"
REPLACEMENT = "advanced scenarios"

_GAP = "[ \t\u00a0]*"
_SEP = _GAP + "[-_\u2010-\u2015]?" + _GAP
BANNED = re.compile(
    "(?:(?<![A-Za-z0-9])|(?=Day))"
    "[Dd][Aa][Yy]" + _SEP + "(?:2(?![0-9])|[Tt][Ww][Oo](?![a-z]))"
    "|(?<![\u0400-\u04ff])"
    "[\u0414\u0434][\u0415\u0435][\u041d\u043d][\u042c\u044c]" + _SEP
    + "(?:2(?![0-9])|[\u0414\u0434][\u0412\u0432][\u0410\u0430](?![\u0430-\u044f\u0451]))"
)


def corpus(root: pathlib.Path) -> list[str]:
    out = subprocess.run(
        ["git", "-C", str(root), "ls-files", "-z", "--cached", "--others", "--exclude-standard"],
        check=True,
        capture_output=True,
    ).stdout
    return sorted({p for p in out.decode("utf-8", "surrogateescape").split("\0") if p})


def scan(root: pathlib.Path, paths: list[str]) -> tuple[dict[tuple[str, int], list[str]], int]:
    hits: dict[tuple[str, int], list[str]] = {}
    scanned = 0
    for rel in paths:
        found = [m.group(0) for m in BANNED.finditer(rel)]
        if found:
            hits[(rel, 0)] = found
        path = root / rel
        if path.is_symlink() or not path.is_file():
            continue
        data = path.read_bytes()
        scanned += 1
        if b"\0" in data[:8192]:
            continue
        for lineno, line in enumerate(data.decode("utf-8", "replace").split("\n"), 1):
            found = [m.group(0) for m in BANNED.finditer(line)]
            if found:
                hits[(rel, lineno)] = found
    return hits, scanned


def parse_allowlist(text: str) -> tuple[dict[tuple[str, int], int], list[str]]:
    entries: dict[tuple[str, int], int] = {}
    problems: list[str] = []
    for n, raw in enumerate(text.splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        where, sep, reason = line.partition("#")
        where, reason = where.strip(), reason.strip()
        m = re.fullmatch(r"(\S+):(0|[1-9][0-9]*):([1-9][0-9]*)", where)
        if not m:
            problems.append(f"{ALLOWLIST}:{n}: expected `path:line:count  # reason`, got {raw!r}")
            continue
        if not sep or not reason:
            problems.append(f"{ALLOWLIST}:{n}: {where} has no reason — an exception nobody can explain is not one")
            continue
        key = (m.group(1), int(m.group(2)))
        if key in entries:
            problems.append(f"{ALLOWLIST}:{n}: {key[0]}:{key[1]} is listed twice")
            continue
        entries[key] = int(m.group(3))
    return entries, problems


def check(root: pathlib.Path, allowlist_text: str, anchor: str | None) -> tuple[list[str], int]:
    paths = corpus(root)
    hits, scanned = scan(root, paths)
    entries, problems = parse_allowlist(allowlist_text)
    if scanned == 0:
        problems.append("scanned no files at all — a guard over an empty corpus is green about nothing")
    if anchor is not None and anchor not in paths:
        problems.append(f"{anchor} is not in the corpus, so the corpus is not this tree")
    for (rel, lineno), found in sorted(hits.items()):
        allowed = entries.get((rel, lineno))
        if allowed == len(found):
            continue
        words = ", ".join(repr(w) for w in found)
        where = f"{rel}:{lineno}" + (" (the path)" if lineno == 0 else "")
        if allowed is None:
            problems.append(f"{where}: {words} — say \"{REPLACEMENT}\" (docs/naming-rules.md)")
        else:
            problems.append(
                f"{where}: carries {len(found)} match(es), the allowlist entry allows {allowed} — "
                f"look at what changed on this line, then fix the line or the count ({words})"
            )
    for (rel, lineno) in sorted(entries):
        if (rel, lineno) not in hits:
            problems.append(f"{ALLOWLIST}: {rel}:{lineno} no longer carries the term — delete the entry")
    return problems, scanned


def main() -> int:
    root = pathlib.Path(
        subprocess.run(["git", "rev-parse", "--show-toplevel"], check=True, capture_output=True, text=True).stdout.strip()
    )
    allow = root / ALLOWLIST
    allowlist_text = allow.read_text(encoding="utf-8") if allow.is_file() else ""
    problems, scanned = check(root, allowlist_text, SELF)
    for p in problems:
        print(f"check-banned-terms: {p}", file=sys.stderr)
    if problems:
        print(f"check-banned-terms: {len(problems)} problem(s) over {scanned} files", file=sys.stderr)
        return 1
    allowed = len(parse_allowlist(allowlist_text)[0])
    print(f"check-banned-terms: no banned term in {scanned} files ({allowed} allowlisted line(s))")
    return 0


# The self-test builds each fixture so that the source line holding it does not match:
# the pattern needs the separator or the number right after the day word, and here a
# quote and a `+` stand between them.
DAY = "day"
DEN = "\u0434\u0435\u043d\u044c"


def self_test() -> int:
    flagged = [
        ("hyphen, lower", DAY + "-2"),
        ("hyphen, title", "Day" + "-2"),
        ("upper, glued", "DAY" + "2"),
        ("space", DAY + " 2"),
        ("glued", DAY + "2"),
        ("underscore", DAY + "_2"),
        ("unicode dash", DAY + "\u20112"),
        ("hyphen U+2010", DAY + chr(0x2010) + "2"),
        ("no-break space", DAY + "\u00a0" + "2"),
        ("tab", DAY + "\t2"),
        ("doubled space", DAY + "  2"),
        ("three spaces", DAY + "   2"),
        ("spaced hyphen", DAY + " - 2"),
        ("spaced en dash", DAY + " \u2013 2"),
        ("spaced em dash", DAY + " " + chr(0x2014) + " 2"),
        ("horizontal bar", DAY + chr(0x2015) + "2"),
        ("spelled, hyphen", DAY + "-two"),
        ("spelled, space", DAY + " two"),
        ("spelled, upper", "DAY" + "-TWO"),
        ("test name", "TestRerun_Day" + "2_Reuses"),
        ("camelCase identifier", "redisDay" + "2User"),
        ("camelCase spelled", "guardDay" + "TwoFields"),
        ("after an acronym", "HTTPDay" + "2"),
        ("snake identifier", "suite_" + DAY + "2"),
        ("cyrillic, hyphen", DEN + "-2"),
        ("cyrillic, space, title", "\u0414" + DEN[1:] + " 2"),
        ("cyrillic, spelled", DEN + " \u0434\u0432\u0430"),
        ("cyrillic, upper, spelled", DEN.upper() + " " + "\u0434\u0432\u0430".upper()),
    ]
    clean = [
        ("a weekday", "Mond" + "ay 2"),
        ("today, then a count", "tod" + "ay two of three"),
        ("a longer number", DAY + "-20"),
        ("a year", DAY + "-2026"),
        ("inside a word", "holi" + DAY + "2"),
        ("plural", DAY + "s 2"),
        ("a longer word", DAY + " twofold"),
        ("punctuation between", DAY + ", 2 runs"),
        ("the reverse order", "2-" + DAY),
        ("cyrillic, inside a word", "\u043f\u043e\u043b" + DEN + " 2"),
        ("cyrillic, a longer number", DEN + " 20"),
        ("cyrillic, a longer word", DEN + " \u0434\u0432\u0430\u0434\u0446\u0430\u0442\u044c"),
    ]
    failures: list[str] = []

    def expect(name: str, ok: bool, detail: str) -> None:
        print(f"  {'PASS' if ok else 'FAIL'}  {name}")
        if not ok:
            failures.append(f"{name}: {detail}")

    for name, text in flagged:
        expect(f"flags {name}", bool(BANNED.search(f"x {text} y")), f"{text!r} was not matched")
    for name, text in clean:
        found = BANNED.search(f"x {text} y")
        expect(f"passes {name}", found is None, f"{text!r} matched {found and found.group(0)!r}")

    scratch = pathlib.Path(tempfile.mkdtemp(prefix="banned-terms-"))
    # The fixtures' git must be the only git in play: a global excludes file that happened
    # to ignore a fixture would turn a red case green on one machine, and a GIT_DIR or
    # GIT_INDEX_FILE inherited from a hook would stage the fixtures into the real index.
    for var in ("GIT_DIR", "GIT_INDEX_FILE", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY",
                "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_COMMON_DIR", "GIT_PREFIX",
                "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT", "GIT_TEMPLATE_DIR"):
        os.environ.pop(var, None)
    os.environ["GIT_CONFIG_GLOBAL"] = os.devnull
    os.environ["GIT_CONFIG_NOSYSTEM"] = "1"
    os.environ["XDG_CONFIG_HOME"] = str(scratch)

    def repo(files: dict[str, bytes], untracked: dict[str, bytes] | None = None) -> pathlib.Path:
        root = pathlib.Path(tempfile.mkdtemp(dir=scratch))
        subprocess.run(["git", "init", "-q", str(root)], check=True)
        for rel, data in files.items():
            (root / rel).parent.mkdir(parents=True, exist_ok=True)
            (root / rel).write_bytes(data)
        if files:
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
        for rel, data in (untracked or {}).items():
            (root / rel).parent.mkdir(parents=True, exist_ok=True)
            (root / rel).write_bytes(data)
        return root

    bad = (DAY + "-2").encode()
    doc = b"# Guide\n\nRun the " + bad + b" scenario.\n"
    go = b"package x\n\n// The " + ("Day" + "2").encode() + b" path reads the row.\nfunc F() {}\n"
    named = "suites/" + DAY + "2.sh"
    base = {"README.md": b"clean\n", "x.go": b"package x\n"}

    def run(name: str, files: dict[str, bytes], allow: str, want: list[str], untracked=None, anchor=None) -> None:
        root = repo(files, untracked)
        problems, _ = check(root, allow, anchor)
        joined = "\n".join(problems)
        if not want:
            expect(name, not problems, f"expected silence, got: {joined}")
            return
        missing = [w for w in want if w not in joined]
        expect(name, not missing, f"expected {missing} in: {joined or '(no problems)'}")

    run("a clean tree is green", base, "", [])
    run("a doc carrying the term is red", {**base, "docs/guide.md": doc}, "", ["docs/guide.md:3:"])
    run("a Go comment carrying the term is red", {**base, "x.go": go}, "", ["x.go:3:"])
    run("a file NAMED with the term is red", {**base, named: b"true\n"}, "", [named + ":0 (the path)"])
    run("a BINARY file named with the term is red", {**base, named + ".bin": b"\0\1"}, "", [named + ".bin:0 (the path)"])
    run("an untracked, unignored file is red", base, "", ["new.md:1:"], untracked={"new.md": bad + b"\n"})
    run(
        "an ignored file is not scanned",
        {**base, ".gitignore": b"scratch/\n"},
        "",
        [],
        untracked={"scratch/notes.md": bad + b"\n"},
    )
    run("a binary file's content is not scanned", {**base, "blob.bin": b"\0\1" + bad}, "", [])
    run("an allowlisted line passes", {**base, "docs/guide.md": doc}, "docs/guide.md:3:1  # vendored\n", [])
    run("an allowlisted path passes", {**base, named: b"true\n"}, named + ":0:1  # vendored\n", [])
    run(
        "an allowlist entry covers its line only",
        {**base, "docs/guide.md": doc + bad + b"\n"},
        "docs/guide.md:3:1  # vendored\n",
        ["docs/guide.md:4:"],
    )
    run(
        "one more hit on an allowlisted line is red",
        {**base, "docs/guide.md": b"# Guide\n\n" + bad + b" and " + bad + b"\n"},
        "docs/guide.md:3:1  # vendored\n",
        ["carries 2 match(es), the allowlist entry allows 1"],
    )
    run(
        "one fewer hit on an allowlisted line is red",
        {**base, "docs/guide.md": doc},
        "docs/guide.md:3:2  # vendored\n",
        ["carries 1 match(es), the allowlist entry allows 2"],
    )
    run("a stale allowlist entry is red", base, "README.md:1:1  # gone\n", ["no longer carries the term"])
    run("an allowlist entry without a reason is red", {**base, "docs/guide.md": doc}, "docs/guide.md:3:1\n", ["has no reason"])
    run(
        "a duplicated allowlist entry is red",
        {**base, "docs/guide.md": doc},
        "docs/guide.md:3:1  # a\ndocs/guide.md:3:1  # b\n",
        ["listed twice"],
    )
    run("an allowlist entry without a count is red", {**base, "docs/guide.md": doc}, "docs/guide.md:3  # x\n", ["expected `path:line:count"])
    run("an empty corpus is red", {}, "", ["scanned no files"])
    run("a corpus without the anchor is red", base, "", ["is not in the corpus"], anchor=SELF)
    shutil.rmtree(scratch)

    if failures:
        for f in failures:
            print(f"check-banned-terms --self-test: FAIL {f}", file=sys.stderr)
        return 1
    print("check-banned-terms --self-test: all cases pass")
    return 0


if __name__ == "__main__":
    if sys.argv[1:] == ["--self-test"]:
        sys.exit(self_test())
    if sys.argv[1:]:
        print(__doc__, file=sys.stderr)
        sys.exit(2)
    sys.exit(main())
