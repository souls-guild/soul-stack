# redis — the vendored schema document

The redis SoulModule lives in its own repository since NIM-868:

    https://github.com/soul-stack-plugin/redis

What is left here is `schema.json` and nothing else — the document that artifact
publishes, vendored, at the commit `scripts/plugin-source.sh` pins.

## Why a copy stayed behind when mongo's did not

`soul-lint` checks a plugin step's `params:` against the module's schema document, and
it can only do that where both halves are present. When mongo left in NIM-825 its
corpus came off the scenario lint entirely, and the cost was recorded as such: nothing
checks the `params:` of the `mongo.*` steps in `examples/service/mongo`.

Redis is eleven steps, not one — six `redis.*` addresses in `examples/service/redis`
and five in `examples/service/dragonfly`, which is served by the same artifact. Taking
eleven off the lint to match one was refused; the document is vendored instead, and
`LINT_MODULES_REDIS` keeps pointing here.

## What keeps it honest

A vendored copy is worth exactly as much as the check that it is still the original.
`make check-plugin-schema` builds the artifact from the pinned commit, runs the real
`soul-mod stamp` over it, and refuses if what the artifact derives is not byte-identical
to this file. So the copy cannot go stale silently: it goes stale loudly, in the gate,
and the remedy is to re-vendor the document and move the pin in the same commit.

**Do not hand-edit this file.** It is generated from `module.Def` values in Go (NIM-377).
To take a change from the plugin repository:

```sh
make plugin-schema-vendor      # re-vendor from the pinned commit
```

and bump the pin in `scripts/plugin-source.sh` if the change is newer than it.
