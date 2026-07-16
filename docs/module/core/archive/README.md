# core.archive

Unpacking archives into a directory. **Soul-side**, statically built into the `soul` binary.
Implementation - [`soul/internal/coremod/archive/archive.go`](../../../../soul/internal/coremod/archive/archive.go).

Supported formats: **tar** / **tar.gz** (`.tgz`) / **tar.bz2** (`.tbz2`) /
**zip**. `format` is optional - auto-detect by extension `path`. Unpacking in progress
**in-process** using Go stdlib (`archive/tar`, `archive/zip`,
`compress/gzip`, `compress/bzip2`) - without external utilities (`tar` / `unzip`) and without
spawning subprocesses. This removes the dependence on host binaries and gives
per-entry security control (zip-slip / zip-bomb / symlink-policy),
not available to backend utilities. tar.bz2 - unpacking only (bzip2 to stdlib
decompress-only; this is enough for MVP).

## States

| State | Destination | Idempotency (when `changed=true`) |
|---|---|---|
| `extracted` | The `path` archive has been unpacked into the `dest` directory. | After unpacking, the SHA-256 of the source archive is written to `<dest>/.soul-archive.sha256`. `changed=true`, if there is no marker or its hash ≠ the hash of the current archive. Matched - `changed=false` (no-op). This is a grounded check "the archive is the same", and not "all files inside `dest` are in place". |

## extracted — params

| Param | Type | Required/default | Meaning |
|---|---|---|---|
| `path` | string | required | Path to the source archive. |
| `dest` | string | required | Destination directory. Created (`MkdirAll`, mode `0755`) if does not exist. |
| `format` | string | optional (default - auto-detect) | Forced format: `tar` / `tar.gz` (`tgz`) / `tar.bz2` (`tbz2`) / `zip`. Empty/not specified → format is determined by the suffix `path`; if the suffix is ​​not recognized, the step drops (`cannot auto-detect format`). |
| `max_size` | string | optional (default `1GiB`) | Ceiling of **total unpacked** size (zip-bomb protection). Naked number = bytes, or a number with a binary suffix `KiB` / `MiB` / `GiB` (suffix case is not important). Decimal SI suffixes (`KB` / `MB` / `GB`) and fractions are **not supported** - unrecognized suffix or garbage → a clear configuration error (`invalid size`), not a silent tail drop. The value `≤ 0` is also rejected. Excess → step falls (`exceeded max_size`). There is no ratio limit - only an absolute ceiling. |
| `max_entries` | integer | optional (default `100000`) | Limit on the number of records in the archive (zip-bomb protection). Excess → step falls (`exceeded max_entries`). |

## Capabilities / side-effects

- **Changes the file system:** creates a directory `dest`, unpacks it into
archive contents, writes the marker `.soul-archive.sha256` to `dest`. For system
paths requires appropriate rights (in practice - root, see.
  [`run_as_root`](../../../naming-rules.md#required_capabilities-enum)).
- **Does not spawn subprocesses.** Unpacking - in-process on Go stdlib; manifesto
declares only [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum),
`exec_subprocess` **removed** (external `tar`/`unzip` are no longer called).
Host decompression utilities are not needed.
- The marker file is **inside** `dest` - take it into account during subsequent verification
directory contents using other steps.

## Output / register

`extracted` returns `{ path, dest, sha256, extracted: true }`, where `sha256` —
hash of the source archive (aka the contents of the marker). On no-op (hash matched) set
fields are the same, only `changed=false` is different.

## Example

```yaml
# Unpack the downloaded tarball into a directory. format auto-detect by .tar.gz.
# Idempotent: core.archive writes a marker and does not unpack the same archive again.
- name: Extract node_exporter tarball
  module: core.archive.extracted
  params:
    path: "${ '/tmp/node_exporter-' + input.version + '.tar.gz' }"
    dest: "${ '/tmp/node_exporter-' + input.version }"
```

(from [`examples/destiny/node-exporter/tasks/install.yml`](../../../../examples/destiny/node-exporter/tasks/install.yml))

## Security

In-process unpacking gives per-entry control over each archive entry before it
materialization. The invariants below are strict - the behavior of the backend utility is not
are determined and (except for zip-bomb limits) are not disabled by flags.

- **zip-slip / path-traversal - fail-fast.** For each entry, the target path
is built via [`filepath-securejoin`](https://github.com/cyphar/filepath-securejoin)
regarding `dest`, plus lexical escape detection. Entry with `..` either
in an absolute way, leading beyond `dest`, → step **falls entirely**
(`archive: entry %q escapes dest`), already unpacked files remain, marker
`.soul-archive.sha256` **not** written (retry is not considered successful). This
fail-fast, but **not** quiet clamp: outward recording is not created either in `dest` or outside
him.
- **zip-bomb - absolute limits.** The total unpacked size is limited
`max_size` (default `1GiB`), number of records - `max_entries` (default `100000`).
The size is calculated using `io.LimitReader` for each record + accumulator;
exceeding any limit → `failed` indicating which limit is broken
(`exceeded max_size` / `exceeded max_entries`). Ratio limit (compression rate)
no - only absolute ceilings. Both are configurable with parameters.
- **symlink — within-dest only.** Symlink from the archive is created **only** if it
target (resolved relative to the symlink directory itself) remains inside
`dest`. Absolute target or relative, outputting beyond `dest`, →
`archive: symlink %q target escapes dest`, the step is falling. This closes symlink-
zip-slip bypass vector (symlink out + subsequent write "through" it).
- **Writing through a symlink directory created inside the same archive does not
supported.** If the archive contains a symlink directory *inside* `dest`
(within-dest, legitimate in itself), and then write along the path "through" this
symlink (`alias/file.txt`, where `alias` → existing directory) - step drops from
`escapes dest`. This is **fail-closed**: `securejoin` resolves symlink by already
to the path on disk, the resolved result ≠ naive `filepath.Join`, and
module rejects the entry. The check goes along the **resolved** path, and not lexically
- naively replacing `securejoin` with lexical `Join` would break this protection. Price -
the legitimate "write through your own symlink directory" does not work in one archive;
this is a deliberate safe disclaimer, the target directory must be addressed directly.
- **setuid / setgid / sticky are always masked.** record mode is applied as
`entry.Mode() & 0o777` — bits `setuid`/`setgid`/`sticky` are unconditionally cleared
(anti-privesc: the archive cannot push the setuid-root binary). It's tough
invariant, not a flag. owner/group from the archive are **not** taken - the files are received
owner of the `soul`-agent process.
- **Unsupported post types are a clear error.** hardlink (`TypeLink`),
devices (`TypeBlock`/`TypeChar`), fifo (`TypeFifo`), socket → step drops
(`archive: entry %q: unsupported type`), rather than silently skipping. In MVP these types
do not materialize. Service PAX/GNU headers and sparse metadata silently
are ignored - they do not add files to the tree.
- **Checksum - for idempotency, not for source verification.** SHA-256 in
`register.<name>.sha256` and the marker is calculated based on the one **already** on the disk
source file (`hashFile`) - this is "is the archive the same as last time", and not
"did it match the expected trusted hash." Verifications against reference
the module does not do checksum/signatures; if you need it, check the hash separately
step (for example `register` + `failed_when:`) before unpacking.
- **Privileges.** Manifest
([`archive.yaml`](../../../../shared/coremanifest/archive.yaml)) announces
only [`fs_write_root`](../../../naming-rules.md#required_capabilities-enum)
(write beyond `/var/lib/soul-stack/`), but **not** `run_as_root` and more
**not** `exec_subprocess` (subprocesses are not spawned). The module runs with
process privileges `soul`-agent; for unpacking into system paths agent on
in practice it works as root - that's why setuid masking and within-dest invariants
the more valuable.
- **Trusting the source is still appropriate.** The invariants above close
zip-slip / zip-bomb / privesc-via-setuid, but the module does not verify the **signature
/ origin** archive. For untrusted artifacts (upload, third-party build)
the isolated unprivileged directory `dest` is still preferred and
separate signature/hash check before unpacking:

  ```yaml
  # Trusted tarball known from Destiny itself to its directory.
  - name: Extract node_exporter tarball
    module: core.archive.extracted
    params:
      path: "${ '/tmp/node_exporter-' + input.version + '.tar.gz' }"
      dest: "${ '/tmp/node_exporter-' + input.version }"
      max_size: "500MiB"
  ```

## See also

- [README.md](../../README.md) - directory of core modules.
- [core/url/README.md](../url/README.md) - download the archive by URL before unpacking.
- [core/cmd/README.md](../cmd/README.md) — layout of the unpacked binary (`install`).
- [soul/modules.md](../../../soul/modules.md) - host side of modules and cache.
- [naming-rules.md → Destiny Modules](../../../naming-rules.md) - a dictionary of names.
- [ADR-015](../../../adr/0015-core-modules-mvp.md) - list of core MVPs.
