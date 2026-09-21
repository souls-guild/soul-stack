#!/usr/bin/env python3
"""Holds every release command RELEASING.md names to either a pipeline job or a stated exemption.

Why this exists (NIM-879). `make e2e-live-gate` was RELEASING.md step (e), marked
blocking, for months. Nothing ran it: the L3b tier appears in one workflow job that had
never executed and that carried `continue-on-error: true` anyway. So the tier was red on
the release tip from migration 118 onward, and the only reason anybody found out is that
someone ran it by hand while working on something else. That is not a forgotten test — it
is a month of decisions taken against a green that was never produced, the same shape as
the `t.Skip` in the single e2e push test (NIM-869).

The fix for ONE step is a job in release.yml. The fix for the CLASS is this guard: a
command is either enforced by the pipeline or listed as a human's with a reason, and there
is no third state in which it is merely asserted.

What is checked.

  1. $(RELEASE_GATE_TARGETS) is non-empty. An empty list makes every check below
     vacuously true, which is this guard's own version of the defect it exists to catch.
  2. Exactly one job publishes — it runs goreleaser. The anchor is the ACTION, not the job
     key: a decoy job named `release` beside a renamed publisher would otherwise satisfy
     every check while the real publisher ran ungated.
  3. Every gate target is run by a job the publisher transitively needs, by a `run:` and
     not by appearing in a `name:` or a comment.
  4. No job on that path, publisher included, carries `continue-on-error`, or an `if:`
     that calls a status function — always()/failure()/cancelled() **and success()**, which
     reads as careful and is the one that gets written. Both defeat `needs:` silently and in one
     line: a dependent's `needs:` is satisfied when a continue-on-error job FAILS, and a
     status function in the `if:` drops the implicit "all needs succeeded". An ordinary
     predicate — a ref guard, say — does not, and is refused by nothing here, because a
     guard that forbade every `if:` would forbid exactly the hardening RELEASING.md
     recommends. A value this script cannot evaluate — `${{ … }}` — counts as present,
     because the question is whether the gate can be proven to hold.
  5. The step running a gate target carries no `if:` and no `continue-on-error` either.
     A step-level `if:` is the same defeat one level down, and it is the one a re-run path
     invites: `if: github.event_name != 'workflow_dispatch'` would skip the tier on every
     dispatched release while the job still concluded success.
  6. Every command named in RELEASING.md — `make …` in a code span or fenced block, and
     every `scripts/…` invocation — is in $(RELEASE_GATE_TARGETS) or in
     scripts/release-gate-allowlist.txt with a reason.
  7. The allowlist has no stale entries and no contradictions.
  8. No job in release.yml can conclude before the gate. Not about this workflow at all:
     check-release-provenance.sh reads "no job in the run failed and one succeeded" as "the
     gate passed", and a sibling job going green ahead of a queued gate makes that false in
     a file that never mentions it.
  9. The apt mirror (apt-publish.yml) sits behind BOTH release checks — the provenance one
     and the asset one — with neither skippable by a step `if:` nor run anywhere `set -e`
     would not see it fail: `|| true`, a pipe, a condition, `set +e`. Some of those have
     fail-closed spellings (`if ! check; then exit 1; fi`) and are refused anyway, because
     telling them apart means reading the branch — a refused honest step costs a rewrite, a
     missed dishonest one costs the pool. It fires on `release: published`,
     which release.yml's `needs:` does not reach, so this is a second gate rather than a
     restatement of the first.

What is NOT checked, and cannot be by a tool of this shape: a blocking step that names no
command at all. RELEASING.md (c2) stamps `introduced_in` by editing two files and (d) is a
documentation audit; neither has a command to gate, so neither appears here or in the
allowlist. They are named in the allowlist header instead, which is the only place their
absence can be seen.

Why the YAML is parsed by hand. The subject is one file we own, the structure needed is
three keys deep, and --self-test pins the parse against fixtures carrying every shape the
checks above are about. The direction that matters is guarded explicitly: anything the
parser cannot make sense of — no `jobs:`, no publishing job, a `needs:` naming a job that
does not exist — is a hard failure, never an empty result that reads as "nothing wrong".

Usage:
    scripts/check-release-gate.py             # check this tree
    scripts/check-release-gate.py --self-test # check the checker
"""

from __future__ import annotations

import pathlib
import re
import sys
import tempfile

RELEASE_WORKFLOW = ".github/workflows/release.yml"
RELEASE_DOC = "RELEASING.md"
ALLOWLIST = "scripts/release-gate-allowlist.txt"

# The second publishing path (NIM-882). apt-publish.yml fires on `release: published` — any
# release, including one a human made by hand — so `needs: live-gate` in release.yml does not
# reach it, and .debs nothing gated would land in a public pool. The ruleset NIM-879 expected
# to close this does not exist: GitHub rulesets target branches, tags and pushes, never
# releases. The check is scripts/check-release-provenance.sh, and what is asserted here is
# that the mirror still sits behind it — deleting a `needs:` is one line and reads as tidying.
APT_WORKFLOW = ".github/workflows/apt-publish.yml"
# Anchored on the script that writes to the bucket, not on a job key, for the same reason the
# publisher is found by its action: a renamed job beside a decoy would otherwise pass.
APT_PUBLISH_SCRIPT = "deploy/apt-r2/publish-apt.sh"
# Two checks, not one, because they answer different questions: whether the gated pipeline
# CREATED the release, and whether these are the assets it BUILT. `gh release upload` onto
# the pipeline's own release satisfies the first and is exactly what the second refuses.
MIRROR_CHECKS = ("scripts/check-release-provenance.sh", "scripts/verify-release-assets.sh")
PROVENANCE_SCRIPT = MIRROR_CHECKS[0]

# Forms that keep the command in the recipe and remove the only reason it was there. All
# one line, all invisible to shell_commands(), which splits on the separators and discards
# them — so the raw text is re-read for these.
#
#   cmd || true      the right arm runs precisely when cmd failed
#   cmd | tee log    Actions' default shell is `bash -e {0}` with NO pipefail, so only the
#                    last command's status is the step's. `|&` is the same thing.
#   if ! cmd; then   a command in a condition is never a `set -e` trigger
#   set +e           turns the mechanism off for the rest of the block
#
# `&&` is deliberately absent: `a && b` still fails when `a` does. Any `|` counts, including
# one inside a quoted argument — a false positive there fails closed, which is the side to
# be wrong on.
SWALLOWED = re.compile(r"\|")
CONDITIONAL = re.compile(r"^\s*(?:if|elif|while|until)\b|(?<![\w!])!\s")
SET_RELAXES = re.compile(r"^\s*set\s+(?:\+[a-zA-Z]*e[a-zA-Z]*|\+o\s+errexit)\b")

# The publisher is identified by the goreleaser action it uses, never by what the job is
# called: a decoy job keyed `release` beside a renamed publisher would otherwise satisfy
# every check while the real publisher ran ungated. `uses:` is matched as a prefix rather
# than as a substring anywhere, so `go install github.com/goreleaser/goreleaser/v2@latest`
# in some other job does not turn that job into a second publisher.
PUBLISHER_ACTION = "goreleaser/goreleaser-action"

# A script invoked from RELEASING.md is a release command too — step (c3) is one and the
# apt mirror is another — and a scanner that only knew `make` would report them as covered
# by saying nothing. Any `<dir>/<file>.sh|.py`, not only `scripts/`.
SCRIPT_CALL = re.compile(r"(?<![\w/.-])((?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_-]+\.(?:sh|py))")

# Fenced blocks and inline code spans. Prose is deliberately excluded: "make sure" is
# English, and a guard that reddens on it teaches people to widen the allowlist.
FENCED = re.compile(r"```[^\n]*\n(.*?)```", re.S)
INLINE = re.compile(r"`([^`\n]+)`")

# A target name may not end in `.` or `-`; without that, a sentence ending in `make foo.`
# reds as an unenforced `foo.` and reports the real `foo` exemption as stale.
TARGET_NAME = re.compile(r"[a-zA-Z](?:[a-zA-Z0-9_.-]*[a-zA-Z0-9_])?$")

# Words that may stand before the command they introduce without being it.
SHELL_LEADERS = {"if", "then", "else", "elif", "do", "while", "until", "!", "time", "sudo", "env", "exec", "nohup"}

# A status-check function in a job's `if:` removes the implicit `success()` that a job with
# `needs:` otherwise carries — GitHub's rule, and it includes `success` itself: `if:
# success() || github.event_name == 'workflow_dispatch'` evaluates to true on a red gate and
# releases over it. A plain predicate — a ref guard, say — keeps the implicit check, so
# refusing every `if:` would forbid exactly the hardening this file's own prose recommends.
STATUS_OVERRIDE = re.compile(r"\b(always|failure|cancelled|success)\s*\(", re.I)

# Block scalars this script does not read. An `if:` written as one is not provably safe, and
# unprovable is treated as unsafe here for the same reason `${{ … }}` is.
BLOCK_SCALARS = ("|", ">", "|-", ">-", "|+", ">+")


def shell_commands(text: str) -> list[list[str]]:
    """A snippet split into commands, each a token list starting at its command word.

    A snippet is not one command. `make foo; make bar` is two and used to yield only the
    last; `echo make foo` is one that runs no target at all and used to count as if it did.
    Both are false answers about whether a step runs a gate target, and the second is the
    false GREEN.

    ★ THE COMMENT IS CUT PER LINE, BEFORE THE SEPARATORS. Doing it the other way round —
    split first, then drop tokens from the `#` — made `echo skip # ; make the-gate` report
    the gate as running, because the text after the `;` became its own segment with no `#`
    in it. That is this ticket's own defect reproduced inside the guard against it.

    Not parsed, and said out loud rather than implied: a command assembled at runtime
    (`eval`, a variable holding the target, a heredoc). RELEASING.md writes its commands
    plainly and the workflow recipes are ordinary lines; anything deliberately obscured is
    outside what a scanner of this shape can claim.
    """
    joined = text.replace("\\\n", " ").replace("\t", " ")
    # Command substitution and backticks hold commands too, so they are separators, not
    # part of a word: `$(make x)` must not read as a token called `$(make`.
    joined = joined.replace("$(", "\n").replace("`", "\n").replace(")", "\n")
    commands: list[list[str]] = []
    for line in joined.split("\n"):
        tokens = line.split()
        cut = next((i for i, t in enumerate(tokens) if t.startswith("#")), len(tokens))
        for segment in re.split(r"[;&|]+", " ".join(tokens[:cut])):
            tokens = [t.strip("\"'") for t in segment.split()]
            while tokens:
                if tokens[0] in SHELL_LEADERS or re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*=.*", tokens[0]):
                    tokens = tokens[1:]
                    continue
                # `bash -c "make x"` and `xargs make x` run their argument; the wrapper is
                # not the command the reader means.
                if tokens[0] in ("bash", "sh", "zsh") and "-c" in tokens[1:3]:
                    tokens = tokens[tokens.index("-c") + 1 :]
                    continue
                if tokens[0] == "xargs":
                    tokens = [t for t in tokens[1:] if not t.startswith("-")]
                    continue
                break
            if tokens:
                commands.append(tokens)
    return commands


def make_targets_in(text: str) -> set[str]:
    """Targets of every `make` / `$(MAKE)` invocation in a snippet.

    Tokenised rather than matched as one regexp so that flags, variable assignments and
    `-C <dir>` do not hide the target behind them — each of those used to yield nothing,
    which reads as "this command runs no target".
    """
    found: set[str] = set()
    for tokens in shell_commands(text):
        if tokens[0].lstrip("$(").rstrip(")") not in ("make", "MAKE", "gmake"):
            continue
        i = 1
        while i < len(tokens):
            t = tokens[i]
            if t in ("-C", "-f", "--directory", "--file"):
                i += 2
                continue
            if t.startswith("-") or "=" in t:
                i += 1
                continue
            break
        if i < len(tokens) and TARGET_NAME.fullmatch(tokens[i]):
            found.add(tokens[i])
    return found


def render(value: bool | str) -> str:
    """A parsed scalar back in the spelling the workflow would carry."""
    return str(value).lower() if isinstance(value, bool) else value


def truthiness(raw: str) -> bool | str:
    """A YAML scalar as True, False, or "unknown" — anything this script cannot evaluate."""
    value = raw.strip()
    if "#" in value and not value.startswith(("'", '"')):
        value = value.split("#", 1)[0].strip()
    value = value.strip("\"'").lower()
    if value == "true":
        return True
    if value == "false":
        return False
    return "unknown"


class Step:
    def __init__(self) -> None:
        self.if_expr: str | None = None
        self.continue_on_error: bool | str = False
        self.make_targets: set[str] = set()
        self.uses: list[str] = []
        self.runs: list[str] = []
        # `with:` entries, read only from inside a `with:` block: an `args:` picked up from
        # any depth let an unrelated nested key overwrite the publisher's and lose it.
        self.args: str | None = None
        self.install_only: bool | str | None = None


class Job:
    def __init__(self, name: str) -> None:
        self.name = name
        self.needs: list[str] = []
        self.if_expr: str | None = None
        self.continue_on_error: bool | str = False
        self.steps: list[Step] = []

    @property
    def make_targets(self) -> set[str]:
        return set().union(*(s.make_targets for s in self.steps)) if self.steps else set()

    def steps_running(self, script: str) -> list[Step]:
        """The steps whose `run:` invokes `script` as a command — not ones that merely name it.

        Same distinction `make_targets` draws: `echo scripts/x.sh` and a commented-out call
        run nothing, and counting them is the false green.
        """
        return [
            step
            for step in self.steps
            if any(tokens[0] == script for tokens in shell_commands("\n".join(step.runs)))
        ]

    def swallows(self, script: str) -> list[str]:
        """Where `script` runs but its failure cannot fail the step.

        `run: scripts/x.sh || true` passes steps_running() — the separator is what makes it
        a no-op, and that is the token shell_commands() throws away. See SWALLOWED for the
        whole list. Not covered, and said rather than implied: a failure caught by a `trap`,
        a `set -e` disabled in a sourced file, and a wrapper script that exits 0 itself.
        """
        found: list[str] = []
        for step in self.steps:
            text = "\n".join(step.runs).replace("\\\n", " ")
            if not any(tokens[0] == script for tokens in shell_commands(text)):
                continue
            for line in text.split("\n"):
                bare = line.split("#", 1)[0]
                if SET_RELAXES.search(bare):
                    found.append(bare.strip())
                if script not in bare:
                    continue
                if SWALLOWED.search(bare.split(script, 1)[1]) or CONDITIONAL.search(bare):
                    found.append(bare.strip())
        return found

    def publishes(self) -> bool:
        """Does this job run `goreleaser release`?

        Three near-misses are excluded on purpose, because each would make a second job
        count and leave this script unable to say which one comes first: a step that merely
        installs the tool (`install-only: true`, or `go install …/goreleaser`), and
        `goreleaser check`, which validates the config and publishes nothing.

        An `args:` this script cannot read counts AS a release, not against one. The
        action's own default is `release --rm-dist`, so the unreadable case is far likelier
        to be the publisher than not — and guessing the other way hides it, which is the
        expensive direction.
        """
        for step in self.steps:
            for used in step.uses:
                if used.split("@")[0].strip("\"' ") != PUBLISHER_ACTION:
                    continue
                if step.install_only is True:
                    continue
                if step.args is None or step.args in BLOCK_SCALARS:
                    return True
                if "release" in step.args.strip("\"'").split():
                    return True
            for tokens in shell_commands("\n".join(step.runs)):
                if tokens[0] == "goreleaser" and "release" in tokens[1:]:
                    return True
        return False


def _indent(line: str) -> int:
    return len(line) - len(line.lstrip(" "))


def parse_workflow(text: str) -> dict[str, Job]:
    """The jobs of a workflow: needs, if, continue-on-error, and each step's run/uses.

    Deliberately narrow — `jobs:` at column 0, job keys at two spaces, job properties at
    four, steps deeper. That is what this repository's workflows are, and a shape it cannot
    read raises rather than returning less.
    """
    lines = text.splitlines()
    try:
        start = next(i for i, l in enumerate(lines) if l.rstrip() == "jobs:")
    except StopIteration:
        raise ValueError("no top-level `jobs:` key")

    jobs: dict[str, Job] = {}
    job: Job | None = None
    step: Step | None = None
    in_steps = False
    block_indent: int | None = None  # a `run:` block scalar we are accumulating
    needs_list = False
    with_indent: int | None = None
    # The indent a step's `- ` sits at, learned from the first one in this job. YAML lets a
    # sequence sit at its key's indent or deeper, and pinning it to six spaces parsed the
    # other style to zero steps — which surfaces as "nothing runs `make …`", a plausible
    # sentence about a file that does run it.
    dash_indent: int | None = None

    for line in lines[start + 1 :]:
        stripped = line.strip()

        if block_indent is not None:
            if not stripped or _indent(line) > block_indent:
                if not stripped.startswith("#"):
                    step.runs.append(line)
                continue
            block_indent = None

        # A blank line or a comment never ends a mapping, and treating a column-0 comment
        # as the end of `jobs:` used to drop every job after it.
        if not stripped or stripped.startswith("#"):
            continue
        if _indent(line) == 0:
            break

        if needs_list:
            item = re.match(r"^\s*-\s*(\S+)\s*$", line)
            if item and _indent(line) >= 4:
                job.needs.append(item.group(1).strip("\"'"))
                continue
            needs_list = False

        job_key = re.match(r"^ {2}([A-Za-z0-9_-]+):\s*$", line)
        if job_key:
            job = Job(job_key.group(1))
            jobs[job.name] = job
            step, in_steps, dash_indent = None, False, None
            continue
        if job is None:
            continue

        indent = _indent(line)

        # A steps sequence may sit at its key's own indent, where its `- ` items collide
        # with the column job properties use. The dash decides, so it is read first.
        if in_steps and re.match(r"^\s*-\s", line) and indent >= 4:
            pass
        elif indent == 4:
            in_steps = False
            key = re.match(r"^\s*([A-Za-z0-9_-]+):\s*(.*)$", line)
            if not key:
                continue
            name, value = key.group(1), key.group(2).strip()
            if name == "steps":
                in_steps, dash_indent = True, None
            elif name == "needs":
                if value.startswith("["):
                    job.needs += [n.strip().strip("\"'") for n in value.strip("[]").split(",") if n.strip()]
                elif value:
                    job.needs.append(value.strip("\"'"))
                else:
                    needs_list = True
            elif name == "if":
                job.if_expr = value or "(an empty if:)"
            elif name == "continue-on-error":
                job.continue_on_error = truthiness(value)
            continue

        if not in_steps:
            continue

        dash = re.match(r"^(\s*)-\s", line)
        if dash and (dash_indent is None or len(dash.group(1)) == dash_indent):
            dash_indent = len(dash.group(1))
            step = Step()
            job.steps.append(step)
            line = " " * (dash_indent + 2) + line.lstrip()[2:]
        if step is None:
            continue

        key = re.match(r"^\s*([A-Za-z0-9_-]+):\s*(.*)$", line)
        if not key:
            continue
        name, value = key.group(1), key.group(2).strip()

        # A step's own keys sit exactly one level in from its dash. Deeper is somebody
        # else's mapping — `with:`, `env:` — and reading `if:` or `continue-on-error:`
        # from there attributes a nested key to the step.
        own_indent = dash_indent + 2
        if _indent(line) > own_indent:
            if with_indent is not None and _indent(line) > with_indent:
                if name == "args":
                    step.args = value
                elif name == "install-only":
                    step.install_only = truthiness(value)
            continue
        with_indent = own_indent if name == "with" else None

        if name == "run":
            if value in BLOCK_SCALARS or value == "":
                block_indent = _indent(line)
            else:
                step.runs.append(value)
        elif name == "uses":
            step.uses.append(value)
        elif name == "if":
            step.if_expr = value or "(an empty if:)"
        elif name == "continue-on-error":
            step.continue_on_error = truthiness(value)

    if not jobs:
        raise ValueError("`jobs:` parsed to nothing")
    for j in jobs.values():
        for s in j.steps:
            # Newline-joined, not space-joined: a `#` comment runs to the end of ITS line,
            # and flattening the lines would let one swallow the command below it.
            s.make_targets = make_targets_in("\n".join(s.runs))
    return jobs


def parse_allowlist(text: str) -> tuple[dict[str, str], list[str]]:
    entries: dict[str, str] = {}
    problems: list[str] = []
    for n, raw in enumerate(text.splitlines(), 1):
        line = raw.strip()
        if not line or line.startswith("#"):
            continue
        name, _, reason = line.partition("#")
        name, reason = name.strip(), reason.strip()
        if not reason:
            problems.append(f"{ALLOWLIST}:{n}: `{name}` has no reason. Every exemption states why.")
        entries[name] = reason
    return entries, problems


def doc_commands(doc: str) -> set[str]:
    """Commands RELEASING.md names, read from code spans and fenced blocks only."""
    snippets = FENCED.findall(doc) + INLINE.findall(doc)
    commands: set[str] = set()
    for snippet in snippets:
        commands |= make_targets_in(snippet)
        commands |= set(SCRIPT_CALL.findall(snippet))
    return commands


def needs_closure(jobs: dict[str, Job], root: str, workflow: str = RELEASE_WORKFLOW) -> tuple[set[str], list[str]]:
    seen: set[str] = set()
    problems: list[str] = []
    stack = [root]
    while stack:
        name = stack.pop()
        if name in seen:
            continue
        seen.add(name)
        for dep in jobs[name].needs:
            if dep not in jobs:
                problems.append(
                    f"{workflow}: job `{name}` needs `{dep}`, which is not a job in this workflow."
                )
                continue
            stack.append(dep)
    return seen, problems


def path_hygiene(jobs: dict[str, Job], gated: set[str], workflow: str) -> list[str]:
    """The two one-line ways to keep a `needs:` while emptying it, on every job of a path.

    `continue-on-error` satisfies a dependent's `needs:` on failure; a status function in an
    `if:` drops the implicit "all needs succeeded". An ordinary predicate does neither and is
    refused by nothing here — a guard that forbade every `if:` would forbid exactly the ref
    guards this file's own prose recommends.
    """
    problems: list[str] = []
    for name in sorted(gated):
        job = jobs[name]
        if job.continue_on_error is not False:
            problems.append(
                f"{workflow}: job `{name}` carries `continue-on-error: {render(job.continue_on_error)}`."
                f" A dependent's `needs:` is satisfied when such a job fails, so every gate at or"
                f" below it passes on red."
            )
        if job.if_expr is not None and STATUS_OVERRIDE.search(job.if_expr):
            problems.append(
                f"{workflow}: job `{name}` carries `if: {job.if_expr}`. A job with `needs:`"
                f" runs only when they all succeeded — unless its `if:` calls a status function"
                f" (always/failure/cancelled/success), which drops that requirement. `if: always()`"
                f" on the publisher releases over a red gate and reads like a retry policy;"
                f" `success() || <anything>` does the same while looking careful."
            )
        elif job.if_expr in BLOCK_SCALARS:
            problems.append(
                f"{workflow}: job `{name}` carries a block-scalar `if:`, which this script"
                f" does not read — so whether it drops the `needs:` requirement cannot be decided"
                f" here. Write it inline. Unreadable is treated as unsafe on this path on purpose."
            )
    return problems


def check_apt_provenance(root: pathlib.Path) -> list[str]:
    """The apt mirror runs behind both release checks, and neither can be reduced to a no-op.

    Separate from the release-workflow checks above because it answers a different question.
    Those ask "does a tag wait for the tier"; this asks "does the OTHER publishing path ask
    where the release came from" — and a green answer there does not imply one here, which is
    exactly how the apt pool ended up as the one outward effect nothing gated (NIM-882).
    """
    workflow = root / APT_WORKFLOW
    if not workflow.is_file():
        return [
            f"{APT_WORKFLOW}: missing. If the apt mirror is gone, delete this check with it"
            f" on purpose; an absent file must not read as a satisfied one."
        ]
    try:
        jobs = parse_workflow(workflow.read_text())
    except ValueError as e:
        return [f"{APT_WORKFLOW}: {e}"]

    # The YAML can keep naming a script that is gone or unrunnable, and the workflow would
    # then fail at publish time rather than here — on the one event nobody is watching.
    problems: list[str] = []
    for name in MIRROR_CHECKS:
        script = root / name
        if not script.is_file():
            problems.append(f"{name}: missing, and {APT_WORKFLOW} runs it to decide whether to mirror.")
        elif not script.stat().st_mode & 0o111:
            problems.append(f"{name}: not executable, so the step running it fails rather than judging.")
    if problems:
        return problems

    mirrors = sorted(name for name, job in jobs.items() if job.steps_running(APT_PUBLISH_SCRIPT))
    if len(mirrors) != 1:
        return [
            f"{APT_WORKFLOW}: expected exactly one job that runs `{APT_PUBLISH_SCRIPT}`, found"
            f" {mirrors or 'none'}. This check anchors on the job that writes to the bucket, and"
            f" it cannot assert what comes before it without knowing which job that is."
        ]
    mirror = mirrors[0]

    gated, problems = needs_closure(jobs, mirror, APT_WORKFLOW)
    problems += path_hygiene(jobs, gated, APT_WORKFLOW)

    for check_script in MIRROR_CHECKS:
        runners = [name for name in sorted(gated) if jobs[name].steps_running(check_script)]
        if not runners:
            beside = sorted(name for name in jobs if jobs[name].steps_running(check_script))
            problems.append(
                f"{APT_WORKFLOW}: `{mirror}` does not need any job that runs `{check_script}`"
                + (f" — it runs in {beside}, beside the mirror rather than before it." if beside else ".")
                + f" This workflow fires on `release: published`, which includes a release a human"
                f" created by hand and assets a human uploaded onto one the pipeline made; without"
                f" both checks it mirrors them into the public apt pool signed with the apt key."
                f" There is no ruleset to fall back on — GitHub rulesets do not target releases."
            )
            continue

        for name in runners:
            for swallowed in jobs[name].swallows(check_script):
                problems.append(
                    f"{APT_WORKFLOW}: job `{name}` runs `{check_script}` where its failure cannot"
                    f" fail the step (`{swallowed}`). The command stays in the recipe and stops"
                    f" being a check, which is the cheapest way to pass this guard while"
                    f" publishing anything."
                )
            for step in jobs[name].steps_running(check_script):
                if step.if_expr is not None:
                    problems.append(
                        f"{APT_WORKFLOW}: the step running `{check_script}` in job `{name}`"
                        f" carries `if: {step.if_expr}`. A skipped step leaves the job green, so the"
                        f" mirror is gated only on the events that expression happens to admit."
                    )
                if step.continue_on_error is not False:
                    problems.append(
                        f"{APT_WORKFLOW}: the step running `{check_script}` in job `{name}`"
                        f" carries `continue-on-error: {render(step.continue_on_error)}`. The job then"
                        f" concludes success on a release the pipeline did not produce."
                    )
    return problems


def parse_gate_targets(makefile: str) -> list[str]:
    m = re.search(r"^RELEASE_GATE_TARGETS\s*:?=\s*((?:.*\\\n)*.*)$", makefile, re.M)
    return m.group(1).replace("\\\n", " ").split() if m else []


def check(root: pathlib.Path) -> list[str]:
    problems: list[str] = []

    gate_targets = parse_gate_targets((root / "Makefile").read_text())
    if not gate_targets:
        return [
            "Makefile: RELEASE_GATE_TARGETS is empty or absent. Every check below would then"
            " pass without asserting anything, which is the failure this guard exists to catch."
        ]

    try:
        jobs = parse_workflow((root / RELEASE_WORKFLOW).read_text())
    except ValueError as e:
        return [f"{RELEASE_WORKFLOW}: {e}"]

    publishers = sorted(name for name, job in jobs.items() if job.publishes())
    if len(publishers) != 1:
        return [
            f"{RELEASE_WORKFLOW}: expected exactly one job that runs goreleaser, found {publishers or 'none'}."
            f" This guard anchors on the publisher, and it cannot assert what comes before it"
            f" without knowing which job it is. If the release is produced some other way now,"
            f" teach PUBLISHER_ACTION about it on purpose."
        ]
    publisher = publishers[0]

    gated, closure_problems = needs_closure(jobs, publisher)
    problems += closure_problems

    for target in gate_targets:
        runners = sorted(name for name, job in jobs.items() if target in job.make_targets)
        if not runners:
            problems.append(
                f"{RELEASE_WORKFLOW}: nothing runs `make {target}`. It is in RELEASE_GATE_TARGETS,"
                f" so a tag is supposed to be blocked on it — and right now a tag is not."
            )
            continue
        if not any(r in gated for r in runners):
            problems.append(
                f"{RELEASE_WORKFLOW}: `make {target}` runs in {runners}, which `{publisher}` does"
                f" not need. The job runs beside the release instead of before it, so a red one"
                f" blocks nothing."
            )
            continue
        for name in runners:
            if name not in gated:
                continue
            for step in jobs[name].steps:
                if target not in step.make_targets:
                    continue
                if step.if_expr is not None:
                    problems.append(
                        f"{RELEASE_WORKFLOW}: the step running `make {target}` in job `{name}` carries"
                        f" `if: {step.if_expr}`. A skipped step leaves the job green, so the gate"
                        f" holds only on the events that expression happens to admit."
                    )
                if step.continue_on_error is not False:
                    problems.append(
                        f"{RELEASE_WORKFLOW}: the step running `make {target}` in job `{name}` carries"
                        f" `continue-on-error: {render(step.continue_on_error)}`. The job then concludes"
                        f" success on a red tier."
                    )

    # The whole needs-path, publisher included: one `if:` or continue-on-error anywhere on it
    # turns `needs:` into decoration, and both are one line.
    problems += path_hygiene(jobs, gated, RELEASE_WORKFLOW)

    # No job in this workflow may conclude before the gate does, and that is a stronger rule
    # than "the publisher waits". check-release-provenance.sh reads a run of this workflow as
    # "the gate passed" from the fact that no job in it failed and at least one succeeded —
    # sound only while nothing can go green ahead of the gate. Add a job that can, and a run
    # whose gate is still queued reads as green over in apt-publish.yml, a file away from
    # anything that mentions it. A job that NEEDS the gate is fine; a sibling is not.
    # Every job but the gate itself must transitively NEED it. An upstream job — one the gate
    # `needs:` — is not an exception: it is the clearest case of concluding green first.
    gate_runners = {name for name, job in jobs.items() if job.make_targets & set(gate_targets)}
    if gate_runners:
        early = sorted(
            name
            for name in jobs
            if name not in gate_runners and not (needs_closure(jobs, name, RELEASE_WORKFLOW)[0] & gate_runners)
        )
        if early:
            problems.append(
                f"{RELEASE_WORKFLOW}: job(s) {early} can conclude before {sorted(gate_runners)}."
                f" Besides running beside the release rather than behind it, this breaks an"
                f" inference made in another file: {PROVENANCE_SCRIPT} treats 'no job in the run"
                f" failed and one succeeded' as 'the gate passed', which holds only while nothing"
                f" can go green ahead of the gate. Put them behind the gate with `needs:` — or,"
                f" where the gate is what needs THEM and the edge cannot be inverted, teach that"
                f" script to name the gate job instead of reading the whole run."
            )

    problems += check_apt_provenance(root)

    allow, allow_problems = parse_allowlist((root / ALLOWLIST).read_text())
    problems += allow_problems

    named = doc_commands((root / RELEASE_DOC).read_text())
    for command in sorted(named - set(gate_targets) - set(allow)):
        problems.append(
            f"{RELEASE_DOC}: names `{command}`, and nothing runs it on a tag. Either add it to"
            f" RELEASE_GATE_TARGETS and to a job `{publisher}` needs, or add a line to {ALLOWLIST}"
            f" saying why it stays a human's step."
        )
    for command in sorted(set(allow) - named):
        problems.append(
            f"{ALLOWLIST}: exempts `{command}`, which {RELEASE_DOC} no longer names. A stale"
            f" exemption is how a step comes back unenforced later."
        )
    for command in sorted(set(allow) & set(gate_targets)):
        problems.append(
            f"{ALLOWLIST}: exempts `{command}`, which RELEASE_GATE_TARGETS also requires. One of the"
            f" two is wrong and the reader cannot tell which."
        )

    return problems


SELF_TEST_MAKEFILE = "RELEASE_GATE_TARGETS := live-gate-target\n"

# Carries every shape the checks are about: comments between jobs, an env block, step-level
# keys, a `uses:` publisher and a decoy-prone job key. A fixture thinner than the real file
# is how a guard passes its own tests and misses the file it guards.
SELF_TEST_WORKFLOW = """\
name: Release
on:
  push:
    tags:
      - "v*"
  workflow_dispatch:

env:
  GO_VERSION: "1.26.6"

jobs:
  # the gate
  live-gate:
    name: make live-gate-target (blocking)
    runs-on: ubuntu-latest
    timeout-minutes: 90
    steps:
      - name: Checkout
        uses: actions/checkout@v7
        with:
          fetch-depth: 0

      - name: make live-gate-target
        env:
          TMPDIR: /tmp/x
        run: |
          set -euo pipefail
          make live-gate-target

# a comment between jobs
  release:
    name: goreleaser
    needs: live-gate
    runs-on: ubuntu-latest
    steps:
      - name: Run GoReleaser
        uses: goreleaser/goreleaser-action@v6
        with:
          args: release --clean
"""

# The second publishing path, in the shape the checks about it are written against: the
# mirror behind a provenance job, the check invoked as a command rather than named.
SELF_TEST_APT = """\
name: Publish apt (R2)
on:
  release:
    types: [published]
  workflow_dispatch:

permissions:
  contents: read
  actions: read

jobs:
  provenance:
    name: the release came from the gated pipeline
    runs-on: ubuntu-latest
    steps:
      - name: Checkout
        uses: actions/checkout@v7

      - name: Require a green release run
        env:
          GH_TOKEN: x
        run: scripts/check-release-provenance.sh

  publish:
    name: mirror .deb
    runs-on: ubuntu-latest
    needs: provenance
    steps:
      - name: Verify the assets
        run: scripts/verify-release-assets.sh

      - name: Publish to R2
        run: deploy/apt-r2/publish-apt.sh
"""

SELF_TEST_DOC = (
    "### (e) gate\n\n"
    "Run `make by-hand` and make sure it is green.\n\n"
    "```sh\n"
    "make live-gate-target\n"
    "scripts/by-hand.sh --context\n"
    "deploy/x/publish.sh\n"
    "```\n"
)

# A second publishing-shaped job, appended by the cases that need one. Each is green only
# because `publishes()` distinguishes it from a release; delete that distinction and the
# count becomes two, which is what makes these cases mutation-tests rather than decoration.
#
# Each carries `needs: live-gate`, which is not incidental: a job in this workflow that is
# NOT behind the gate is refused on its own (see the needs-path check), because the run's
# jobs are what check-release-provenance.sh reads to decide the gate passed. So these three
# model the shape a maintainer should write, and the case below models the one they should not.
SECOND_JOB = {
    "goreleaser check": "\n  validate:\n    name: config check\n    needs: live-gate\n    runs-on: ubuntu-latest\n    steps:\n      - uses: goreleaser/goreleaser-action@v6\n        with:\n          args: check\n",
    "install only": "\n  tools:\n    name: install goreleaser\n    needs: live-gate\n    runs-on: ubuntu-latest\n    steps:\n      - uses: goreleaser/goreleaser-action@v6\n        with:\n          install-only: true\n",
    "look-alike": "\n  fork:\n    name: a different action\n    needs: live-gate\n    runs-on: ubuntu-latest\n    steps:\n      - uses: my-org/goreleaser/goreleaser-action-fork@v1\n        with:\n          args: release --clean\n",
}

SELF_TEST_ALLOW = (
    "by-hand  # a human runs this, and here is the reason\n"
    "scripts/by-hand.sh  # step (c3), a human's\n"
    "deploy/x/publish.sh  # a script outside scripts/ is still a release command\n"
)


def self_test() -> int:
    """Cases expecting a problem are one edit from the baseline, in the direction that hides.

    The ones expecting silence are positive controls: they assert the parser READS a shape
    it once dropped, and a dropped shape shows up as a plausible-sounding problem rather
    than as a crash — which is how "nothing runs `make …`" got said about a file that does.
    """
    W = "m:" + RELEASE_WORKFLOW
    D = "m:" + RELEASE_DOC
    A = "m:" + APT_WORKFLOW
    cases: list[tuple[str, dict, str]] = [
        ("baseline", {}, ""),
        # The apt mirror (NIM-882). Each edit is the one a tidy-up would make.
        ("the mirror no longer waits for provenance",
         {A: ("    needs: provenance\n", "")}, "does not need any job that runs"),
        ("the provenance check is named but not run",
         {A: ("        run: scripts/check-release-provenance.sh\n", "        run: echo scripts/check-release-provenance.sh\n")},
         "does not need any job that runs"),
        ("the provenance step is skipped on dispatch",
         {A: ("      - name: Require a green release run\n", "      - name: Require a green release run\n        if: github.event_name != 'workflow_dispatch'\n")},
         "carries `if:"),
        ("the provenance step cannot fail",
         {A: ("        env:\n          GH_TOKEN: x\n", "        continue-on-error: true\n        env:\n          GH_TOKEN: x\n")},
         "concludes success on a release the pipeline did not produce"),
        ("the provenance job cannot fail",
         {A: ("    name: the release came from the gated pipeline\n", "    name: the release came from the gated pipeline\n    continue-on-error: true\n")},
         "passes on red"),
        ("the mirror runs whatever provenance concluded",
         {A: ("    needs: provenance\n", "    needs: provenance\n    if: always()\n")}, "status function"),
        ("nothing mirrors to the bucket any more",
         {A: ("        run: deploy/apt-r2/publish-apt.sh\n", "        run: echo done\n")},
         "expected exactly one job that runs"),
        ("a ref guard on the mirror is allowed",
         {A: ("    needs: provenance\n", "    needs: provenance\n    if: github.event_name == 'release'\n")}, ""),
        ("the provenance script is gone", {PROVENANCE_SCRIPT: None}, "missing, and"),
        ("the asset verifier is gone", {MIRROR_CHECKS[1]: None}, "verify-release-assets.sh: missing"),
        ("the mirror does not verify the assets",
         {A: ("        run: scripts/verify-release-assets.sh\n", "        run: echo skip\n")},
         "does not need any job that runs `scripts/verify-release-assets.sh`"),
        # The one-line defeat shell_commands() cannot see: the call stays, the failure does not.
        ("the provenance check runs but cannot fail the step",
         {A: ("        run: scripts/check-release-provenance.sh\n", "        run: scripts/check-release-provenance.sh || true\n")},
         "its failure cannot fail the step"),
        ("the asset verifier runs under set +e",
         {A: ("        run: scripts/verify-release-assets.sh\n", "        run: |\n          set +e\n          scripts/verify-release-assets.sh\n")},
         "its failure cannot fail the step"),
        ("a pipe swallows too — the runner's bash has no pipefail",
         {A: ("        run: scripts/verify-release-assets.sh\n", "        run: scripts/verify-release-assets.sh | tee verify.log\n")},
         "its failure cannot fail the step"),
        ("a command in an `if` condition is not a set -e trigger",
         {A: ("        run: scripts/verify-release-assets.sh\n", "        run: |\n          if ! scripts/verify-release-assets.sh; then echo soft; fi\n")},
         "its failure cannot fail the step"),
        ("`set +ex` relaxes errexit just as `set +e` does",
         {A: ("        run: scripts/verify-release-assets.sh\n", "        run: |\n          set +ex\n          scripts/verify-release-assets.sh\n")},
         "its failure cannot fail the step"),
        # The gate's own upstream is the clearest case of concluding green first, and the
        # first version of this rule exempted exactly it.
        ("a job the GATE needs concludes before the gate",
         {W: ("  live-gate:\n    name: make live-gate-target (blocking)\n", "  prep:\n    name: prepare\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n\n  live-gate:\n    name: make live-gate-target (blocking)\n    needs: prep\n")},
         "can conclude before"),
        ("`&&` does not swallow the way `||` does",
         {A: ("        run: scripts/verify-release-assets.sh\n", "        run: scripts/verify-release-assets.sh && echo mirrored\n")}, ""),
        ("the apt workflow is gone", {APT_WORKFLOW: None}, "apt-publish.yml: missing"),
        # A job beside the gate rather than behind it — green on its own terms, and it makes
        # check-release-provenance.sh's "no job failed" inference unsound one file away.
        ("a release job that is not behind the gate",
         {W: ("  release:\n    name: goreleaser\n", "  extra:\n    name: a job beside the gate\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n\n  release:\n    name: goreleaser\n")},
         "can conclude before ['live-gate']"),
        ("a sibling that NEEDS the gate is fine",
         {W: ("  release:\n    name: goreleaser\n", "  extra:\n    name: a job behind the gate\n    needs: live-gate\n    runs-on: ubuntu-latest\n    steps:\n      - run: echo hi\n\n  release:\n    name: goreleaser\n")},
         ""),
        # Written without a shebang, which the fixture writer takes as "leave it unexecutable" —
        # the state a `git add` of a new script lands in by default.
        ("the provenance script is not executable", {PROVENANCE_SCRIPT: "exit 0\n"}, "not executable"),
        ("empty RELEASE_GATE_TARGETS", {"Makefile": "RELEASE_GATE_TARGETS :=\n"}, "is empty"),
        ("the gate job is not needed", {W: ("    needs: live-gate\n", "")}, "does not need"),
        ("the publisher runs anyway", {W: ("    needs: live-gate\n", "    needs: live-gate\n    if: always()\n")}, "carries `if: always"),
        ("`success() || …` defeats needs while looking careful",
         {W: ("    needs: live-gate\n", "    needs: live-gate\n    if: success() || github.event_name == 'workflow_dispatch'\n")},
         "status function"),
        ("a block-scalar if: cannot be judged, so it is refused",
         {W: ("    needs: live-gate\n", "    needs: live-gate\n    if: >-\n      startsWith(github.ref, 'refs/tags/v')\n")},
         "block-scalar `if:`"),
        ("the gate job cannot fail", {W: ("    timeout-minutes: 90\n", "    continue-on-error: true\n")}, "passes on red"),
        ("job coe true, a later step says false",
         {W: ("    timeout-minutes: 90\n", "    continue-on-error: true\n"),
          "m2:" + RELEASE_WORKFLOW: ("        env:\n", "        continue-on-error: false\n        env:\n")},
         "passes on red"),
        # Expects the rendered value, not the shared "passes on red": with the `#` strip in
        # truthiness() deleted this becomes `unknown`, which is also a problem — so the
        # weaker expectation pinned nothing about the comment.
        ("coe hidden behind a trailing comment", {W: ("    timeout-minutes: 90\n", "    continue-on-error: true  # runner flake\n")}, "continue-on-error: true"),
        ("coe behind an expression", {W: ("    timeout-minutes: 90\n", "    continue-on-error: ${{ github.event_name == 'push' }}\n")}, "continue-on-error: unknown"),
        ("the gate step is skipped on dispatch", {W: ("      - name: make live-gate-target\n", "      - name: make live-gate-target\n        if: github.event_name != 'workflow_dispatch'\n")}, "carries `if:"),
        ("the gate step cannot fail", {W: ("        env:\n          TMPDIR: /tmp/x\n", "        continue-on-error: true\n        env:\n          TMPDIR: /tmp/x\n")}, "concludes\n success on a red tier"),
        ("the target is only in a job name", {W: ("          make live-gate-target\n", "          echo nothing\n")}, "nothing runs `make live-gate-target`"),
        ("the target is only echoed, not run",
         {W: ("          make live-gate-target\n", "          echo make live-gate-target\n")},
         "nothing runs `make live-gate-target`"),
        ("the target is only named in a comment",
         {W: ("          make live-gate-target\n", "          # make live-gate-target\n          true\n")},
         "nothing runs `make live-gate-target`"),
        ("a decoy job wears the publisher's name",
         {W: ("  release:\n    name: goreleaser\n    needs: live-gate\n",
              "  release:\n    name: decoy\n    needs: live-gate\n    steps:\n      - run: echo nothing\n\n  publish:\n    name: goreleaser\n")},
         "does not need"),
        ("nothing publishes at all",
         {W: ("        uses: goreleaser/goreleaser-action@v6\n", "        uses: actions/checkout@v7\n")},
         "exactly one job that runs goreleaser"),
        ("installing goreleaser is not publishing it",
         {W: ("          make live-gate-target\n", "          go install github.com/goreleaser/goreleaser/v2@latest\n          make live-gate-target\n")},
         ""),
        # Each second-job case is green only because publishes() tells that job apart from a
        # release. Put the decoy in the SAME job and it is green whatever publishes() does —
        # the first step already answered. These sit in their own job so deleting the
        # distinction makes the count two and reddens.
        ("`goreleaser check` in its own job is not a second publisher",
         {W: ("          args: release --clean\n", "          args: release --clean\n" + SECOND_JOB["goreleaser check"])},
         ""),
        ("`install-only: true` is not a second publisher",
         {W: ("          args: release --clean\n", "          args: release --clean\n" + SECOND_JOB["install only"])},
         ""),
        ("an action whose path merely CONTAINS the publisher's is not one",
         {W: ("          args: release --clean\n", "          args: release --clean\n" + SECOND_JOB["look-alike"])},
         ""),
        ("a quoted args: still names the release",
         {W: ("          args: release --clean\n", "          args: \"release --clean\"\n")}, ""),
        ("an args: outside `with:` does not overwrite the publisher's",
         {W: ("        with:\n          args: release --clean\n", "        with:\n          args: release --clean\n        env:\n          args: not-a-release\n")},
         ""),
        ("a new doc step nobody enforces", {D: ("make live-gate-target\n", "make live-gate-target\nmake newly-added\n")}, "nothing runs it on a tag"),
        ("a new script step nobody enforces", {D: ("scripts/by-hand.sh --context\n", "scripts/by-hand.sh --context\nscripts/newly-added.sh\n")}, "nothing runs it on a tag"),
        ("an exemption with no reason", {ALLOWLIST: "by-hand\nscripts/by-hand.sh  # r\n"}, "has no reason"),
        ("a stale exemption", {ALLOWLIST: SELF_TEST_ALLOW + "gone  # reason\n"}, "no longer names"),
        ("an exemption that contradicts the gate list", {ALLOWLIST: SELF_TEST_ALLOW + "live-gate-target  # reason\n"}, "also requires"),
        ("needs a job that does not exist", {W: ("    needs: live-gate\n", "    needs: ghost\n")}, "not a job in this workflow"),
        ("a second command on the line is still read",
         {D: ("make live-gate-target\n", "make live-gate-target; make also-new\n")}, "names `also-new`"),
        ("a ref guard on the publisher is allowed",
         {W: ("    needs: live-gate\n", "    needs: live-gate\n    if: startsWith(github.ref, 'refs/tags/v')\n")}, ""),
        ("a block-style needs list is read",
         {W: ("    needs: live-gate\n", "    needs:\n      - \"live-gate\"\n")}, ""),
        ("steps with the dash at the key's indent are read",
         {W: ("    steps:\n      - name: Run GoReleaser\n        uses: goreleaser/goreleaser-action@v6\n        with:\n          args: release --clean\n",
              "    steps:\n    - name: Run GoReleaser\n      uses: goreleaser/goreleaser-action@v6\n      with:\n        args: release --clean\n")}, ""),
        ("make -C does not hide the target",
         {W: ("          make live-gate-target\n", "          make -C . VAR=1 live-gate-target\n")}, ""),
        ("a leading assignment and a leader do not hide it",
         {W: ("          make live-gate-target\n", "          TMPDIR=/tmp time make live-gate-target\n")}, ""),
        ("`bash -c` does not hide it",
         {W: ("          make live-gate-target\n", "          bash -c \"make live-gate-target\"\n")}, ""),
        ("a trailing comment does not eat the command before it",
         {W: ("          make live-gate-target\n", "          make live-gate-target  # the gate\n")}, ""),
        # The `#` must be cut per line BEFORE the separators; the other order made the text
        # after the `;` its own comment-free segment and reported the gate as running.
        ("a commented-out call after a separator does not count",
         {W: ("          make live-gate-target\n", "          echo skip # ; make live-gate-target\n")},
         "nothing runs `make live-gate-target`"),
        ("a sentence ending in a target is not a new target",
         {D: ("Run `make by-hand` and", "Run `make by-hand` and then `make by-hand.` and")}, ""),
    ]

    failures = 0
    for label, edits, expect in cases:
        with tempfile.TemporaryDirectory() as td:
            rootdir = pathlib.Path(td)
            files = {
                "Makefile": SELF_TEST_MAKEFILE,
                RELEASE_WORKFLOW: SELF_TEST_WORKFLOW,
                APT_WORKFLOW: SELF_TEST_APT,
                **{s: "#!/usr/bin/env bash\nexit 0\n" for s in MIRROR_CHECKS},
                RELEASE_DOC: SELF_TEST_DOC,
                ALLOWLIST: SELF_TEST_ALLOW,
            }
            for key, value in edits.items():
                if key.startswith(("m:", "m2:")):
                    path = key.split(":", 1)[1]
                    old, new = value
                    if old not in files[path]:
                        print(f"self-test: {label}: fixture edit does not apply ({old!r})")
                        failures += 1
                    files[path] = files[path].replace(old, new, 1)
                elif value is None:
                    files.pop(key, None)
                else:
                    files[key] = value
            for path, body in files.items():
                p = rootdir / path
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_text(body)
                # A shebang is the fixture's way of saying "and it is runnable"; a body
                # without one is left 0644 so the missing-exec-bit case has a shape to be.
                if p.suffix == ".sh" and body.startswith("#!"):
                    p.chmod(0o755)

            got = check(rootdir)
            joined = " ".join(" ".join(got).split())
            if expect == "":
                if got:
                    print(f"self-test: {label}: expected no problem, got:\n  " + "\n  ".join(got))
                    failures += 1
            elif " ".join(expect.split()) not in joined:
                print(f"self-test: {label}: expected a problem containing {expect!r}, got:\n  " + ("\n  ".join(got) or "(none)"))
                failures += 1

    if failures:
        print(f"check-release-gate: self-test FAILED ({failures} case(s))")
        return 1
    print(f"check-release-gate: self-test OK ({len(cases)} cases)")
    return 0


def main() -> int:
    if "--self-test" in sys.argv[1:]:
        return self_test()

    root = pathlib.Path(__file__).resolve().parent.parent
    problems = check(root)
    if problems:
        print("check-release-gate: the release procedure asserts steps the pipeline does not enforce:")
        for p in problems:
            print(f"  {p}")
        print("")
        print("  A blocking step nothing executes is not a gate. That is NIM-879 — `make")
        print("  e2e-live-gate` was blocking on paper while the tier ran in no CI at all, and")
        print("  stayed red on the release tip for a month with nobody able to notice.")
        return 1
    print("check-release-gate: every command RELEASING.md names is either gated on a tag or exempted with a reason")
    return 0


if __name__ == "__main__":
    sys.exit(main())
