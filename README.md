# Registration backend

## Версии сборки

Go и protoc-gen-go берутся из `go.mod`; образы и остальные инструменты —
из `versions.mk`; Python-инструменты — из `requirements-tools.txt`.
`make versions` показывает значения. Make экспортирует их в Compose, скрипты
проверок и CI. Сборка образов: `make compose-build`; запуск: `make up`.
После изменения инструментов повторите `make install`.

Go-backend регистрации на новый выезд. Владеет PostgreSQL, анкетой, правами,
служебными чатами, XLSX-экспортом и надёжными заданиями доставки. Telegram API
вызывает отдельный frontend. Межсервисный контракт — gRPC/protobuf; HTTP нужен
только для `/livez`, `/readyz`, `/metrics`.

### Временное отключение вопросов об участии

`PARTICIPATION_ENABLED=false` (по умолчанию и в Compose) скрывает вопросы
`will_drive` и `trip_attendance`, их ответы в анкете/редактировании, статистике и XLSX.
Опрос `/poll`, аудитории yes/maybe и уведомления о порогах участия отключены.
Итоговое подтверждение самой анкеты остаётся: это не подтверждение участия.
Старые ответы хранятся без изменений, старые кнопки этих вопросов не принимаются;
незавершённое состояние такого вопроса переходит к актуальной анкете.
Чтобы включить позже, поставить `PARTICIPATION_ENABLED: 'true'` в Compose backend
и пересоздать backend. Миграции/удаление данных не нужны. Полный `domain.Fields`
не сокращать — он задаёт порядок SQL-колонок; активные поля выбираются отдельно.

## Структура и решения

- `cmd/registration`: lifecycle и CLI; `internal/app`: конфигурация и сборка приложения.
- `internal/domain`: независимые валидация/нормализация полей.
- `internal/service`: регистрация, административные сценарии, доставка.
- `internal/store`: PostgreSQL adapter с `go:embed` SQL и миграциями.
- `internal/legacy`: standalone SQLite только read-only; Python-код не загружается.
- `internal/queue`: publisher broadcast outbox → Kafka; interactive outbox обнаруживается через gRPC.
- `api/registration.proto`: канонический контракт; `*.pb.go` генерируются локально и игнорируются Git.

Сохраняем предметные таблицы `users`, `user_permissions`, `bot_chats`, имена
полей и старые строковые варианты ответа. `will_drive` означает намерение
поехать, не наличие автомобиля. Даты рождения пока TEXT `ДД.ММ.ГГГГ`, чтобы
сохранить существующие значения; технические даты — TIMESTAMPTZ, ID — BIGINT.
Новый ввод допускает день/месяц без ведущего нуля (`10.3.2002`) и сохраняется
канонически (`10.03.2002`). Импортированные значения автоматически не переписываются.
У пользователя одна актуальная анкета. Прошлый выезд и переписка остаются
в архивной SQLite. `users.version` — техническая версия для защиты callback.

Технические таблицы: `processed_updates`, `broadcasts`, `outbound_messages`,
`runtime_state`, `import_runs`, `milestone_notifications`, `audit_events`,
`schema_migrations`. Нет отдельной таблицы профилей, событий или универсальных ответов.

Телефон проверяется по прежнему правилу: 11 цифр с начальной 7 или 8;
пробелы, скобки, дефисы и начальный плюс допускаются, 8 нормализуется в 7.
Буквы и добавочные номера не принимаются. Группа проверяется по прежнему шаблону
`^[А-Я]{1,5}[0-9]{0,2}[СИЦ]?-([1-9]|1[0-6])[1-9][АБМТ]?В?$`
после удаления крайних пробелов и приведения к верхнему регистру, например
`ИУ7-41`, `РК6-71Б`. Проверки одинаковы для нового ввода, редактирования и
определения следующего вопроса. Невалидные сохранённые ответы запрашиваются заново,
но миграция и автоматическое переписывание старых значений не выполняются.

`first_starts` хранит неизменяемый первый источник запуска на Telegram-пользователя.
Миграция `002_first_starts.sql` добавляет её, не меняя начальную миграцию.

Для небольшого adapter используется `pgx` + SQL в файлах вместо дополнительного
генератора sqlc. Мигратор выполняет версионированный SQL в транзакции под advisory
lock, хранит SHA-256 версии; применённый файл изменять нельзя. Процесс `serve`
только проверяет совместимость схемы. `make up` автоматически запускает отдельный
CLI мигратора до запуска backend — одинаково локально и в SSH CD.

Это сознательно согласованное исключение из общего правила явных миграций:
пользователь выбирает цикл «разработка → локальные/CI проверки → применение CD».
Отдельное ручное применение схемы между проверенным релизом и `make up` больше
не требуется. Последовательность одной команды:

1. Собрать образ backend **один раз**; ошибка сборки не останавливает старый backend.
2. Остановить прежний backend, чтобы он не работал с изменяющейся схемой.
3. Запустить PostgreSQL и дождаться healthcheck (до 180 секунд).
4. `make migrate-compose`: новый `run --rm` контейнер из того же образа, только
   с доступом к database-сети, без сервера. Каждый `up` повторяет этот шаг:
   сохранённый завершённый Compose service не используется как признак успеха.
5. Только после exit 0 запустить backend без сборки/pull и дождаться readiness
   (до 180 секунд). `restart` не заменяет применение новой версии через `up`.

На каждом запуске проверяются checksums и совместимость версии. Все ожидающие
миграции и их история применяются одной транзакцией под advisory lock; ошибка
SQL откатывает весь набор. CLI ограничен пятью минутами, включая ожидание lock.
При ошибке PostgreSQL/миграции команда завершается неуспешно, backend остаётся
остановленным. При ошибке readiness схема уже могла обновиться; автоматического
отката контейнеров или данных нет. Исправить причину и повторить `make up`;
правила восстановления — в OPERATIONS/MIGRATION. Это обновление с коротким простоем,
не rolling deployment. Параллельные `make up` одного проекта не поддерживаются;
CD сериализован flock, а advisory lock защищает конкурентные CLI-миграции.
Несовместимый frontend (в том числе переход 003→004) останавливается отдельно
по coordinated rollout. Импорт, топики и webhook остаются явными операциями.

Миграции автоматически обнаруживаются в embedded-каталоге
`internal/store/migrations/` и сортируются по числовому префиксу. Для новой миграции
достаточно добавить файл `NNN_description.sql` и пересобрать приложение; список в
Go-коде менять не нужно. Номера идут подряд от 1, без повторов и пропусков.
Некорректное имя или последовательность приводят к ошибке до применения SQL.

## Разработка

Локальные измерения gRPC Accept с временной PostgreSQL: `make benchmark`.
Сценарий, ограничения и результаты — [PERFORMANCE.md](docs/PERFORMANCE.md).

Prerequisites: Go из `make versions`, Python 3 с venv/pip (в CI версия из
`make versions`), Make; Docker Compose v2 для интеграционных/конфигурационных
проверок и контейнеров. Эти системные инструменты устанавливаются вручную.
Для установки нужен HTTPS-доступ к Go module proxy и PyPI.
На Ubuntu (как runner CI `ubuntu-24.04`) установите Go нужной версии и пакеты
`make ca-certificates python3-venv`, затем выполните `make install && make build`.
`make install` загружает Go-зависимости по `go.sum`, устанавливает закреплённые
Buf, Go-генераторы/goimports в `.bin`, pre-commit и Ruff в `.tools`
(Python-зависимости закреплены в requirements-tools.txt). Hooks отдельно.

Как в notes-bot, `make install-proto` — три команды `go install`: Buf и локальные
`protoc-gen-go` / `protoc-gen-go-grpc`. Buf и gRPC plugin закреплены в `versions.mk`,
версия Go protobuf plugin берётся из `go.mod`. Отдельный protoc не нужен.
`make proto-gen` вызывает `buf generate`; минимальные `buf.yaml` и `buf.gen.yaml`
сохраняют каноническое имя `api/registration.proto` и выходные файлы
`api/registration.pb.go`, `api/registration_grpc.pb.go` (`paths=source_relative`).
Make ставит `BIN` первым в PATH для локальных plugins. Можно задать
`make install-proto BIN=/absolute/path/to/empty/bin` и использовать тот же BIN
в `proto-gen`, build/test. Для локальной генерации Docker не нужен; build-stage
Dockerfile и CI устанавливают инструменты через тот же Makefile.

Как в notes-bot, generated protobuf не хранится в Git. `make build`, `test`,
`test-race`, `check`, `format`, `run` и CLI-цели сначала выполняют `proto-gen`.
Для прямого `go test`/`go build` после checkout сначала нужен `make proto-gen`.
Отдельная проверка сравнения с закоммиченным generated-кодом удалена: проверяются
генерация, компиляция и тесты. Docker исключает локальные `*.pb.go` из контекста,
устанавливает закреплённый toolchain и генерирует их самостоятельно в build-stage.

```sh
make install
make install-hooks
make format
make check
make test-race
make build
```

`format` выполняет goimports, безопасные модернизации `go fix` и Ruff для скриптов. `check` ничего
не форматирует: проверяет генерацию, формат/импорты/go fix, vet, unit-тесты,
интеграцию с временными PostgreSQL **17.9** и Kafka **8.2.0**, Compose на
синтетических параметрах, Gitleaks 8.24.2 с полным redaction. Сканируются только
версионируемые файлы, локальные игнорируемые credentials не читаются.
Docker должен быть доступен; недоступность — ошибка,
не пропуск тестов. Тестовые контейнеры имеют уникальные имена, tmpfs и удаляются.
`test` — быстрые тесты без Docker; `test-integration` — отдельный запуск интеграции.
`test-compose` (также в `check`) собирает реальный образ и вызывает настоящий
`make up` на уникальном Compose-проекте: пустая БД, повтор, upgrade 003→004
с сохранением анкеты/заданий, checksum failure при работающем backend, откат
SQL failure и конкурентные CLI под advisory lock. Используются синтетический
config, PostgreSQL tmpfs, уникальные временные external networks без live-сервисов
и без host ports. Контейнеры/сети/тестовый image удаляются в finally.

Pre-commit форматирует только staged Go-файлы, исключает generated; framework
сохраняет unstaged-правки и останавливает коммит после исправлений. Автоматического
`git add` нет, неизвестные hooks не перезаписываются.

CI на PR и push main вызывает `make install`, `make check`, `make test-race`,
`make build`; отдельный job выполняет `make compose-build`. SSH CD зависит от
успеха обоих jobs и включается только repository variable `DEPLOY_ENABLED=true`
для push main. По умолчанию деплой пропущен. Репозитории пока не опубликованы.
Все jobs используют `ubuntu-24.04`. Deploy использует environment `production`,
обновляет main fast-forward до проверенного SHA
и вызывает `make up`, как при локальном запуске: сборка, остановка backend,
готовность PostgreSQL, миграции, запуск backend и проверка healthchecks.
Настройки и coordinated rollout migration 004 — [OPERATIONS.md](docs/OPERATIONS.md),
первый импорт — [MIGRATION.md](docs/MIGRATION.md).

## Локальный запуск

В `.env` для Compose нужны только четыре значения: `POSTGRES_PASSWORD`,
`BACKEND_TOKEN`, `BOT_ID`, `ROOT_ID`. Для базы задаётся только пароль: пользователь
и база — `registration`, внутренний адрес — `postgres:5432`, `DATABASE_URL`
собирается в Compose. Пароль должен быть URL-safe (например, случайная hex-строка),
поскольку подставляется в URI. Остальные настройки заданы в `docker-compose.yml`.

Для запуска на хосте:

1. Подготовить `.env` по `.env.example` и экспортировать нужные переменные в shell.
   `make run` сам `.env` не загружает; Compose читает его штатным способом.
2. Для PostgreSQL: `make database-up` (host port 127.0.0.1:55432).
3. Выполнить `make migrate`; затем `make run`. Остановка локального процесса — Ctrl-C.
4. Kafka должна быть доступна по advertised address из `KAFKA_BROKERS`.
   Установка на VM рекламирует `kafka:9092`: один SSH tunnel к порту не делает
   её доступной локальному приложению. Для полного запуска использовать Compose
   на том же Docker host либо отдельную dev Kafka с корректным advertised listener.

Host CLI использует `DATABASE_URL`, а не `POSTGRES_PASSWORD` напрямую. После
экспорта пароля сформировать адрес локальной БД без дублирования пароля в `.env`:

```sh
export DATABASE_URL="postgres://registration:${POSTGRES_PASSWORD:?required}@127.0.0.1:55432/registration?sslmode=disable"
```

Перед `make migrate` достаточно этого адреса. Для `make run` дополнительно задать
`ALLOW_INSECURE_GRPC=true`, `GRPC_LISTEN=127.0.0.1:50051`,
`HTTP_LISTEN=127.0.0.1:9090` и доступный с хоста `KAFKA_BROKERS`. Настройки из
Compose при host-запуске автоматически не применяются.

Работающий backend всегда принимает регистрацию и обрабатывает доставку.
Для работы бота нужны подготовленные frontend, база и топики Kafka. При временной
недоступности Kafka ответы сохраняются в outbox и публикуются после восстановления.
Для обслуживания остановить frontend и backend; CLI миграции и импорта запускаются
отдельно без сервера. `MILESTONES` и настройки инфраструктуры меняются в Compose.

## Конфигурация

Переменные приложения ниже — справочник для host-запуска и Compose, а не список
обязательных полей `.env`.

| Переменная | Назначение |
|---|---|
| `POSTGRES_PASSWORD` | Единственная настройка БД в `.env`; Compose использует её для PostgreSQL и сборки DSN |
| `DATABASE_URL` | PostgreSQL DSN, секрет; Compose задаёт внутренний адрес |
| `BACKEND_TOKEN` | Общий высокоэнтропийный секрет frontend/backend, минимум 32 символа |
| `ROOT_ID` | Telegram ID владельца, имеет все права |
| `BOT_ID` | Числовая идентичность Telegram-бота, namespace дедупликации |
| `GRPC_LISTEN` / `HTTP_LISTEN` | По умолчанию `:50051` / `:9090` |
| `GRPC_TLS_CERT` / `GRPC_TLS_KEY` | TLS сервера; с другой машиной использовать TLS |
| `ALLOW_INSECURE_GRPC` | Только явное `true` разрешает plaintext в доверенной внутренней сети |
| `KAFKA_BROKERS` | Список адресов через запятую, по умолчанию `kafka:9092` |
| `KAFKA_TOPIC_PREFIX` | По умолчанию `registration.telegram` |
| `MILESTONES` | Пороговые значения подтверждённых анкет желающих поехать, без организаторов |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | В Compose `http://jaeger:4317`; вне Compose необязателен |
| `OTEL_EXPORTER_OTLP_INSECURE` | В Compose true: OTLP без TLS внутри `jaeger-net` |

В Compose PostgreSQL подключена только к отдельной database-сети; frontend
подключается к `registration-api` и существующей `kafka-net`. Опубликованные
порты доступны только с loopback хоста. Текущий профиль предполагает одну
доверенную VM; plaintext Kafka не обеспечивает ACL между её клиентами.
Для DBeaver использовать SSH-туннель к `127.0.0.1:55432` на сервере. Backend
продолжает обращаться к `postgres:5432` внутри Docker. После изменения сети
пересоздать контейнеры через Compose: ручное переподключение без `--alias postgres`
теряет DNS-имя БД и приводит к `database unavailable`.
Backend также подключён к существующим `jaeger-net` и `prometheus-net`, как frontend
и `notes-bot`. Новый стек мониторинга не создаётся.

## Отправка и рассылки

### Direct interactive gRPC

`Accept` возвращает `Receipt.delivery_ids` после атомарного сохранения ответа.
Повтор update возвращает те же ID без повторного выполнения сценария. Frontend
передаёт их общему sender с прежними Claim/Complete, lease и limiter. Bounded RPC
`PendingInteractive` восстанавливает ответы, due retries, exports, sync и milestones
после сбоя; Kafka обслуживает только broadcast. Это синхронный результат команды,
а не Telegram HTTP внутри RPC и не очередь только в памяти.

Точный контракт, правила callback edit и recovery:
[INTERACTIVE.md](docs/INTERACTIVE.md). Изменение protobuf additive, но требуется
coordinated transition backend/frontend с миграцией 004: legacy/dual-delivery
режима нет, старый frontend не умеет получать новые interactive jobs.

Недоступные административные команды backend молча завершает: update сохраняется
для дедупликации, но outbox-ответ и delivery IDs не создаются. Неизвестные команды
также игнорируются. Обычная `/help` содержит только публичные команды (`help_public`),
без отдельного интерфейса участника. Operator help и `/my_permissions` доступны только в личном
чате root/владельцам admin, table_viewer или message_sender. Ролевые flags и staff
permission сами по себе не открывают operator help. В группе help всегда публичный.
При Claim доступ к queued административным ответам проверяется заново; недоступные
задания отменяются без отправки и без замены отказом. Ошибки анкеты, некорректное
содержимое сообщения и обычная информация по-прежнему получают ответы.

### Учёт источников

Ссылка `https://t.me/ИМЯ_БОТА?start=website` поступает как `/start website`.
Первый `/start` в личном чате атомарно сохраняет метку и время вместе с update
и ответом в outbox. `PRIMARY KEY(telegram_id)` и `ON CONFLICT DO NOTHING` защищают
от повторов и конкурентных запусков. Источник сохраняется независимо от завершения
анкеты и не сбрасывается при перезапуске. Групповые `/start` не учитываются.

Метки регистрозависимы, 1–64 символа `[A-Za-z0-9_-]`. Нет метки или она невалидна —
сохраняется пустая строка («Без метки»), позднее она не заменяется рекламной меткой.
Миграция и импорт создают историческим пользователям запись с NULL source/time:
их исходный источник неизвестен и будущие ссылки не должны получать за них зачёт.
Новые пользователи без `/start` записи в `first_starts` ещё не имеют.

`/sources [СТРАНИЦА]` доступна root/`table_viewer`, выдаёт агрегаты по 20 источников.
Считаются пользователи, не клики и не повторные запуски; одинаковая метка объединяет
все ссылки. XLSX дополнен `first_start_source`, `first_start_status`
(`tagged`, `direct`, `unknown`, `not_started`) и `first_start_at_utc`.
Frontend отображает новый View `sources` через существующий контракт protobuf.

При обновлении остановить приём, обновить backend через `make up` (включает
миграции), затем совместимый frontend и возобновить работу. Старые источники задним
числом не восстанавливаются; host startup проверяет схему, но не применяет её.

### Надёжность доставки

Callback анкеты содержит версию состояния пользователя; устаревшие кнопки
отклоняются. Отдельного номера выезда нет. Миграция `003_single_registration.sql`
убирает epoch из уведомлений о порогах, сохраняя каждый уже отправленный порог
один раз. `make up` применяет миграции при остановленном backend; для host-запуска
использовать отдельный `make migrate` при остановленных сервисах.
Кнопки старого формата после обновления предлагают заново открыть `/start`.

Изменение анкеты, дедупликация update, создание ответа и связь update → jobs — одна транзакция.
Получатели рассылки фиксируются при `/send ID`, уникальны по `(broadcast_id,chat)`.
Kafka несёт только ID задания рассылки, ключ чата и traceparent, не содержимое анкеты.
Publisher ждёт `acks=all`, затем отмечает публикацию. Через 30 секунд незавершённое
broadcast-задание может публиковаться снова: PostgreSQL остаётся источником состояния,
потеря Kafka event/retention не означает потерю задания.

Sender получает эксклюзивный lease 90 секунд; таймаут обработки 55 секунд плюс
10 секунд на отчёт. Устаревший worker не может завершить захваченное заново
задание. Короткий глобальный lease ограничивает систему одним активным sender.
При штатной остановке новый frontend вызывает `ReleaseSender` после завершения
sender/heartbeat и отчётов Complete. Backend атомарно снимает только lease указанного
worker, сохраняя cooldown и leases отдельных заданий. Миграция БД не требуется.
После аварийного завершения, ошибки освобождения или остановки старого frontend
перезапуск по-прежнему может ожидать истечения lease до 90 секунд.

- `429`: сохраняются `next_attempt_at` и общий cooldown бота, failure budget не тратится.
- Временная/неопределённая ошибка: exponential backoff + jitter, максимум 8 неудачных попыток.
- `403` отправки: получатель недоступен, дальнейшие pending-задания прекращаются.
- Ошибка содержимого: конечная ошибка и пауза соответствующей рассылки.
- Отмена прекращает неотправленные задания; уже выполняющийся вызов может завершиться.

Гарантия — повторяемая обработка с дедупликацией зафиксированных результатов,
**не exactly-once Telegram**. Если Telegram принял сообщение, а результат потерян,
повтор возможен. Один брокер и PostgreSQL на одной VM не дают HA при потере VM.

Топики создаются **отдельно**, не при старте приложения:

```sh
make init-topics
```

Команда создаёт только `<prefix>.broadcast.v1` (interactive доставляется через gRPC):
3 партиции, replication factor 1 для существующего single-node Kafka,
retention 7 суток / 256 MiB на партицию, max message 64 KiB. Существующие
конфигурации не изменяет. Запуск на VM — после проверки доступного диска.

## Наблюдаемость и сопровождение

JSON stdout: `time` UTC, `level`, `msg`, `service`; `trace_id`/`span_id` из
активного контекста. Сырые updates, анкеты, токены, SQL-параметры не логируются.
События `rpc.completed`, `update.committed`, `update.replayed`, `delivery.completed`
дают безопасные результаты/длительность/счётчики; Claim/PendingInteractive success
пишутся на Debug. Authentication failures также учитываются. Ошибки содержат
только классификацию, stage и проверенный SQLSTATE, без raw error.
gRPC stats handlers переносят OTel
контекст; traceparent сохраняется с outbox и восстанавливается sender.

`/livez` проверяет процесс, `/readyz` — PostgreSQL. Kafka — восстанавливаемая
зависимость доставки, поэтому её сбой не делает регистрацию недоступной.
Метрики: `registration_rpc_total`, `registration_rpc_seconds`,
`registration_outbound_pending`, `registration_outbound_oldest_seconds`.
Нет идентификаторов пользователей/чатов в labels.

Подключение к действующим Prometheus/Alloy/Jaeger описано в
[OPERATIONS.md](docs/OPERATIONS.md). Наличие SDK не означает, что события уже
найдены в Grafana; runtime-проверка выполняется после доставки.
