"""Scan only versionable files, excluding local credentials through Git ignore rules."""

import os
import pathlib
import shutil
import subprocess
import tempfile

root = pathlib.Path.cwd()
paths = (
    subprocess.check_output(["git", "ls-files", "--cached", "--others", "--exclude-standard", "-z"])
    .decode()
    .split("\0")
)
with tempfile.TemporaryDirectory() as directory:
    destination = pathlib.Path(directory)
    for name in set(paths) - {""}:
        source = root / name
        if source.is_symlink() or not source.is_file():
            continue
        target = destination / name
        target.parent.mkdir(parents=True, exist_ok=True)
        shutil.copyfile(source, target)
    subprocess.run(
        [
            "docker",
            "run",
            "--rm",
            "--network=none",
            "--mount",
            f"type=bind,src={directory},dst=/src,readonly",
            "zricethezav/gitleaks:v" + os.environ["GITLEAKS_VERSION"],
            "dir",
            "/src",
            "--redact",
            "--config=/src/.gitleaks.toml",
            "--no-banner",
        ],
        check=True,
    )
