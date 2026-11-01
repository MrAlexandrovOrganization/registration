# Перенос registration на Finland, 2 ноября 2026

Backend, Telegram frontend и PostgreSQL размещаются вместе на Finland. Это стабильная пара main, без локальных незавершённых HA изменений. Приоритет: registration, затем VPN, затем notes. Russia не получает новые сервисы. Не использовать server/private-test-server как целевые хосты.

COMPOSE_FILE=compose/docker-compose.yml:compose/compose.finland.yml; REGISTRATION_KAFKA_HOST=100.64.0.3, REGISTRATION_OTLP_HOST=100.64.0.3. Kafka нужна для broadcast; прямые интерактивные ответы и durable delivery хранятся в PostgreSQL. Отключение WSL задержит рассылки. Данные, outbox, dedup, leases, роли и sequences переносить полностью, с остановленными writers. До включения frontend сверять fingerprint всех таблиц, sequence values и ролей. Не создавать новые topics, не импортировать legacy SQLite, не сбрасывать sent/dedup/leases.

Telegram Bot API — отдельный registration-bot-api на Finland, старый токен не запускать на двух экземплярах. Сначала остановить source frontend, сохранить pending updates, close старой локальной Bot API сессии, перенести только её каталог. Затем getMe/getWebhookInfo и запуск frontend. Другие боты/сессии source API не изменяются. Финский API использует прямой Telegram egress, не меняет маршруты VPN.

Исходные writers сохраняются остановленными с restart=no и .retired. После новых записей откат требует актуальной копии БД и закрытой Bot API сессии; старый snapshot не является актуальным rollback. CI/CD обоих приложений направлен на Finland; старые volumes не удалять.
