# Registration backend

Go 1.26.1, PostgreSQL, gRPC, Kafka outbox. Read README and docs/MIGRATION.md.
Original SQLite is immutable input, not application storage. No real import in tests.
Preserve users/user_permissions/bot_chats and legacy answer strings. New event,
manual PostgreSQL cleanup, ask missing answers and require confirmation.

Commands: make install, format, check, test-race, build, compose-build.
Generated *.pb.go are ignored; .proto is versioned. Make generates before Go
checks/build/run/CLI; Docker generates independently with the pinned toolchain.
`make check` starts disposable PostgreSQL/Kafka fixture containers, never production.
`make migrate`, `init-topics`, `up` and non-dry-run import modify the selected environment;
run only as a deliberate operation. Hooks install separately via install-hooks.

SQL belongs in internal/store/*.sql; schema versions are immutable after deployment.
All Telegram effects are durable jobs, never an RPC-side HTTP call. Auth and permission
checks belong here. Do not log DSNs, errors containing parameters, or request payloads.
Interactive replies: Accept returns durable delivery_ids; PendingInteractive is
bounded discovery/recovery, never a lease. Preserve Claim/Complete fencing/order/retry.
Kafka publishes broadcast only. See docs/INTERACTIVE.md for the additive contract,
coordinated frontend compatibility boundary and safe callback edit attestation.
Participant help/refusals must not reveal roles/permissions; operator views are private.
No commits/push/deployment unless requested. Read docs/OPERATIONS.md before delivery.
CI validates/builds on PR/push main; SSH CD requires repository DEPLOY_ENABLED=true.
Keep CD disabled until provisioning and coordinated migration 004 rollout are complete.
SSH workflow fast-forwards main to the checked SHA, then runs the same make up as locally.
Deliberately approved project exception to explicit-only migrations: make up builds
backend once, stops the old backend, waits for PostgreSQL, runs migrate-compose in a
fresh disposable CLI container, then starts backend without rebuilding and waits for
healthchecks (180s). Local and SSH CD use this same sequence. Database/migration
failure leaves backend stopped; build failure leaves the old backend untouched.
No automatic rollback. serve (including make run/restart) only checks the schema.
Do not run concurrent make up operations for the same Compose project; the CD flock
serializes delivery, while the database advisory lock serializes CLI migrations.
make check includes make test-compose: real make up on unique synthetic tmpfs fixtures,
including upgrade/repeat/concurrency/failure. Never use live networks or data for tests.
No automatic import, topics or webhook operations in CI/CD.
