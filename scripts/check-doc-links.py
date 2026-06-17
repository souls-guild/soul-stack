#!/usr/bin/env python3
"""Проверка внутренних markdown-ссылок репозитория.

Собирает ссылки вида `[текст](path.md)` и `[текст](path.md#anchor)` из всех
*.md-файлов (включая CLAUDE.md и examples/), а также ссылки `docs/...#anchor`
из Go-комментариев, и проверяет:
  - файл по относительному пути существует;
  - якорь (если указан) генерится GitHub-slug-ом из какого-нибудь заголовка
    целевого файла.

GitHub-slug: lowercase, не-alphanumeric (кроме пробела/дефиса/подчёркивания)
вырезается, пробелы → дефис; кириллица сохраняется.

Pre-existing битые ссылки, не входящие в текущий скоуп, заносятся в
scripts/doc-links-allowlist.txt (формат `файл:ссылка`) и пропускаются.

Внешние ссылки (http/https/mailto), чистые якоря (`#...` без файла),
не-markdown-таргеты (`.go`, `.yaml`, `.yml`, картинки и т.п.) — вне проверки
якорей (для не-md проверяется только существование файла).
"""
from __future__ import annotations

import os
import re
import sys

REPO_ROOT = os.path.abspath(os.path.join(os.path.dirname(__file__), ".."))
ALLOWLIST_PATH = os.path.join(REPO_ROOT, "scripts", "doc-links-allowlist.txt")

# Каталоги, которые не сканируем.
SKIP_DIRS = {".git", "node_modules", "vendor", ".pm", "proto/gen"}

MD_LINK_RE = re.compile(r"\[[^\]]*\]\(([^)\s]+?)\)")
# Go-комментарии: ловим docs/...#anchor или ../docs/...#anchor внутри строки.
GO_DOC_LINK_RE = re.compile(r"(?:\.\./)*docs/[\w./-]+\.md#[\w.-]+")

HEADING_RE = re.compile(r"^(#{1,6})\s+(.*?)\s*#*\s*$")


def github_slug(text: str) -> str:
    s = text.strip().lower()
    # markdown-разметку из заголовка убираем (backticks, *, _, [..](..)).
    s = re.sub(r"\[([^\]]*)\]\([^)]*\)", r"\1", s)  # [t](u) -> t
    s = s.replace("`", "").replace("*", "")
    s = re.sub(r"[^\w\s-]", "", s, flags=re.UNICODE)
    s = s.replace(" ", "-")
    return s


def collect_anchors(md_path: str) -> set[str]:
    anchors: set[str] = set()
    seen: dict[str, int] = {}
    try:
        with open(md_path, encoding="utf-8") as fh:
            in_fence = False
            for line in fh:
                stripped = line.strip()
                if stripped.startswith("```") or stripped.startswith("~~~"):
                    in_fence = not in_fence
                    continue
                if in_fence:
                    continue
                m = HEADING_RE.match(line.rstrip("\n"))
                if not m:
                    continue
                base = github_slug(m.group(2))
                if base in seen:
                    seen[base] += 1
                    anchors.add(f"{base}-{seen[base]}")
                else:
                    seen[base] = 0
                    anchors.add(base)
    except OSError:
        pass
    return anchors


def load_allowlist() -> set[str]:
    entries: set[str] = set()
    if not os.path.exists(ALLOWLIST_PATH):
        return entries
    with open(ALLOWLIST_PATH, encoding="utf-8") as fh:
        for raw in fh:
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            # отрезаем хвостовой комментарий после ` # `
            line = line.split(" #", 1)[0].strip()
            if line:
                entries.add(line)
    return entries


def iter_files() -> list[str]:
    out: list[str] = []
    for dirpath, dirnames, filenames in os.walk(REPO_ROOT):
        rel = os.path.relpath(dirpath, REPO_ROOT)
        parts = set(rel.split(os.sep))
        if parts & {".git", "node_modules", "vendor", ".pm"}:
            dirnames[:] = []
            continue
        if rel.replace(os.sep, "/").startswith("proto/gen"):
            dirnames[:] = []
            continue
        for name in filenames:
            if name.endswith(".md") or name.endswith(".go"):
                out.append(os.path.join(dirpath, name))
    return out


def is_external(target: str) -> bool:
    return (
        target.startswith("http://")
        or target.startswith("https://")
        or target.startswith("mailto:")
        or target.startswith("//")
    )


# Кэш якорей по абсолютному пути целевого файла.
_anchor_cache: dict[str, set[str]] = {}


def anchors_for(path: str) -> set[str]:
    if path not in _anchor_cache:
        _anchor_cache[path] = collect_anchors(path)
    return _anchor_cache[path]


def check_link(src_file: str, target: str, root_relative: bool = False) -> str | None:
    """Возвращает текст ошибки или None если ссылка валидна/вне скоупа.

    root_relative=True — путь резолвится от корня репо (семантика doc-ссылок
    в Go-комментариях: `docs/...` отсчитывается от корня, не от каталога файла).
    """
    if is_external(target) or target.startswith("#"):
        return None

    path_part, _, anchor = target.partition("#")
    if not path_part:
        return None

    base = REPO_ROOT if root_relative else os.path.dirname(src_file)
    abs_target = os.path.normpath(os.path.join(base, path_part))

    # Fallback: часть markdown-файлов (.claude/agents/*, CLAUDE.md) пишет ссылки
    # от корня репо, а не относительно своего каталога. Если относительный
    # резолв не нашёл файл — пробуем от корня.
    if not os.path.exists(abs_target) and not root_relative:
        alt = os.path.normpath(os.path.join(REPO_ROOT, path_part))
        if os.path.exists(alt):
            abs_target = alt

    if not os.path.exists(abs_target):
        return f"target file not found: {path_part}"

    # Якорь проверяем только для markdown-файлов.
    if anchor and abs_target.endswith(".md"):
        if anchor not in anchors_for(abs_target):
            return f"anchor not found in {path_part}: #{anchor}"

    return None


def main() -> int:
    allowlist = load_allowlist()
    used_allow: set[str] = set()
    errors: list[str] = []

    for src_file in iter_files():
        rel_src = os.path.relpath(src_file, REPO_ROOT).replace(os.sep, "/")
        try:
            with open(src_file, encoding="utf-8") as fh:
                content = fh.read()
        except (OSError, UnicodeDecodeError):
            continue

        is_go = src_file.endswith(".go")
        if is_go:
            targets = GO_DOC_LINK_RE.findall(content)
        else:  # .md
            targets = MD_LINK_RE.findall(content)

        for target in targets:
            key = f"{rel_src}:{target}"
            if key in allowlist:
                used_allow.add(key)
                continue
            err = check_link(src_file, target, root_relative=is_go)
            if err:
                errors.append(f"{rel_src}: {err}  (link: {target})")

    stale = allowlist - used_allow
    if errors:
        print("check-doc-links: найдены битые ссылки:\n")
        for e in sorted(errors):
            print(f"  {e}")
        print(f"\nИТОГО битых ссылок: {len(errors)}")
        if stale:
            print(f"(в allowlist {len(stale)} устаревших записей — см. ниже)")
        return 1

    if stale:
        print("check-doc-links: ссылки целы, но в allowlist есть устаревшие записи")
        print("(ссылка починена/удалена — убери строку из scripts/doc-links-allowlist.txt):\n")
        for s in sorted(stale):
            print(f"  {s}")
        return 1

    print(f"check-doc-links: все внутренние ссылки целы (allowlist: {len(allowlist)})")
    return 0


if __name__ == "__main__":
    sys.exit(main())
