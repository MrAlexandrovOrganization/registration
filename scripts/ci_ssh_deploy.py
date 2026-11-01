"""Deliver checked CI images over host-key-pinned SSH; secrets travel on stdin."""

import os
import re
import shlex
import subprocess
import tempfile
from pathlib import Path


def main():
    sha = os.environ["DEPLOY_SHA"]
    if not re.fullmatch(r"[0-9a-f]{40}", sha):
        raise ValueError("Invalid deploy SHA")
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="registration-deploy-") as temporary:
        root = Path(temporary)
        (root / "key").write_text(os.environ["DEPLOY_SSH_KEY"].rstrip() + "\n")
        (root / "known").write_text(os.environ["DEPLOY_KNOWN_HOSTS"].rstrip() + "\n")
        config = (
            "Host registration-finland\n"
            " HostName 193.84.2.141\n"
            " User maxim\n"
            " HostKeyAlias registration-finland\n"
            " HostKeyAlgorithms ssh-ed25519\n"
            " KexAlgorithms curve25519-sha256\n"
            " IdentitiesOnly yes\n"
            " StrictHostKeyChecking yes\n"
            " BatchMode yes\n"
            " ConnectTimeout 20\n"
            " ServerAliveInterval 15\n"
            " ServerAliveCountMax 3\n"
            " ForwardAgent no\n"
            f" IdentityFile {root / 'key'}\n"
            f" UserKnownHostsFile {root / 'known'}\n"
        )
        (root / "config").write_text(config)
        script = "set -eu\n"
        for key in ["DEPLOY_SHA", "IMAGE_MANIFEST_B64", "GHCR_USER", "GHCR_TOKEN"]:
            script += "export " + key + "=" + shlex.quote(os.environ[key]) + "\n"
        script += "cd /home/maxim/projects/backends/registration\n"
        script += "test ! -e .retired\n"
        script += 'test "$(git rev-parse --show-toplevel)" = "$(pwd -P)"\n'
        script += "git fetch --quiet origin refs/heads/main\n"
        script += 'git show "$DEPLOY_SHA:makefiles/ci-deploy.mk" | make -f - ci-deploy\n'
        subprocess.run(
            ["ssh", "-F", str(root / "config"), "registration-finland", "sh", "-s"],
            input=script,
            text=True,
            check=True,
            timeout=2100,
        )


if __name__ == "__main__":
    main()
