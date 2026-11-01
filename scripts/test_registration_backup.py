"""Backup corruption, rotation, and failed-transfer tests use only synthetic data."""

import datetime as dt
import hashlib
import io
import json
import subprocess
import tarfile
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

import registration_backup as backup


def filename(when):
    return f"registration-{when:%Y%m%dT%H%M%SZ}-{'a' * 32}.tar"


def bundle(path, *, corrupt=False, extra=None):
    files = {
        "database.dump": b"synthetic-dump",
        "roles.sql": b"fixture",
        "backend.env": b"FIXTURE=synthetic",
        "frontend.env": b"FIXTURE=synthetic",
    }
    manifest = {
        "format": 1,
        "postgres_image": "postgres@sha256:" + "0" * 64,
        "postgres_image_id": "sha256:" + "0" * 64,
        "sha256": {k: hashlib.sha256(v).hexdigest() for k, v in files.items()},
    }
    if corrupt:
        files["database.dump"] = b"corrupted"
    files["manifest.json"] = json.dumps(manifest).encode()
    if extra:
        files[extra] = b"unexpected"
    with tarfile.open(path, "w") as archive:
        for name, data in files.items():
            info = tarfile.TarInfo(name)
            info.size = len(data)
            archive.addfile(info, io.BytesIO(data))


class BackupTest(unittest.TestCase):
    def test_retired_source_cannot_export(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            retired = root / "backends/registration/.retired"
            retired.parent.mkdir(parents=True)
            retired.touch()
            with patch.object(backup, "_source_export") as export:
                with self.assertRaises(backup.BackupError):
                    backup.source_export(root)
                export.assert_not_called()

    def test_export_holds_shared_deploy_lock(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)

            def export(root):
                with (home / ".registration-deploy.lock").open("a") as contender:
                    with self.assertRaises(BlockingIOError):
                        backup.fcntl.flock(contender, backup.fcntl.LOCK_EX | backup.fcntl.LOCK_NB)

            with patch.object(Path, "home", return_value=home):
                with patch.object(backup, "_source_export", side_effect=export) as source:
                    backup.source_export(home)
                    source.assert_called_once_with(home)
            with (home / ".registration-deploy.lock").open("a") as contender:
                backup.fcntl.flock(contender, backup.fcntl.LOCK_EX | backup.fcntl.LOCK_NB)

    def test_restore_uses_cached_image_without_holding_backup_lock(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            archive = directory / filename(dt.datetime.now(dt.UTC))
            bundle(archive)
            backup.write_status(
                directory,
                "latest.json",
                {
                    "file": archive.name,
                    "sha256": backup.checksum(archive),
                },
            )
            commands = []

            def restore(command, **kwargs):
                commands.append(command)
                # A scheduled backup must be able to run during download/restore.
                with backup.locked(directory):
                    pass
                if "psql" in command:
                    return json.dumps({"schema_migrations": 5, "users": 1}).encode()

            with (
                patch.object(backup, "run", side_effect=restore),
                patch.object(
                    backup.subprocess, "run", return_value=subprocess.CompletedProcess([], 0)
                ),
            ):
                backup.verify({"directory": temporary})
            self.assertFalse(any(command[1] == "pull" for command in commands))
            start = next(command for command in commands if command[1] == "run")
            self.assertIn("--pull=never", start)
            self.assertIn("--network=none", start)
            self.assertEqual(start[-1], "postgres@sha256:" + "0" * 64)
            self.assertEqual(commands[-1][1:3], ["rm", "-fv"])
            self.assertEqual(
                json.loads((directory / "verified.json").read_text())["counts"]["users"], 1
            )

    def test_imported_image_cache_requires_matching_content(self):
        metadata = {
            "Architecture": "amd64",
            "Os": "linux",
            "Config": {"Cmd": ["fixture"]},
            "RootFS": {"Layers": ["synthetic-layer"]},
        }
        manifest = {
            "postgres_image": "postgres@sha256:" + "0" * 64,
            "postgres_image_id": "sha256:" + "1" * 64,
            "postgres_image_fingerprint": backup.image_fingerprint(metadata),
        }
        results = [
            subprocess.CompletedProcess([], 1),
            subprocess.CompletedProcess([], 0, stdout=json.dumps([metadata]).encode()),
        ]
        with patch.object(backup.subprocess, "run", side_effect=results):
            self.assertEqual(
                backup.restore_image(manifest), "registration-backup-postgres:" + "1" * 64
            )
        metadata["Config"]["Cmd"] = ["changed-entrypoint"]
        results = [
            subprocess.CompletedProcess([], 1),
            subprocess.CompletedProcess([], 0, stdout=json.dumps([metadata]).encode()),
        ]
        with patch.object(backup.subprocess, "run", side_effect=results):
            with self.assertRaisesRegex(RuntimeError, "differs"):
                backup.restore_image(manifest)

    def test_checksums_and_members(self):
        with tempfile.TemporaryDirectory() as temporary:
            path = Path(temporary) / "fixture.tar"
            bundle(path)
            self.assertEqual(backup.validate(path)["format"], 1)
            bundle(path, corrupt=True)
            with self.assertRaisesRegex(RuntimeError, "checksum"):
                backup.validate(path)
            bundle(path, extra="../secret")
            with self.assertRaisesRegex(RuntimeError, "members"):
                backup.validate(path)
            bundle(path)
            with tarfile.open(path, "a") as archive:
                duplicate = tarfile.TarInfo("database.dump")
                archive.addfile(duplicate)
            with self.assertRaisesRegex(RuntimeError, "members"):
                backup.validate(path)

    def test_retention_spans_year_boundary_and_preserves_recent(self):
        now = dt.datetime(2026, 1, 2, 12, tzinfo=dt.UTC)
        dates = [now - dt.timedelta(hours=i) for i in range(24 * 800)]
        paths = [Path(filename(when)) for when in dates]
        keep = backup.retained(paths, now)
        self.assertTrue(set(paths[:49]).issubset(keep))
        self.assertIn(paths[0], keep)
        self.assertNotIn(paths[-1], keep)
        self.assertLessEqual(len(keep), 49 + 7 + 4 + 12)
        # Every retained bucket chooses the newest file; no mtime dependency.
        self.assertIn(Path(filename(dt.datetime(2025, 12, 31, 23, tzinfo=dt.UTC))), keep)
        self.assertIn(Path(filename(dt.datetime(2025, 2, 28, 23, tzinfo=dt.UTC))), keep)
        self.assertNotIn(Path("unrelated.tar"), backup.retained([Path("unrelated.tar")], now))

    def test_failed_transfer_does_not_publish_or_rotate(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            old = directory / filename(dt.datetime(2020, 1, 1, tzinfo=dt.UTC))
            old.write_bytes(b"previous-backup")
            config = {
                "directory": temporary,
                "identity": "/fixture/key",
                "known_hosts": "/fixture/known_hosts",
                "source": "fixture@127.0.0.1",
            }

            def fail(command, *, output, **kwargs):
                output.write(b"partial-data")
                raise subprocess.TimeoutExpired("ssh", 600)

            with patch.object(backup, "run", side_effect=fail):
                with self.assertRaises(subprocess.TimeoutExpired):
                    backup.pull(config)
            self.assertEqual(old.read_bytes(), b"previous-backup")
            self.assertEqual(list(directory.glob("*.partial")), [])
            self.assertFalse((directory / "latest.json").exists())

    def test_successful_transfer_and_private_permissions(self):
        with tempfile.TemporaryDirectory() as temporary:
            directory = Path(temporary)
            fixture = directory / "synthetic.tar"
            bundle(fixture)
            # Two old copies in one bucket: rotate only the older duplicate.
            old = directory / filename(dt.datetime(2020, 1, 1, 0, tzinfo=dt.UTC))
            newer = directory / filename(dt.datetime(2020, 1, 1, 1, tzinfo=dt.UTC))
            old.write_bytes(b"old")
            newer.write_bytes(b"newer")
            config = {
                "directory": temporary,
                "identity": "/fixture/key",
                "known_hosts": "/fixture/known_hosts",
                "source": "fixture@127.0.0.1",
            }

            def receive(command, *, output, **kwargs):
                output.write(fixture.read_bytes())

            with patch.object(backup, "run", side_effect=receive):
                backup.pull(config)
            status = json.loads((directory / "latest.json").read_text())
            destination = directory / status["file"]
            self.assertEqual(backup.checksum(destination), status["sha256"])
            self.assertEqual(destination.stat().st_mode & 0o777, 0o600)
            self.assertTrue(fixture.exists())
            self.assertFalse(old.exists())
            self.assertTrue(newer.exists())


if __name__ == "__main__":
    unittest.main()
