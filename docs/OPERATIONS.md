# Эксплуатация

## CI/CD и provisioning

Независимый репозиторий пока не опубликован. `.github/workflows/ci.yml` проверяет
PR и push main через Make; `check` включает binary build, отдельный `build` собирает
Compose-образ. Deploy зависит от обоих jobs, работает только на push main при
**repository variable `DEPLOY_ENABLED=true`**. Отсутствующее/false значение отключает CD.
Все jobs используют `ubuntu-24.04`, как notes-bot и subscription-catalog.
Deploy job использует GitHub environment `production`: в нём можно хранить environment
secrets и настроить protection rules, например required reviewers и разрешённую ветку main.
Само `environment: production` не включает approval: он требуется только при настроенном
правиле. Проверкам production secrets не нужны.

В каждом из двух репозиториев настроить отдельно repository/environment secrets:

| Secret | Назначение |
|---|---|
| `VM_HOST` | SSH hostname/IP VM (порт 22) |
| `VM_USER` | Выделенный пользователь с Docker-доступом |
| `VM_SSH_KEY` | Приватный ключ для подключения Actions → VM |
| `VM_PROJECT_PATH` | Абсолютный путь **этого** checkout на VM |

Для backend ожидаемый путь `/home/maxim/projects/backends/registration`, для frontend —
`/home/maxim/projects/telegram-bots/registration-bot`; фактические пути проверить при
provisioning. VM нужны Git, Bash, Make, `flock` (util-linux), Docker/Compose v2 с
`up --wait --wait-timeout`, исходящий доступ к GitHub и registry. Go на VM не нужен.
Создать независимые чистые clones на main с origin соответствующего репозитория.
Для private origin настроить **отдельный**
read-only deploy key VM → GitHub и known_hosts; VM_SSH_KEY этого доступа не даёт.

Pinning SSH host key для подключения Actions → VM не настроен.

Секреты приложения остаются в защищённых `.env` на VM, не в Actions artifacts.
Подготовить Kafka и external networks `kafka-net`, `jaeger-net`, `prometheus-net`;
backend создаёт `registration-api`, frontend использует её как external. Сохранить
имя Compose-проекта `registration` и существующий volume PostgreSQL. Настроить
backup вне VM и проверить восстановление. Импорт и создание broadcast-топика —
только явные операции по MIGRATION, не workflow/startup.

SSH-шаг workflow требует чистый корень репозитория (включая untracked-файлы, кроме
игнорируемых) и main, получает main из origin и сверяет FETCH_HEAD с SHA успешно
проверенного push. Затем выполняет fast-forward только на этот SHA и проверяет HEAD.
Устаревший workflow, divergent checkout или локальные правки приводят к отказу.
После обновления вызывается `make up` — та же команда, что локально: собирает
один образ, останавливает backend, ждёт PostgreSQL, выполняет новый одноразовый
`migrate-compose`, затем запускает backend без сборки/pull и ждёт healthcheck.
Ожидание PostgreSQL и backend ограничено 180 секундами каждое, CLI migrate — 5 минутами.
Backend healthcheck проверяет `/readyz` с PostgreSQL; готовность Kafka этим не подтверждается.
CI-образ проверяется, но не публикуется/не переносится на VM. Автоматические миграции
в `make up` согласованы пользователем; импорт, топики и webhook автоматически
не выполняются. Deploy jobs не отменяют друг друга;
общий `$HOME/.registration-deploy.lock` сериализует оба репозитория при одном VM_USER.
Этот lock **не задаёт порядок релизов** и не заменяет coordinated rollout.

## Coordinated rollout migration 004

1. Оставить/выставить `DEPLOY_ENABLED=false` в **обоих** репозиториях; дождаться
   окончания уже запущенных deploy jobs. Выбрать совместимую пару SHA с зелёным CI,
   одинаковым proto snapshot и direct/PendingInteractive frontend. См. INTERACTIVE.md.
2. Остановить старый frontend (включая Python poller), затем backend, сохранив БД.
   Не использовать `down -v`. Для webhook отключить маршрутизацию к старому receiver
   с ответом 503 на время окна; не подтверждать потерянные updates ответом 200.
3. Сохранить PostgreSQL backup вне VM. Обновить оба чистых checkout на выбранную
   пару SHA. Это ручное окно обслуживания,
   не независимые автоматические деплои.
4. В backend выполнить `make up`: сборка, готовность PostgreSQL, автоматическое
   применение миграций до 004, затем запуск backend. CLI `serve` сам миграции не
   выполняет. Для уже перенесённой БД повторный импорт SQLite не нужен.
   Pending jobs, leases и sent statuses не сбрасывать.
5. Проверить backend `127.0.0.1:9095/readyz` и gRPC
   `registration-backend:50052`. Затем в новом frontend — `make up`.
   Для webhook настроить TLS proxy и выполнить явный register; для polling прежде
   удалить старый webhook. Подробности в frontend OPERATIONS.
6. Проверить readiness frontend `127.0.0.1:9093/readyz`, контролируемые Accept →
   direct reply и PendingInteractive recovery, callback edit, broadcast и trace.
   Старый frontend не совместим: backend публикует в Kafka **только broadcast**.
   Sender lease после остановки может освобождаться до 90 секунд.
7. Только после успешного переключения включить `DEPLOY_ENABLED=true` отдельно
   в обоих репозиториях. Следующий push main (либо повтор соответствующего workflow
   текущего SHA) сможет выполнить CD. Будущие несовместимые изменения схемы/контракта
   снова требуют отключения CD и согласованного окна.

При ошибке сборки старый backend ещё работает. После сборки он останавливается:
при ошибке БД/миграции новый backend не запускается. Мигратор проверяет SHA-256
применённых файлов и выполняет все ожидающие SQL в одной транзакции под advisory
lock: при SQL failure откатываются и DDL/DML, и записи истории. Повторный `make up`
обязательно запускает новый migrate-контейнер; завершённый контейнер не кешируется.
После исправления причины повторить `make up`. Не менять применённые SQL/checksums
ради обхода отказа. Параллельные ручные `make up` одного проекта запрещены;
CLI-миграции сериализуются в БД, но это не lock всего Compose lifecycle.

При ошибке readiness workflow завершается неуспешно, автоматического rollback нет;
часть контейнеров уже может быть обновлена. Сначала остановить приём и проверить SHA,
Compose state и безопасные логи. Откатить один компонент за границу 004 нельзя;
предпочтительно исправление вперёд. Restore согласуется с риском повторных отправок
из восстановленных delivery statuses; правила отката данных — в MIGRATION.md.

## Минимальная диагностика

`docker compose ps`, `/livez`, `/readyz`, затем метрики очереди. В логах искать
уровень, код RPC и SQLSTATE; не включать логирование payload и не печатать `.env`.
Зависшая очередь диагностируется по старейшему заданию, статусу рассылки,
`next_attempt_at`, глобальному cooldown и сроку lease. Не сбрасывать `sent` для
всей рассылки: `/retry ID` выбирает только временные/неопределённые ошибки.

Остановка контейнеров даёт 30 секунд на graceful shutdown. Новые запросы прекращаются,
выполняющиеся RPC ограничены 15 секундами; фоновые задачи останавливаются,
gRPC shutdown ограничен, телеметрия сбрасывается отдельным контекстом.

## Мониторинг

Compose подключает приложения к существующим `prometheus-net` и `jaeger-net`,
как в `notes-bot`; отдельный monitoring override не требуется. Для backend
настроить Prometheus scrape target `registration-backend:9095`, path `/metrics`:
этот alias задан в Compose на `prometheus-net`. Для frontend использовать его
сетевое имя в `prometheus-net` и порт 9093. Наличие сети не добавляет scrape target автоматически.
Контейнерный Prometheus не должен использовать host `127.0.0.1` как адрес приложений.

OTLP gRPC уже направлен в `http://jaeger:4317` через `jaeger-net`,
`OTEL_EXPORTER_OTLP_INSECURE=true`. Изменение collector/TLS выполняется в Compose.
Логи идут в stdout для Alloy,
не дублировать их отдельным OTLP log exporter. Loki labels — service/level,
trace ID — поле JSON, не label. JSON поле времени `time` содержит UTC RFC3339Nano.

После деплоя проверить успешную и неуспешную операцию: найти лог, открыть trace,
проверить frontend → backend и causal связь отложенной доставки. Sampling 10%,
поэтому для контрольной проверки временно адаптировать sampler или использовать
выбранный sampled trace. Runtime Grafana в локальных тестах не проверяется.

Полезные сигналы: readiness недоступен; рост `registration_outbound_oldest_seconds`
при открытой рассылке; `failed`/`rate_limit` в delivery counters; outbox publisher
повторно сообщает deferred. Kafka consumer lag дополнителен — не отражает задания,
которые ещё не опубликованы. Пороги подбираются по рабочему объёму.

## Резервное копирование

PostgreSQL — источник истины, Kafka не является backup анкет или результатов отправки.
Делать регулярный `pg_dump --format=custom` и копировать вне VM в защищённое хранилище.
Credentials передавать штатно через окружение/pgpass, не аргументом с паролем.
Сроки хранения и расписание определяет владелец инфраструктуры.

Пример локального dump при настроенном Compose и уже созданном защищённом каталоге:

```sh
docker compose exec -T postgres pg_dump -U registration -d registration -Fc > /secure/backups/registration.dump
```

Проверка восстановления: создать **отдельный** PostgreSQL, выполнить pg_restore
с `--exit-on-error` в пустую БД, запустить проверку схемы и `validate-data`, сверить
количество users/permissions/chats и статусы outbox. Frontend и backend при
восстановлении должны быть остановлены; использовать отдельные CLI-команды,
никакого live Telegram. Проверку проводить регулярно и фиксировать дату.

В архиве SQLite есть персональная переписка; не класть её в репозиторий, образ
или CI artifacts. Аналогично не публиковать XLSX, PostgreSQL dumps и Kafka UI
на внешний интерфейс.

## Обновление контрактов

### Быстрый штатный перезапуск frontend

Для `ReleaseSender` сначала обновить backend через `make up`, затем frontend через
его `make up`. Новая миграция не нужна. Старый frontend при первой замене ещё не
освобождает lease, поэтому первый запуск новой версии может ждать до 90 секунд.
При следующих штатных остановках новый frontend завершает sender и Complete,
затем освобождает свой lease отдельным RPC (таймаут 5 секунд).
В frontend-логах `sender lease cleanup completed` с `released=true` подтверждает
освобождение; `sender lease acquired` — захват новым процессом. При SIGKILL,
недоступном backend или старом backend без RPC действует прежний TTL.

### Порядок обновления proto

Канонический proto — `api/registration.proto` backend. Пока нет опубликованного
Go-модуля, frontend хранит versioned snapshot того же proto и генерирует
Go с переопределённым go_package при сборке. `*.pb.go` игнорируются Git.
Сборка не читает соседний репозиторий.
Совместимые изменения контракта доставлять согласованно: сначала backend, затем
frontend. Для migration 004 обязателен остановленный coordinated rollout выше,
поскольку старый frontend не получает direct replies. Поля protobuf не
переиспользовать; при удалении резервировать номера.
`make proto-gen` использует закреплённые версии генераторов; Make-цели сборки,
проверок и запуска вызывают его автоматически, Docker генерирует код в build-stage.
