"""Measure Accept on a disposable tmpfs PostgreSQL; never read live configuration."""

import os
import subprocess
import time
import uuid

name = "registration-benchmark-" + uuid.uuid4().hex[:10]
try:
    subprocess.run(
        [
            "docker",
            "run",
            "--detach",
            "--rm",
            "--name",
            name,
            "--label",
            "registration.fixture=true",
            "--cpus",
            "1",
            "--memory",
            "512m",
            "--publish",
            "127.0.0.1::5432",
            "--tmpfs",
            "/var/lib/postgresql/data",
            "--env",
            "POSTGRES_PASSWORD=fixture",
            "--env",
            "POSTGRES_DB=registration_benchmark",
            "postgres:" + os.environ["POSTGRES_VERSION"],
        ],
        check=True,
    )
    for _ in range(60):
        if (
            subprocess.run(
                ["docker", "exec", name, "pg_isready", "-U", "postgres"],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
            ).returncode
            == 0
        ):
            break
        time.sleep(1)
    else:
        raise SystemExit("fixture PostgreSQL did not become ready")
    address = subprocess.check_output(["docker", "port", name, "5432/tcp"], text=True).strip()
    env = dict(
        os.environ,
        TEST_DATABASE_URL=f"postgres://postgres:fixture@{address}/registration_benchmark?sslmode=disable",
        GOMAXPROCS="1",
    )
    subprocess.run(
        [
            "go",
            "test",
            "-tags=integration",
            "./internal/integration",
            "-run=^$",
            "-bench=^BenchmarkAccept$",
            "-benchtime=3s",
            "-count=1",
            "-timeout=120s",
        ],
        env=env,
        check=True,
    )
finally:
    subprocess.run(
        ["docker", "rm", "--force", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
