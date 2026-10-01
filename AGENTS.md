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
No commits/push/deployment unless requested. CI currently validates/builds, not deploys.
