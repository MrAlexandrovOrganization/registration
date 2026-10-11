"""Create and validate a private PostgreSQL custom-format backup via Compose."""

import datetime
import os
import subprocess
import sys
import tempfile
from pathlib import Path


def main():
    directory = Path(sys.argv[1]).resolve()
    compose = sys.argv[2:]
    if not compose:
        raise SystemExit("Docker Compose command is required")
    os.umask(0o077)
    directory.mkdir(parents=True, exist_ok=True, mode=0o700)
    timestamp = datetime.datetime.now(datetime.UTC).strftime("%Y%m%dT%H%M%SZ")
    fd, name = tempfile.mkstemp(
        prefix=f"registration-{timestamp}-", suffix=".partial", dir=directory
    )
    partial = Path(name)
    try:
        with os.fdopen(fd, "wb") as output:
            subprocess.run(
                [
                    *compose,
                    "exec",
                    "-T",
                    "postgres",
                    "pg_dump",
                    "-U",
                    "registration",
                    "-d",
                    "registration",
                    "--format=custom",
                ],
                stdout=output,
                check=True,
            )
            output.flush()
            os.fsync(output.fileno())
        with partial.open("rb") as source:
            subprocess.run(
                [*compose, "exec", "-T", "postgres", "pg_restore", "--list"],
                stdin=source,
                stdout=subprocess.DEVNULL,
                check=True,
            )
        destination = partial.with_suffix(".dump")
        partial.rename(destination)
        print(f"Backup: {destination}")
    finally:
        partial.unlink(missing_ok=True)


if __name__ == "__main__":
    main()
