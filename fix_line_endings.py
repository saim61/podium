"""Normalise line endings to LF.

Prefers `git ls-files` over walking the tree, so .gitignore is respected and build output,
coverage files and scratch directories are never touched. Falls back to a filesystem walk
outside a git repo.

    python fix_line_endings.py            # convert
    python fix_line_endings.py --check    # report only, exit 1 if anything would change
"""

from __future__ import annotations

import subprocess
import sys
from pathlib import Path

# Only consulted by the filesystem-walk fallback; inside a repo .gitignore does this job.
SKIP_DIRS = {
    ".git", ".venv", "venv", "node_modules", "__pycache__",
    ".mypy_cache", ".ruff_cache", ".pytest_cache", "bin", "dist", "vendor",
}

SKIP_EXTS = {
    ".png", ".jpg", ".jpeg", ".gif", ".ico", ".svgz", ".pdf",
    ".zip", ".gz", ".tar", ".bz2", ".xz", ".7z",
    ".db", ".sqlite", ".sqlite3",
    ".pyc", ".whl", ".so", ".dll", ".dylib", ".a", ".o",
    ".exe", ".test", ".wasm",
    ".woff", ".woff2", ".ttf", ".otf", ".eot",
    ".mp3", ".mp4", ".mov", ".webm", ".webp",
}

# Windows shells can misbehave on a batch file with bare LF, so these keep CRLF deliberately.
# Nothing in Podium matches today; the guard is here so adding one later cannot break silently.
KEEP_CRLF_EXTS = {".bat", ".cmd", ".ps1", ".sln", ".vcxproj"}


def is_probably_binary(data: bytes) -> bool:
    return b"\x00" in data[:8000]


def candidates(root: Path) -> list[Path]:
    """Tracked files if this is a git repo, otherwise everything not obviously junk."""
    try:
        result = subprocess.run(
            ["git", "-C", str(root), "ls-files", "-z"],
            capture_output=True,
            check=True,
        )
    except (OSError, subprocess.CalledProcessError):
        return [p for p in root.rglob("*") if not any(part in SKIP_DIRS for part in p.parts)]

    names = result.stdout.decode("utf-8", "surrogateescape").split("\0")
    return [root / name for name in names if name]


def normalise(data: bytes) -> bytes:
    """CRLF and lone CR both become LF, without turning an existing LF into two."""
    return data.replace(b"\r\n", b"\n").replace(b"\r", b"\n")


def main(argv: list[str]) -> int:
    check_only = "--check" in argv
    positional = [a for a in argv if not a.startswith("-")]
    root = Path(positional[0] if positional else ".").resolve()

    changed: list[Path] = []
    kept: list[Path] = []

    for path in candidates(root):
        if not path.is_file():
            continue
        if path.suffix.lower() in SKIP_EXTS:
            continue

        try:
            data = path.read_bytes()
        except OSError as err:
            print(f"skipped {path}: {err}", file=sys.stderr)
            continue

        if b"\r" not in data or is_probably_binary(data):
            continue

        if path.suffix.lower() in KEEP_CRLF_EXTS:
            kept.append(path)
            continue

        changed.append(path)
        if not check_only:
            path.write_bytes(normalise(data))

    for path in kept:
        print(f"kept CRLF (by extension): {path.relative_to(root)}")

    if not changed:
        print("Nothing to convert - all line endings are already LF.")
        return 0

    verb = "Would convert" if check_only else "Converted"
    print(f"{verb} {len(changed)} file(s):")
    for path in changed:
        print("  ", path.relative_to(root))

    return 1 if check_only else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
