# Локальный baseline производительности

Запуск: `make benchmark`. Цель генерирует protobuf и запускает
`scripts/benchmark.py`: отдельный PostgreSQL с уникальным именем, tmpfs, loopback
портом, синтетическим паролем и БД `registration_benchmark`. Контейнер удаляется
в finally. Рабочие `.env`, сети и volumes не используются. Нужен Docker.

Измеряется реальный gRPC Accept `/start` для **нового пользователя на каждый
запрос**, с PostgreSQL commit и двумя durable reply jobs. Есть auth, метрики и
лимит 32 concurrent streams; pool — 10 соединений, как в приложении. Ошибки,
duplicate или отсутствие двух delivery IDs делают benchmark неуспешным.
Клиент и gRPC-сервер работают на хосте с GOMAXPROCS=1, PostgreSQL в контейнере
с лимитами 1 CPU / 512 MiB. Измерение closed-loop с 1/8/32 клиентами, 3 секунды
на финальную выборку; p95/p99 включают ожидание RPC на стороне клиента.

В этой проверке нет Telegram, Kafka, sender, экспорта OTel и записи логов на диск.
Outbox накапливается, не отправляется. PostgreSQL tmpfs не моделирует fsync на
production-диске. Это короткий воспроизводимый baseline, **не максимальный
устойчивый RPS сервиса**: для SLO нужны длительная нагрузка, production-подобные
лимиты/диск/объём базы, смесь запросов, open-loop arrival rate и проверка backlog.

## Результат 2026-10-08

Apple M3 Pro, darwin/arm64, PostgreSQL в Colima, GOMAXPROCS=1:

| Клиентов | Accept/с | p95, мс | p99, мс | Ошибок |
|---:|---:|---:|---:|---:|
| 1 | 215 | 5,14 | 7,43 | 0 |
| 8 | 1360 | 8,20 | 15,39 | 0 |
| 32 | 1290 | 47,07 | 55,05 | 0 |

Увеличение конкурентности до 32 здесь увеличило задержку без роста throughput.
Один frontend сейчас вызывает Accept последовательно, поэтому показатель для
8 клиентов не является скоростью одного работающего бота. Доставка сообщений
ограничена отдельным sender/limiter; см. frontend `docs/PERFORMANCE.md`.
