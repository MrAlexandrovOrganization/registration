"""Creates a disposable PostgreSQL instance. Never reads .env or uses production volumes."""

import os
import socket
import subprocess
import time
import uuid

name = "registration-test-" + uuid.uuid4().hex[:10]
kafka_name = name + "-kafka"
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
            "--publish",
            "127.0.0.1::5432",
            "--tmpfs",
            "/var/lib/postgresql/data",
            "--env",
            "POSTGRES_PASSWORD=fixture",
            "--env",
            "POSTGRES_DB=registration_test",
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
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        kafka_port = sock.getsockname()[1]
    kafka_env = {
        "CLUSTER_ID": "MkU3OEVBNTcwNTJENDM2Qk",
        "KAFKA_NODE_ID": "1",
        "KAFKA_PROCESS_ROLES": "broker,controller",
        "KAFKA_LISTENERS": f"INTERNAL://:9092,CONTROLLER://:9093,EXTERNAL://:{kafka_port}",
        "KAFKA_ADVERTISED_LISTENERS": f"INTERNAL://localhost:9092,EXTERNAL://127.0.0.1:{kafka_port}",
        "KAFKA_LISTENER_SECURITY_PROTOCOL_MAP": "INTERNAL:PLAINTEXT,EXTERNAL:PLAINTEXT,CONTROLLER:PLAINTEXT",
        "KAFKA_INTER_BROKER_LISTENER_NAME": "INTERNAL",
        "KAFKA_CONTROLLER_LISTENER_NAMES": "CONTROLLER",
        "KAFKA_CONTROLLER_QUORUM_VOTERS": "1@localhost:9093",
        "KAFKA_OFFSETS_TOPIC_REPLICATION_FACTOR": "1",
        "KAFKA_GROUP_INITIAL_REBALANCE_DELAY_MS": "0",
        "KAFKA_HEAP_OPTS": "-Xms256m -Xmx512m",
    }
    command = [
        "docker",
        "run",
        "--detach",
        "--rm",
        "--name",
        kafka_name,
        "--label",
        "registration.fixture=true",
        "--publish",
        f"127.0.0.1:{kafka_port}:{kafka_port}",
        "--tmpfs",
        "/var/lib/kafka/data:uid=1000,gid=1000",
    ]
    for key, value in kafka_env.items():
        command.extend(["--env", f"{key}={value}"])
    subprocess.run(command + ["confluentinc/cp-kafka:" + os.environ["KAFKA_VERSION"]], check=True)
    for _ in range(60):
        if (
            subprocess.run(
                [
                    "docker",
                    "exec",
                    kafka_name,
                    "kafka-broker-api-versions",
                    "--bootstrap-server",
                    "localhost:9092",
                ],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                timeout=15,
            ).returncode
            == 0
        ):
            break
        time.sleep(1)
    else:
        raise SystemExit("fixture Kafka did not become ready")
    env = dict(
        os.environ,
        TEST_DATABASE_URL=f"postgres://postgres:fixture@{address}/registration_test?sslmode=disable",
        TEST_KAFKA_BROKER=f"127.0.0.1:{kafka_port}",
    )
    subprocess.run(
        [
            "go",
            "test",
            "-race",
            "-tags=integration",
            "./internal/integration",
            "-count=1",
            "-timeout=180s",
        ],
        env=env,
        check=True,
    )
finally:
    subprocess.run(
        ["docker", "rm", "--force", name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL
    )
    subprocess.run(
        ["docker", "rm", "--force", kafka_name],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    )
