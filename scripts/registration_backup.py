"""Restricted source export and off-host backup/restore checks (Python 3.12+)."""

import argparse
import contextlib
import datetime as dt
import fcntl
import hashlib
import json
import os
import re
import shutil
import subprocess
import sys
import tarfile
import tempfile
import time
import uuid
from pathlib import Path

NAME = re.compile(r"registration-(\d{8}T\d{6}Z)-[0-9a-f]{32}\.tar")
MEMBERS = {"database.dump", "roles.sql", "backend.env", "frontend.env", "manifest.json"}
COUNTS_SQL = """SELECT json_build_object(
    'users', (SELECT count(*) FROM users),
    'user_permissions', (SELECT count(*) FROM user_permissions),
    'bot_chats', (SELECT count(*) FROM bot_chats),
    'schema_migrations', (SELECT count(*) FROM schema_migrations),
    'outbound_messages', (SELECT count(*) FROM outbound_messages));"""


class BackupError(RuntimeError):
    """An intentionally non-sensitive diagnostic, safe to write to journal."""


def log(event, **fields):
    print(
        json.dumps(
            {
                "time": dt.datetime.now(dt.UTC).isoformat(),
                "level": "info",
                "service": "registration-backup",
                "message": event,
                **fields,
            }
        ),
        flush=True,
    )


def run(command, *, output=None, source=None, timeout=300):
    # Commands can process credentials/personal data: never echo stderr or argv on failure.
    result = subprocess.run(
        command,
        stdin=source,
        stdout=output or subprocess.PIPE,
        stderr=subprocess.PIPE,
        timeout=timeout,
        check=False,
    )
    if result.returncode:
        raise BackupError(f"{command[0]} failed (exit {result.returncode}); output withheld")
    return result.stdout


def checksum(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def image_fingerprint(metadata):
    # Docker stores can expose different IDs after save/load. Compare actual runtime
    # configuration and uncompressed layer hashes instead of trusting a local tag.
    value = [
        metadata.get("Architecture"),
        metadata.get("Os"),
        metadata.get("Config"),
        metadata["RootFS"]["Layers"],
    ]
    return hashlib.sha256(json.dumps(value, sort_keys=True).encode()).hexdigest()


def restore_image(manifest):
    references = [manifest["postgres_image"]]
    if "postgres_image_id" in manifest and "postgres_image_fingerprint" in manifest:
        references.append(
            "registration-backup-postgres:" + manifest["postgres_image_id"].split(":")[1]
        )
    for reference in references:
        cached = subprocess.run(
            ["docker", "image", "inspect", reference],
            capture_output=True,
            timeout=30,
        )
        if not cached.returncode:
            if (
                "postgres_image_fingerprint" in manifest
                and image_fingerprint(json.loads(cached.stdout)[0])
                != manifest["postgres_image_fingerprint"]
            ):
                raise BackupError("Cached PostgreSQL image content differs from source")
            return reference
    run(["docker", "pull", "--quiet", manifest["postgres_image"]], timeout=600)
    return manifest["postgres_image"]


def source_export(root):
    """Serialize snapshots with application DDL delivery; refuse retired sources."""
    if (root / "backends/registration/.retired").exists():
        raise BackupError("Retired source cannot export stale registration data")
    with (Path.home() / ".registration-deploy.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_SH)
        _source_export(root)


def _source_export(root):
    """Only this fixed operation is allowed by the dedicated SSH authorized key."""
    ids = (
        run(
            [
                "docker",
                "ps",
                "-q",
                "--filter",
                "label=com.docker.compose.project=registration",
                "--filter",
                "label=com.docker.compose.service=postgres",
            ]
        )
        .decode()
        .split()
    )
    if len(ids) != 1:
        raise BackupError("Expected exactly one running registration PostgreSQL container")
    container = ids[0]
    image_id = run(["docker", "inspect", "--format", "{{.Image}}", container]).decode().strip()
    metadata = json.loads(run(["docker", "image", "inspect", image_id]))[0]
    images = metadata["RepoDigests"]
    image = next((v for v in images or [] if v.startswith("postgres@sha256:")), None)
    if image is None:
        raise BackupError("PostgreSQL image must have an immutable repository digest")
    exec_pg = ["docker", "exec", "-i", container]
    os.umask(0o077)
    with tempfile.TemporaryDirectory(prefix="registration-export-") as temporary:
        directory = Path(temporary)
        with (directory / "database.dump").open("wb") as stream:
            run(
                [*exec_pg, "pg_dump", "-U", "registration", "-d", "registration", "-Fc"],
                output=stream,
            )
        with (directory / "database.dump").open("rb") as stream:
            run([*exec_pg, "pg_restore", "--list"], source=stream)
        with (directory / "roles.sql").open("wb") as stream:
            run(
                [
                    *exec_pg,
                    "pg_dumpall",
                    "-U",
                    "registration",
                    "--roles-only",
                    "--no-role-passwords",
                ],
                output=stream,
            )
        revisions = {}
        for name, project in (
            ("backend", root / "backends/registration"),
            ("frontend", root / "telegram-bots/registration-bot"),
        ):
            config = project / ".env"
            if not config.is_file() or config.is_symlink():
                raise BackupError("Required application configuration is missing or a symlink")
            (directory / f"{name}.env").write_bytes(config.read_bytes())
            revisions[name] = run(["git", "-C", str(project), "rev-parse", "HEAD"]).decode().strip()
        manifest = {
            "format": 1,
            "created_at": dt.datetime.now(dt.UTC).isoformat(),
            "postgres_image": image,
            "postgres_image_id": image_id,
            "postgres_image_fingerprint": image_fingerprint(metadata),
            "revisions": revisions,
            "sha256": {p.name: checksum(p) for p in directory.iterdir()},
        }
        (directory / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")
        with tarfile.open(fileobj=sys.stdout.buffer, mode="w|") as archive:
            for path in sorted(directory.iterdir()):
                info = archive.gettarinfo(str(path), arcname=path.name)
                info.mode, info.uid, info.gid, info.uname, info.gname = 0o600, 0, 0, "", ""
                with path.open("rb") as stream:
                    archive.addfile(info, stream)


def validate(path):
    with tarfile.open(path, "r:") as archive:
        members = archive.getmembers()
        if len(members) != len(MEMBERS) or {m.name for m in members} != MEMBERS:
            raise BackupError("Unexpected backup archive members")
        if any(not m.isfile() for m in members):
            raise BackupError("Backup archive must contain regular files only")
        if archive.getmember("manifest.json").size > 65536:
            raise BackupError("Backup manifest is too large")
        manifest = json.load(archive.extractfile("manifest.json"))
        if manifest.get("format") != 1 or set(manifest["sha256"]) != MEMBERS - {"manifest.json"}:
            raise BackupError("Unsupported backup manifest")
        if not re.fullmatch(r"postgres@sha256:[0-9a-f]{64}", manifest["postgres_image"]):
            raise BackupError("Invalid PostgreSQL image digest")
        if "postgres_image_id" in manifest and not re.fullmatch(
            r"sha256:[0-9a-f]{64}", manifest["postgres_image_id"]
        ):
            raise BackupError("Invalid PostgreSQL image ID")
        if "postgres_image_fingerprint" in manifest and not re.fullmatch(
            r"[0-9a-f]{64}", manifest["postgres_image_fingerprint"]
        ):
            raise BackupError("Invalid PostgreSQL image fingerprint")
        for name, expected in manifest["sha256"].items():
            with archive.extractfile(name) as stream:
                if hashlib.file_digest(stream, "sha256").hexdigest() != expected:
                    raise BackupError("Backup checksum mismatch")
        if archive.getmember("database.dump").size == 0:
            raise BackupError("Empty database dump")
        return manifest


def retained(paths, now):
    """Keep 48h, then newest of 7 UTC days / 4 ISO weeks / 12 months with backups."""
    dated = []
    for path in paths:
        match = NAME.fullmatch(path.name)
        if match:
            dated.append(
                (dt.datetime.strptime(match[1], "%Y%m%dT%H%M%SZ").replace(tzinfo=dt.UTC), path)
            )
    dated.sort(reverse=True)
    keep = {p for when, p in dated if now - when <= dt.timedelta(days=2)}
    for key, limit in (
        (lambda t: t.date(), 7),
        (lambda t: t.isocalendar()[:2], 4),
        (lambda t: (t.year, t.month), 12),
    ):
        seen = set()
        for when, path in dated:
            bucket = key(when)
            if bucket not in seen and len(seen) < limit:
                keep.add(path)
                seen.add(bucket)
    return keep


def write_status(directory, name, data):
    with tempfile.NamedTemporaryFile(
        mode="w", dir=directory, prefix=".status-", delete=False
    ) as stream:
        temporary = Path(stream.name)
        try:
            json.dump(data, stream)
            stream.write("\n")
            stream.flush()
            os.fsync(stream.fileno())
        except BaseException:
            temporary.unlink(missing_ok=True)
            raise
    temporary.replace(directory / name)


@contextlib.contextmanager
def locked(directory):
    os.umask(0o077)
    directory.mkdir(mode=0o700, parents=True, exist_ok=True)
    if directory.is_symlink() or directory.stat().st_mode & 0o077:
        raise BackupError("Backup directory must be private (0700) and not a symlink")
    with (directory / ".lock").open("a") as stream:
        fcntl.flock(stream, fcntl.LOCK_EX | fcntl.LOCK_NB)
        yield


def pull(config):
    directory = Path(config["directory"])
    with locked(directory):
        if os.statvfs(directory).f_bavail * os.statvfs(directory).f_frsize < 1024**3:
            raise BackupError("Less than 1 GiB free at backup destination")
        timestamp = dt.datetime.now(dt.UTC).strftime("%Y%m%dT%H%M%SZ")
        destination = directory / f"registration-{timestamp}-{uuid.uuid4().hex}.tar"
        partial = destination.with_suffix(".partial")
        try:
            with partial.open("xb") as stream:
                run(
                    [
                        "ssh",
                        "-F",
                        "/dev/null",
                        "-T",
                        "-i",
                        config["identity"],
                        "-o",
                        "IdentitiesOnly=yes",
                        "-o",
                        "BatchMode=yes",
                        "-o",
                        "StrictHostKeyChecking=yes",
                        "-o",
                        "ConnectTimeout=20",
                        "-o",
                        "ServerAliveInterval=15",
                        "-o",
                        "ServerAliveCountMax=3",
                        "-o",
                        f"UserKnownHostsFile={config['known_hosts']}",
                        config["source"],
                        "export",
                    ],
                    output=stream,
                    timeout=600,
                )
                stream.flush()
                os.fsync(stream.fileno())
            validate(partial)
            partial.replace(destination)
            status = {
                "time": dt.datetime.now(dt.UTC).isoformat(),
                "file": destination.name,
                "bytes": destination.stat().st_size,
                "sha256": checksum(destination),
            }
            write_status(directory, "latest.json", status)
            # Rotation only follows a fully received, checksum-validated backup.
            paths = [p for p in directory.iterdir() if NAME.fullmatch(p.name) and p.is_file()]
            keep = retained(paths, dt.datetime.now(dt.UTC))
            for path in paths:
                if path not in keep:
                    path.unlink()
            log("backup completed", **status, retained=len(keep))
        finally:
            partial.unlink(missing_ok=True)


def verify(config):
    directory = Path(config["directory"])
    with tempfile.TemporaryFile() as dump:
        with locked(directory):
            latest = json.loads((directory / "latest.json").read_text())
            if not NAME.fullmatch(latest["file"]):
                raise BackupError("Invalid latest backup filename")
            path = directory / latest["file"]
            if checksum(path) != latest["sha256"]:
                raise BackupError("Latest backup checksum mismatch")
            manifest = validate(path)
            with (
                tarfile.open(path, "r:") as archive,
                archive.extractfile("database.dump") as stream,
            ):
                shutil.copyfileobj(stream, dump)
            dump.seek(0)
        # Release the backup lock before slow image downloads or SQL restore.
        image = restore_image(manifest)
        container = "registration-backup-verify-" + uuid.uuid4().hex[:12]
        # This container has no network, host mounts, live credentials or application.
        try:
            run(
                [
                    "docker",
                    "run",
                    "-d",
                    "--pull=never",
                    "--name",
                    container,
                    "--network=none",
                    "--label",
                    "service=registration-backup-verify",
                    "--memory=384m",
                    "--cpus=1",
                    "--pids-limit=128",
                    "--security-opt=no-new-privileges:true",
                    "--tmpfs",
                    "/var/lib/postgresql/data:rw,nosuid,nodev,size=256m",
                    "-e",
                    "POSTGRES_HOST_AUTH_METHOD=trust",
                    "-e",
                    "POSTGRES_USER=registration",
                    "-e",
                    "POSTGRES_DB=registration",
                    image,
                ]
            )
            for _ in range(60):
                ready = subprocess.run(
                    [
                        "docker",
                        "exec",
                        container,
                        "sh",
                        "-c",
                        'test "$(cat /proc/1/comm)" = postgres && pg_isready -U registration -d registration',
                    ],
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.DEVNULL,
                    timeout=10,
                )
                if ready.returncode == 0:
                    break
                time.sleep(1)
            else:
                raise BackupError("Restore fixture did not become ready")
            run(
                [
                    "docker",
                    "exec",
                    "-i",
                    container,
                    "pg_restore",
                    "-U",
                    "registration",
                    "-d",
                    "registration",
                    "--exit-on-error",
                    "--single-transaction",
                ],
                source=dump,
            )
            counts = json.loads(
                run(
                    [
                        "docker",
                        "exec",
                        container,
                        "psql",
                        "-XAt",
                        "-v",
                        "ON_ERROR_STOP=1",
                        "-U",
                        "registration",
                        "-d",
                        "registration",
                        "-c",
                        COUNTS_SQL,
                    ]
                )
            )
            if counts["schema_migrations"] < 1:
                raise BackupError("Restored schema has no migration history")
            status = {
                "time": dt.datetime.now(dt.UTC).isoformat(),
                "file": path.name,
                "counts": counts,
            }
            write_status(directory, "verified.json", status)
            log("restore verified", **status)
        finally:
            run(["docker", "rm", "-fv", container], timeout=60)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("command", choices=("source", "pull", "verify"))
    parser.add_argument("--root", type=Path, default=Path("/home/maxim/projects"))
    parser.add_argument(
        "--config", type=Path, default=Path("/home/maxim/.config/registration-backup/config.json")
    )
    args = parser.parse_args()
    if args.command == "source":
        source_export(args.root)
    else:
        config = json.loads(args.config.read_text())
        {"pull": pull, "verify": verify}[args.command](config)


if __name__ == "__main__":
    try:
        main()
    except Exception as error:
        # Never expose subprocess arguments, file contents, or arbitrary exception text.
        print(
            json.dumps(
                {
                    "time": dt.datetime.now(dt.UTC).isoformat(),
                    "level": "error",
                    "service": "registration-backup",
                    "message": "backup operation failed",
                    "error_type": type(error).__name__,
                    "reason": str(error) if isinstance(error, BackupError) else "details withheld",
                }
            ),
            file=sys.stderr,
        )
        sys.exit(1)
