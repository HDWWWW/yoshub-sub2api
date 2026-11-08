"""Reject local SQL bytes that differ from the committed migration checksums."""
import pathlib
import subprocess

root = pathlib.Path(__file__).resolve().parents[2]
paths = subprocess.check_output(
    ["git", "ls-files", "backend/migrations/*.sql"], cwd=root, text=True
).splitlines()
for name in paths:
    committed = subprocess.check_output(["git", "show", "HEAD:" + name], cwd=root)
    actual = (root / name).read_bytes()
    if actual != committed:
        raise SystemExit("Migration differs from committed bytes (including line endings): " + name)
print("Verified committed bytes for " + str(len(paths)) + " SQL migrations")
