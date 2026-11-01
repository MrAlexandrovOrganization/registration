# Автоматические внешние бэкапы registration

Рабочая PostgreSQL с 2 ноября 2026 работает на `hostkey-finland`. На `asus-server` установлен pull backup:
он работает через Headscale независимо от административного Mac и не запускает
backend/frontend или старую HA-конфигурацию. Первичная настройка — 22 октября 2026.

## Расписание и хранение

- `registration-backup.timer`: каждый час в `:05` по Москве с задержкой до 120 секунд.
- `Persistent=true`: после выключения ASUS выполняется один пропущенный запуск.
- При ошибке — до трёх попыток за час с интервалом пять минут.
- Каталог ASUS: `/home/maxim/backups/registration`, права 0700; файлы 0600.
- Все копии за последние 48 часов, затем последние копии 7 UTC-дней,
  4 ISO-недель и 12 месяцев, в которых были бэкапы. Категории перекрываются.
- Ротация удаляет только файлы собственного формата и только после успешного
  получения и проверки новой копии. При обрыве старые копии сохраняются.
- `registration-backup-verify.timer`: первого числа месяца в 04:35 по Москве
  с задержкой до 120 секунд, с наверстыванием пропущенного запуска.

Архив `registration-<UTC>-<uuid>.tar` содержит:

| Файл | Содержимое |
| --- | --- |
| `database.dump` | Консистентный `pg_dump -Fc` базы registration |
| `roles.sql` | Роли PostgreSQL без хэшей паролей |
| `backend.env`, `frontend.env` | Секретная конфигурация двух приложений |
| `manifest.json` | SHA-256 вложений, ревизии приложений, digest/ID и fingerprint PostgreSQL |

На источнике проверяется `pg_restore --list`; на ASUS проверяются состав архива
и SHA-256 всех вложений. Итоговое имя публикуется атомарно. Статус последнего
успеха, размер и SHA-256 всего архива — в `latest.json`.

Передача зашифрована SSH; сами архивы не зашифрованы и доступны владельцу/root ASUS.
Не публиковать архивы, `.env`, ключи и содержимое конфигурации в Git, CI или логах.
Это внешняя копия относительно VM; отдельной копии за пределами ASUS здесь нет.
Исходная legacy SQLite и Kafka в эти архивы не входят. Данные регистрации и
доставки хранятся в PostgreSQL; ограничения восстановления очереди — в OPERATIONS.

## Доступ и установленная конфигурация

Канонический код: `scripts/registration_backup.py`, units: `ops/backup/`.
На обоих хостах установлена копия кода в
`/home/maxim/.local/lib/registration-backup/registration_backup.py`.
Приложения и их CI/CD для этой настройки не меняются; systemd provisioned отдельно.
Текущая доставка приложений не обновляет эту административную установку автоматически.
При изменении backup-кода обновить установленные копии на обоих хостах явно.

На ASUS в `/home/maxim/.config/registration-backup/` (0700):

- `config.json` (0600): `directory`, `source=maxim@100.64.0.6`, пути `identity` и `known_hosts`;
- `id_ed25519` (0600): отдельный приватный ключ, никогда не покидает ASUS;
- `known_hosts`: проверенный ED25519 host key Finland, обязательная строгая проверка.

На Finland в `~/.ssh/authorized_keys` добавлен отдельный публичный ключ
`registration-backup-asus`: `from="100.64.0.11",restrict,command="/usr/bin/python3
/home/maxim/.local/lib/registration-backup/registration_backup.py source"`
(в реальном файле это одна строка). Он разрешает только фиксированную выгрузку,
не shell/PTY/forwarding. Код находит единственный PostgreSQL по Compose labels
`project=registration`, `service=postgres`, не по отображаемому имени контейнера.
База и приложения при копировании не останавливаются.

Prerequisites: Python 3.12+ со стандартной библиотекой, OpenSSH, systemd на ASUS;
Docker на обоих хостах. Восстановление использует digest PostgreSQL из архива,
а не второй редактируемый pin. При отсутствии образа в cache
`docker pull` по digest требует доступности Docker Hub. Уже загруженный образ
используется без сети; при первом provisioning его можно передать через
`docker image save/load` с server. Docker stores могут показать разные image IDs
после импорта; imported cache маркируется локальным тегом
`registration-backup-postgres:<исходный ID без sha256:>`. Перед использованием
обязательно сравнивается fingerprint архитектуры, ОС, image config и хэшей слоёв
с manifest. Совпадения имени/тега недостаточно.
Скачивание образа и SQL-восстановление не удерживают блокировку почасовых бэкапов.

При повторном provisioning сначала сверить установленные файлы и сохранить
местные правки. Установить код в приватный каталог, units через `sudo install -m644`
в `/etc/systemd/system/`, проверить `systemd-analyze verify`, выполнить
`systemctl daemon-reload`. До включения timers выполнить пробные backup/verify.
Ключи создавать только на ASUS, не переиспользовать пользовательские ключи.
Проверенный host key передавать через уже аутентифицированный канал server.

## Проверка и ручной запуск

На ASUS:

```sh
sudo systemctl start registration-backup.service
sudo systemctl start registration-backup-verify.service
systemctl list-timers 'registration-backup*' --all
systemctl show registration-backup.service -p Result -p ExecMainStatus
journalctl -u registration-backup.service -u registration-backup-verify.service
cat /home/maxim/backups/registration/latest.json
cat /home/maxim/backups/registration/verified.json
```

Проверять свежесть `latest.json`: обычный интервал около часа, при offline нового
бэкапа не будет. Логи и ненулевой статус сохраняются в journal/systemd;
отдельная внешняя система уведомлений о просроченном бэкапе не настроена.

Полная проверка создаёт PostgreSQL без сети, портов и host mounts, с отдельным
tmpfs 256 MiB, лимитами RAM 384 MiB и 1 CPU. Ни приложение, ни рабочие credentials
не запускаются. `pg_restore --exit-on-error --single-transaction` восстанавливает
схему, данные и ограничения; затем проверяются таблицы и история миграций.
`verified.json` содержит дату, имя проверенного архива и количества строк.
Это проверка SQL-восстановления; business `validate-data`, отправка Telegram,
восстановление паролей ролей и активация приложений в неё не входят.
При росте БД заранее пересмотреть tmpfs/RAM проверки. После штатного завершения
контейнер удаляется; после аварийного kill проверить оставшиеся контейнеры
по label `service=registration-backup-verify` и удалить только их.

Локальная проверка автоматизации: `make backup-test`, Ruff для scripts;
`make check` включает backup-test. Тесты используют синтетические данные,
проверяют ротацию, повреждение архива и незавершённую передачу без SSH/production.

Подтверждено 22 октября 2026 в 19:48 МСК: полное SQL-восстановление архива
`registration-20261022T164844Z-fd4fccfe834948bb81c2a11d34128451.tar` прошло успешно.
Восстановлены 767 users, 4 user_permissions, 4 bot_chats, 5 schema_migrations
и 4564 outbound_messages. Проверочный контейнер удалён; оба systemd service
завершились с `Result=success`, timers включены. Почасовые автоматические запуски
до проверки также подтверждены журналом. Локально прошли 6 синтетических тестов
backup и Ruff; полный backend `make check` не запускался, Docker daemon на Mac
недоступен. Проверки общего репозитория `make check` прошли (8 тестов).

## Восстановление рабочего сервиса

Сначала остановить frontend/senders и старый backend. Распаковать выбранный архив
в приватный каталог после проверки состава/SHA-256. Развернуть PostgreSQL версии
из manifest в **новом** хранилище, восстановить нужные роли и пароли из приватной
конфигурации, затем `database.dump` с `--exit-on-error`. Дамп ролей не применять
вслепую к существующему кластеру: роль registration уже может быть создана.
Восстановить конфигурацию с 0600, проверить schema/`validate-data`, пользователей,
permissions/chats и outbox. Не заменять существующий рабочий volume до проверки.

После отката БД Telegram не откатывается: возможны повторы отправок. Сверить
delivery statuses/offset и только затем включать приложения. Согласованный
порядок — [OPERATIONS.md](OPERATIONS.md) и [MIGRATION.md](MIGRATION.md).

Для остановки расписания, сохраняя архивы:

```sh
sudo systemctl disable --now registration-backup.timer registration-backup-verify.timer
```

Для полного отзыва доступа удалить только строку ключа `registration-backup-asus`
из authorized_keys server. Остальные SSH-ключи, приложения и архивы сохранять.

При миграции сохраняются прежние расписания и retention. Экспорт разделяет shared lock с exclusive CD lock и отказывает retired source. Проверка SQL restore выполняется до завершения переключения. Frontend .env содержит также TELEGRAM_API_ID/HASH для восстановления выделенного локального Bot API; state-сессия отдельно сохраняется при миграции.
