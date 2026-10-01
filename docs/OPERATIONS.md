# Эксплуатация

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
настроить Prometheus scrape target `registration-backend:9090`, path `/metrics`:
этот alias задан в Compose на `prometheus-net`. Для frontend использовать его
сетевое имя в `prometheus-net` и порт 9091. Наличие сети не добавляет scrape target автоматически.
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

Канонический proto — `api/registration.proto` backend. Пока нет опубликованного
Go-модуля, frontend хранит versioned snapshot того же proto и генерирует
Go с переопределённым go_package при сборке. `*.pb.go` игнорируются Git.
Сборка не читает соседний репозиторий.
Изменения контракта доставлять согласованно: сначала совместимый backend, затем
frontend. Поля protobuf не переиспользовать; при удалении резервировать номера.
`make proto-gen` использует закреплённые версии генераторов; Make-цели сборки,
проверок и запуска вызывают его автоматически, Docker генерирует код в build-stage.
