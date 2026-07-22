# Translations

Reader-facing translations of the top-level [README](../../README.md). The English
`README.md` at the repository root is the source of truth; the files here are
translations kept in sync with it.

| Language | File |
|---|---|
| Русский (Russian) | [README.ru.md](README.ru.md) |

## Adding a language

1. Copy the root [`README.md`](../../README.md) to `docs/i18n/README.<lang>.md`, using
   the ISO 639-1 code (`de`, `zh`, `fr`, …).
2. Translate the prose. Keep the domain vocabulary untranslated — **Keeper**, **Souls**,
   **Destiny**, **Soulprint**, **Essence**, **Archon**, **Coven**, **SoulSeed** — as
   well as the binary names (`keeper` / `soul` / `soul-lint`), code, and commands.
3. Fix the relative links for this folder: root files become `../../<file>` (for example
   `../../LICENSE`) and `docs/` files become `../<file>` (for example
   `../architecture.md`).
4. Add a row to the table above, and add the language to the switcher line at the top of
   the root `README.md` and of every existing translation.
5. `make check-doc-links` must stay green.
