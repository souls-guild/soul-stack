#!/usr/bin/env python3
"""Aggregate the raw runs bench.sh wrote into the tables README.md describes.

Usage: report.py <results dir>   (the <commit> directory bench.sh prints at the end)

Every number is derived here from what bench.sh saved per run — the run's apply_runs
rows and audit timestamps (run.json) and metric snapshots taken before and after it —
so a table can be recomputed from the raw directory without re-running anything.
Output is deterministic for a given directory.
"""
import json
import math
import os
import re
import statistics
import sys

CELL_RE = re.compile(r"^(?P<model>staged|linear)-a(?P<acol>\d+)-d(?P<delay>\d+)-(?P<target>h\d+|push)$")
NON_HOST_SIDS = {"keeper", "__run__"}
# The barrier polls apply_runs on a ticker (keeper/internal/scenario/scenario.go
# defaultPollInterval) started once the whole wave was inserted and sent row by row
# (dispatch.go dispatchWave, then failure_policy.go waitStep), so a step is seen done
# only on the first tick after its slowest host finished. The tick's origin is taken as
# the step's last row insert.
POLL_MS = 200.0


def prom_sum(path, name, label=None):
    """Sum of every sample of metric `name` (optionally only lines containing `label`)."""
    total = 0.0
    if not os.path.exists(path):
        return None
    with open(path) as f:
        for line in f:
            metric = line.split("{", 1)[0].split(" ", 1)[0]
            if metric != name or (label and label not in line):
                continue
            total += float(line.rsplit(" ", 1)[1])
    return total


def read_int(path):
    try:
        with open(path) as f:
            return int(f.read().strip())
    except (OSError, ValueError):
        return None


def delta(run_dir, suffix, name, label=None):
    a = prom_sum(os.path.join(run_dir, "pre." + suffix), name, label)
    b = prom_sum(os.path.join(run_dir, "post." + suffix), name, label)
    return None if a is None or b is None else b - a


def mean(xs):
    return statistics.fmean(xs) if xs else None


def analyse_run(run_dir, target):
    with open(os.path.join(run_dir, "run.json")) as f:
        run = json.load(f)
    completed = run.get("completed") or {}
    rows = [r for r in run["rows"] if r["sid"] not in NON_HOST_SIDS and r["finished_ms"] is not None]
    hosts = sorted({r["sid"] for r in rows})
    steps = sorted({r["passage"] for r in rows})
    dispatch = {p: min(float(r["started_ms"]) for r in rows if r["passage"] == p) for p in steps}
    sent = {p: max(float(r["started_ms"]) for r in rows if r["passage"] == p) for p in steps}
    done = {p: max(float(r["finished_ms"]) for r in rows if r["passage"] == p) for p in steps}
    started, finished = float(run["started_ms"]), float(run["completed_ms"])
    out = {
        "status": completed.get("status"),
        "model": completed.get("execution_model"),
        "wall": finished - started,
        "steps": len(steps),
        "messages": len(rows),
        "msgs_per_host": len(rows) / len(hosts) if hosts else None,
        "lead": dispatch[steps[0]] - started if steps else None,
        "step": mean([done[p] - dispatch[p] for p in steps]),
        "gap": mean([dispatch[b] - done[a] for a, b in zip(steps, steps[1:])]),
        # Keeper's own work between two steps: next dispatch minus the first tick that
        # could see the step done (the immediate poll at the origin never can).
        "work": mean([dispatch[b] - sent[a] - POLL_MS * max(1, math.ceil((done[a] - sent[a]) / POLL_MS))
                      for a, b in zip(steps, steps[1:])]),
        "inline_rows": sum(1 for r in rows if r["claim_ms"] is None),
        "tail": finished - done[steps[-1]] if steps else None,
        "rt": mean([float(r["finished_ms"]) - float(r["started_ms"]) for r in rows]),
        "queue": mean([float(r["claim_ms"]) - float(r["started_ms"]) for r in rows if r["claim_ms"] is not None]),
        "renders": delta(run_dir, "keeper.prom", "keeper_render_duration_seconds_count"),
        "render_ms": None,
        "vault_reads": None,
        "soul_run": None,
        "outside": None,
        "ssh": None,
    }
    rsum = delta(run_dir, "keeper.prom", "keeper_render_duration_seconds_sum")
    out["render_ms"] = None if rsum is None else rsum * 1000
    va, vb = read_int(os.path.join(run_dir, "pre.vault")), read_int(os.path.join(run_dir, "post.vault"))
    out["vault_reads"] = None if va is None or vb is None else vb - va
    if target == "push":
        sa, sb = read_int(os.path.join(run_dir, "pre.sshd")), read_int(os.path.join(run_dir, "post.sshd"))
        out["ssh"] = None if sa is None or sb is None else sb - sa
    else:
        s_sum = s_cnt = 0.0
        for name in os.listdir(run_dir):
            if name.startswith("pre.soul.") and name.endswith(".prom"):
                c = name[len("pre.soul."):-len(".prom")]
                ds = delta(run_dir, "soul." + c + ".prom", "soul_apply_duration_seconds_sum")
                dc = delta(run_dir, "soul." + c + ".prom", "soul_apply_duration_seconds_count")
                if ds is not None and dc:
                    s_sum += ds
                    s_cnt += dc
        if s_cnt:
            out["soul_run"] = s_sum / s_cnt * 1000
            queue = out["queue"] or 0.0
            out["outside"] = out["rt"] - queue - out["soul_run"]
    return out


def pct(xs, q):
    xs = sorted(xs)
    if not xs:
        return None
    k = (len(xs) - 1) * q
    lo, hi = int(k), min(int(k) + 1, len(xs) - 1)
    return xs[lo] + (xs[hi] - xs[lo]) * (k - lo)


def fmt(x, digits=0):
    if x is None:
        return "–"
    return f"{x:.{digits}f}"


def load(root):
    cells = {}
    for name in sorted(os.listdir(root)):
        m = CELL_RE.match(name)
        if not m:
            continue
        cell_dir = os.path.join(root, name)
        runs = []
        for rep in sorted(os.listdir(cell_dir)):
            if rep == "w" or not os.path.exists(os.path.join(cell_dir, rep, "run.json")):
                continue
            runs.append(analyse_run(os.path.join(cell_dir, rep), m["target"]))
        key = (m["model"], int(m["acol"]), int(m["delay"]), m["target"])
        cells[key] = runs
    return cells


def agg(runs, field):
    xs = [r[field] for r in runs if r[field] is not None]
    return (statistics.median(xs) if xs else None), pct(xs, 0.9)


def target_order(t):
    return (1, 0) if t == "push" else (0, int(t[1:]))


def main():
    root = sys.argv[1]
    with open(os.path.join(root, "meta.json")) as f:
        meta = json.load(f)
    cells = load(root)
    delays = sorted({k[2] for k in cells})
    targets = sorted({k[3] for k in cells}, key=target_order)
    acols = sorted({k[1] for k in cells})
    models = [m for m in ("staged", "linear") if any(k[0] == m for k in cells)]

    print(f"# Linear-dispatch benchmark — commit `{meta['commit']}`\n")
    print(f"Fleet sizes `{meta['fleets']}`, added delay `{meta['delays_ms']}` ms, "
          f"{meta['reps']} measured runs per cell after one warm-up, {meta['cpus']} CPUs.")
    rtts = []
    for d in delays:
        p = os.path.join(root, f"rtt-d{d}.txt")
        if os.path.exists(p):
            with open(p) as f:
                v = f.read().strip()
            if v:
                rtts.append(f"{d} ms → {float(v) * 1000:.1f} ms")
    if rtts:
        print("Measured TCP connect time Soul→Keeper by added delay: " + ", ".join(rtts) + ".")

    # A run counts only if it succeeded in the model its cell configured; a queue cell
    # whose rows carry no claim was dispatched inline and is marked, not relabelled.
    raw, cells = cells, {k: [r for r in v if r["status"] == "success" and r["model"] == k[0]] for k, v in cells.items()}
    inline_in_queue = {k for k, v in cells.items() if k[1] > 0 and any(r["inline_rows"] for r in v)}
    problems = []
    for key, runs in sorted(raw.items()):
        name = f"{key[0]}-a{key[1]}-d{key[2]}-{key[3]}"
        bad = [r for r in runs if r not in cells[key]]
        if bad:
            why = sorted({f"status {r['status']}, model {r['model']}" for r in bad})
            problems.append(f"`{name}`: {len(bad)} of {len(runs)} runs excluded ({'; '.join(why)})")
        if key in inline_in_queue:
            n = sum(r["inline_rows"] for r in cells[key])
            total = sum(r["messages"] for r in cells[key])
            problems.append(f"`{name}`: {n} of {total} messages were sent inline, not claimed from the queue (†)")
    if problems:
        print("\n**Excluded or marked** — the tables below count only the remaining runs:\n")
        for p in problems:
            print(f"- {p}")

    path = {0: "inline"}
    print("\n## Run wall clock, ms — median (p90)\n")
    cols = [(m, a) for a in acols for m in models]
    print("| delay | target | " + " | ".join(f"{m} {path.get(a, 'queue')}" for m, a in cols) + " |")
    print("|---|---|" + "---|" * len(cols))
    for d in delays:
        for t in targets:
            vals = []
            for m, a in cols:
                med, p90 = agg(cells.get((m, a, d, t), []), "wall")
                mark = " †" if (m, a, d, t) in inline_in_queue else ""
                vals.append("–" if med is None else f"{fmt(med)} ({fmt(p90)}){mark}")
            print(f"| {d} | {t} | " + " | ".join(vals) + " |")

    print("\n## Where the time goes — medians, ms\n")
    print("`lead` start→first dispatch · `step` dispatch→slowest host done · `gap` one step done→next dispatched · "
          f"`work` gap less the wait for the barrier's {POLL_MS:.0f} ms tick · "
          "`tail` last step done→terminal · `rt` one message, Keeper row insert→result · `queue` row→claim · "
          "`soul_run` Soul's Run per message · `outside` rt − queue − soul_run\n")
    fields = ["steps", "msgs_per_host", "lead", "step", "gap", "work", "tail", "rt", "queue", "soul_run", "outside"]
    print("| model | path | delay | target | " + " | ".join(fields) + " |")
    print("|---|---|---|---|" + "---|" * len(fields))
    for (m, a, d, t), runs in sorted(cells.items(), key=lambda kv: (kv[0][1], kv[0][2], target_order(kv[0][3]), kv[0][0])):
        if not runs:
            continue
        vals = [fmt(agg(runs, f)[0], 1 if f in ("msgs_per_host",) else 0) for f in fields]
        mark = " †" if (m, a, d, t) in inline_in_queue else ""
        print(f"| {m} | {path.get(a, 'queue')}{mark} | {d} | {t} | " + " | ".join(vals) + " |")

    print("\n## Keeper render and Vault per run — medians\n")
    print("| model | path | target | renders | render ms | Vault reads (audit) | SSH sessions |")
    print("|---|---|---|---|---|---|---|")
    d0 = delays[0] if delays else 0
    for (m, a, d, t), runs in sorted(cells.items(), key=lambda kv: (kv[0][1], target_order(kv[0][3]), kv[0][0])):
        if d != d0 or not runs:
            continue
        mark = " †" if (m, a, d, t) in inline_in_queue else ""
        print(f"| {m} | {path.get(a, 'queue')}{mark} | {t} | {fmt(agg(runs, 'renders')[0])} | {fmt(agg(runs, 'render_ms')[0], 1)} | "
              f"{fmt(agg(runs, 'vault_reads')[0])} | {fmt(agg(runs, 'ssh')[0])} |")


if __name__ == "__main__":
    main()
