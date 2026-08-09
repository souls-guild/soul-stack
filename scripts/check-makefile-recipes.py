#!/usr/bin/env python3
"""Holds every `bash -c` / `sh -c` recipe in the Makefile to a fully quoted argument.

Why this exists (NIM-615). `dev-stop` was written as

    @bash -c 'set -e; ...; grep -qaE 'vite|node|npm' "/proc/$$wp/cmdline"; ...'

The inner quotes are not nested - they CLOSE the outer string. With `SHELL := /bin/sh`
the recipe therefore stopped being one command and became a three-stage pipeline:

    bash -c 'set -e; ...kp="' | node | npm ' ...rest of the script... '

so `bash` received a script truncated mid-statement (syntax error, nothing executed at
all), and make exited 127 on the missing `node`. The target that exists to kill an orphan
keeper killed nothing, `dev-down` died on the same prerequisite before ever reaching
`docker compose down`, and both stayed broken for two and a half weeks: a Makefile recipe
is not compiled, not linted and not executed by any gate, so nothing could notice.

What is checked. The argument that follows `bash -c` / `sh -c` must be entirely covered by
quotes. A single character sitting outside them means the outer string closed early and the
rest of the recipe is being read by the shell as separate words and operators.

Why quote coverage and not the token structure. A token-level rule ("nothing but a
redirection may follow the script") cannot tell the accidental `|` of a collapsed quote
from the deliberate `;` of a multi-command recipe - `check-stand-template` legitimately
runs four commands on one logical line. Quote coverage separates them exactly: the
collapse always leaves bare characters behind, a command separator never does.

Why not execute the recipe. Executing these lines is unsafe even with a stubbed PATH:
several carry redirections to absolute paths (`> "$STAND_DEV_DIR/keeper.dev.yml"`), so a
guard that ran them would write files. Scanning reproduces the exact step where the defect
happens - word splitting - without executing anything.

Deliberately loud: `bash -c 'a'"$X"` is a legal shell idiom this guard rejects, because in
this Makefile it is indistinguishable from the defect and nothing here needs it. Escaping a
literal quote as '\\'' is NOT rejected - `dev-stand` uses it and it stays one word.
"""

import re
import sys

RECIPE_PREFIXES = "@-+"
WORD_BREAK = set(" \t|&;<>()")
TARGET_LINE = re.compile(r"^([A-Za-z0-9._%/$()-]+)\s*:(?!=)")


class Word:
    """One shell word of a recipe line, with the position and quoting it was read at."""

    def __init__(self, value, start, bare, unterminated):
        self.value = value
        self.start = start
        self.bare = bare  # carries at least one unquoted, unescaped character
        self.unterminated = unterminated


def scan_word(text, i):
    """Read one shell word starting at i; return it plus how much of it was quoted."""
    value = []
    start = i
    bare = False
    quote = None
    while i < len(text):
        char = text[i]
        if quote is None:
            if char in WORD_BREAK:
                break
            if char == "\\":  # an escaped character is deliberate, not a collapsed quote
                value.append(text[i + 1 : i + 2])
                i += 2
                continue
            if char in "'\"":
                quote = char
                i += 1
                continue
            bare = True
            value.append(char)
            i += 1
            continue
        if quote == "'":
            if char == "'":
                quote = None
            else:
                value.append(char)
            i += 1
            continue
        if char == "\\" and i + 1 < len(text):
            value.append(text[i + 1])
            i += 2
            continue
        if char == '"':
            quote = None
        else:
            value.append(char)
        i += 1
    return Word("".join(value), start, bare, quote is not None), i


def split_words(line):
    """Split a recipe line into words, skipping the shell's own operators."""
    words = []
    i = 0
    while i < len(line):
        if line[i] in WORD_BREAK:
            i += 1
            continue
        word, i = scan_word(line, i)
        words.append(word)
    return words


def logical_recipe_lines(text):
    """Yield (line_number, text) for every recipe line, continuations folded in."""
    lines = text.split("\n")
    i = 0
    while i < len(lines):
        if not lines[i].startswith("\t"):
            i += 1
            continue
        start = i + 1
        parts = [lines[i][1:]]
        while parts[-1].endswith("\\") and i + 1 < len(lines):
            i += 1
            parts[-1] = parts[-1][:-1]
            parts.append(lines[i].lstrip("\t"))
        yield start, " ".join(parts).lstrip(RECIPE_PREFIXES)
        i += 1


def enclosing_targets(text):
    """Map line number -> the target whose recipe that line belongs to."""
    owner = {}
    current = "<unknown>"
    for number, line in enumerate(text.split("\n"), start=1):
        if not line.startswith(("\t", " ", "#")):
            match = TARGET_LINE.match(line)
            if match:
                current = match.group(1)
        owner[number] = current
    return owner


def is_shell(word):
    """True for an unquoted `bash`/`sh` command word, including a path to one."""
    if word.bare is False:
        return False
    name = word.value.rsplit("/", 1)[-1]
    return name in ("bash", "sh")


def check_line(number, line, owner):
    """Return findings for one logical recipe line, and whether it was inspected."""
    words = split_words(line)
    findings = []
    inspected = False
    for i, word in enumerate(words):
        if not is_shell(word) or i + 2 >= len(words) or words[i + 1].value != "-c":
            continue
        inspected = True
        script = words[i + 2]
        if script.unterminated:
            findings.append(
                f"Makefile:{number} ({owner}): the `{word.value} -c` argument never "
                f"closes its quote."
            )
        elif script.bare:
            findings.append(
                f"Makefile:{number} ({owner}): the `{word.value} -c` argument is not "
                f"fully quoted - the outer string closes early, so the shell splits the "
                f"rest of the recipe into words and operators of its own.\n"
                f"    bash would receive only: {script.value[:80]!r}...\n"
                f"    Quote inner patterns with \" \", or escape a literal quote as "
                f"'\\'' - never open a bare ' ' inside a '...' recipe."
            )
    return findings, inspected


def main():
    path = sys.argv[1] if len(sys.argv) > 1 else "Makefile"
    with open(path, encoding="utf-8") as handle:
        text = handle.read()

    owner = enclosing_targets(text)
    findings = []
    inspected = 0
    for number, line in logical_recipe_lines(text):
        line_findings, was_inspected = check_line(
            number, line, owner.get(number, "<unknown>")
        )
        findings.extend(line_findings)
        inspected += int(was_inspected)

    if findings:
        print(f"check-makefile-recipes: {len(findings)} broken recipe(s)\n")
        for finding in findings:
            print(f"  {finding}\n")
        return 1

    print(f"check-makefile-recipes: {inspected} `bash -c` recipes fully quoted")
    return 0


if __name__ == "__main__":
    sys.exit(main())
