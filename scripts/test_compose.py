"""Exercise the real make up lifecycle on disposable Compose resources only."""

import hashlib
import json
import os
import shlex
import subprocess
import tempfile
import time
import uuid
from pathlib import Path

root = Path(__file__).resolve().parent.parent
name = "registration-fixture-" + uuid.uuid4().hex[:10]
# Never inherit Compose selectors or application credentials; never load .env.
env = {key: value for key, value in os.environ.items() if not key.startswith("COMPOSE_")}
env.update(
    POSTGRES_PASSWORD="synthetic-fixture-password",
    BACKEND_TOKEN="synthetic-fixture-backend-token-at-least-32",
    BOT_ID="1000",
    ROOT_ID="1001",
)


def run(args, **kwargs):
    try:
        return subprocess.run(args, cwd=root, env=env, check=True, text=True, **kwargs)
    except subprocess.CalledProcessError as error:
        # Every Compose/SQL parameter in this test is synthetic.
        raise RuntimeError(
            f"fixture command failed: {error.stdout or ''}{error.stderr or ''}"
        ) from error


with tempfile.TemporaryDirectory(prefix=name) as directory:
    compose = [
        "docker",
        "compose",
        "--project-name",
        name,
        "--env-file",
        str(root / "config/test.env"),
        "-f",
        str(root / "docker-compose.yml"),
    ]
    config = json.loads(
        run(
            compose + ["--profile", "tools", "config", "--format", "json"], capture_output=True
        ).stdout
    )
    for service in config["services"].values():
        service.pop("ports", None)
    config["services"]["postgres"].pop("volumes")
    config["services"]["postgres"]["tmpfs"] = ["/var/lib/postgresql/data"]
    config.pop("volumes", None)
    config["services"]["backend"]["environment"]["OTEL_EXPORTER_OTLP_ENDPOINT"] = ""
    networks = []
    for key, network in config["networks"].items():
        network["name"] = name + "-" + key
        if network.get("external"):
            networks.append(network["name"])
    fixture = Path(directory) / "compose.json"
    fixture.write_text(json.dumps(config))
    compose[-1] = str(fixture)
    make = ["make", "DOCKER_COMPOSE=" + shlex.join(compose)]

    def sql(statement):
        return run(
            compose
            + [
                "exec",
                "-T",
                "postgres",
                "psql",
                "-X",
                "-U",
                "registration",
                "-d",
                "registration",
                "-v",
                "ON_ERROR_STOP=1",
                "-Atq",
            ],
            input=statement,
            capture_output=True,
        ).stdout.strip()

    def backend_state():
        ids = run(
            compose + ["ps", "--all", "--quiet", "backend"], capture_output=True
        ).stdout.strip()
        if not ids:
            return "absent"
        return run(
            ["docker", "inspect", "--format", "{{.State.Status}}", ids], capture_output=True
        ).stdout.strip()

    def up(success=True):
        result = subprocess.run(
            make + ["up"], cwd=root, env=env, text=True, capture_output=True, timeout=600
        )
        if (result.returncode == 0) != success:
            raise AssertionError(result.stdout + result.stderr)
        if success:
            assert backend_state() == "running"
            assert sql("SELECT max(version) FROM schema_migrations") == "4"
        else:
            assert backend_state() in ("exited", "absent"), "backend started after failed migration"

    def schema3():
        run(compose + ["stop", "backend"])
        # Only this unique fixture's tmpfs database is reset.
        sql("DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
        sql(
            "CREATE TABLE schema_migrations(version INTEGER PRIMARY KEY, checksum TEXT NOT NULL, applied_at TIMESTAMPTZ NOT NULL DEFAULT now());"
        )
        for path in sorted((root / "internal/store/migrations").glob("*.sql"))[:3]:
            data = path.read_bytes()
            checksum = hashlib.sha256(data).hexdigest()
            version = int(path.name.split("_")[0])
            sql(
                data.decode()
                + f"\nINSERT INTO schema_migrations(version,checksum) VALUES({version},'{checksum}');"
            )
        sql("""INSERT INTO users(telegram_id,name) VALUES(123,'Synthetic participant');
            INSERT INTO outbound_messages(chat,priority,kind,body,status,attempts)
            VALUES(123,'interactive','view','{"kind":"help"}','retry_wait',2),
                  (124,'interactive','view','{"kind":"help"}','sent',0);""")

    try:
        for network in networks:
            run(["docker", "network", "create", "--internal", network], capture_output=True)
        # Fresh schema: serve would fail CheckSchema if ordering were wrong.
        up()
        history = sql("SELECT version,checksum,applied_at FROM schema_migrations ORDER BY version")
        up()
        assert (
            sql("SELECT version,checksum,applied_at FROM schema_migrations ORDER BY version")
            == history
        )
        # A running backend must be stopped and migration must run AGAIN on each up.
        sql("UPDATE schema_migrations SET checksum='fixture-corruption' WHERE version=4")
        up(success=False)
        assert sql("SELECT checksum FROM schema_migrations WHERE version=4") == "fixture-corruption"

        # Real SQL failure after ALTER TABLE: all pending changes must roll back.
        schema3()
        sql("CREATE INDEX outbound_interactive_due ON outbound_messages(id)")
        up(success=False)
        assert sql("SELECT max(version) FROM schema_migrations") == "3"
        assert (
            sql(
                "SELECT count(*) FROM information_schema.columns WHERE table_name='outbound_messages' AND column_name='update_bot_id'"
            )
            == "0"
        )
        assert sql("SELECT body->>'kind' FROM outbound_messages WHERE chat=123") == "help"
        sql("DROP INDEX outbound_interactive_due")
        up()
        assert sql("SELECT name FROM users WHERE telegram_id=123") == "Synthetic participant"
        assert (
            sql("SELECT body->>'kind',status,attempts FROM outbound_messages WHERE chat=123")
            == "help_participant|retry_wait|2"
        )
        assert (
            sql("SELECT body->>'kind',status FROM outbound_messages WHERE chat=124") == "help|sent"
        )

        # Concurrent actual CLI containers wait for the advisory transaction lock.
        schema3()
        locker = subprocess.Popen(
            compose
            + [
                "exec",
                "-T",
                "postgres",
                "psql",
                "-X",
                "-U",
                "registration",
                "-d",
                "registration",
                "-v",
                "ON_ERROR_STOP=1",
            ],
            cwd=root,
            env=env,
            stdin=subprocess.PIPE,
            text=True,
            stdout=subprocess.DEVNULL,
        )
        assert locker.stdin is not None
        locker.stdin.write(
            "SET idle_in_transaction_session_timeout='120s'; BEGIN; SELECT pg_advisory_xact_lock(725046901);\n"
        )
        locker.stdin.flush()
        processes = []
        try:
            for _ in range(50):
                if (
                    sql("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND granted")
                    == "1"
                ):
                    break
                time.sleep(0.1)
            else:
                raise AssertionError("fixture lock not acquired")
            for target in ("up", "migrate-compose"):
                processes.append(
                    subprocess.Popen(make + [target], cwd=root, env=env, stdout=subprocess.DEVNULL)
                )
            for _ in range(300):
                if (
                    sql("SELECT count(*) FROM pg_locks WHERE locktype='advisory' AND NOT granted")
                    == "2"
                ):
                    break
                time.sleep(0.1)
            else:
                raise AssertionError("migrators did not serialize on advisory lock")
            assert sql("SELECT max(version) FROM schema_migrations") == "3"
            assert backend_state() in ("exited", "absent"), (
                "backend started before migration committed"
            )
            locker.stdin.write("COMMIT;\n\\q\n")
            locker.stdin.flush()
            locker.stdin.close()
            assert locker.wait(timeout=30) == 0
            for process in processes:
                assert process.wait(timeout=60) == 0
            assert sql("SELECT count(*) FROM schema_migrations") == "4"
            assert sql("SELECT name FROM users WHERE telegram_id=123") == "Synthetic participant"
        finally:
            for process in [locker, *processes]:
                if process.poll() is None:
                    process.terminate()
                    process.wait(timeout=10)
        up()
        print(
            "Compose lifecycle: fresh/repeat, stop-on-failure, SQL rollback, v3 upgrade, concurrent migration passed"
        )
    finally:
        run(compose + ["--profile", "tools", "down", "--volumes", "--remove-orphans"])
        for network in networks:
            subprocess.run(
                ["docker", "network", "rm", network],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            )
        subprocess.run(
            ["docker", "image", "rm", name + "-backend"],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        )
