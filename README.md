# imapsync — двусторонний синхронизатор папок IMAP

CLI-демон на Go для **двусторонней** синхронизации почтовых папок между двумя
IMAP-серверами. Основной сценарий — синхронизация папки Sent между двумя
инсталляциями почты, где один и тот же пользователь имеет ящики на обоих
серверах.

Синхронизация **только дописывающая** (append): на каждую сторону копируются
письма, которых на ней нет. Флаги, прочтения и удаления **не** синхронизируются.

## Возможности

- **Мастер-доступ.** Одна сервисная учётка на каждом сервере имперсонирует
  целевых пользователей через SASL PLAIN с authzid
  (`authcid` = мастер, `authzid` = целевой ящик, пароль мастера).
- **Worker-pool.** Число воркеров меньше числа юзеров; воркеры разбирают юзеров
  из очереди, один юзер обрабатывается одним воркером целиком.
- **Надёжная дедупликация** с учётом особенностей Exchange (Message-ID может
  отсутствовать или появляться позже) — см. ниже.
- **Источник конфигурации** — YAML или локальная БД SQLite (для больших списков).
- **Логирование** — ошибки с контекстом (юзер/папка/сервер) по мере
  возникновения, периодическая сводная статистика и итог по каждому юзеру.

## Сборка

```sh
go build -o imapsync ./cmd/imapsync
```

Требуется Go 1.25+. Зависимости: `emersion/go-imap` v1, `emersion/go-message`,
`emersion/go-sasl`, `gopkg.in/yaml.v3`, `modernc.org/sqlite` (чистый Go, без CGO).

## Запуск

```sh
imapsync run -config config.yaml
```

Демон крутит полные циклы синхронизации с паузой `sync_interval` между ними,
завершается по SIGINT/SIGTERM (текущие юзеры доводятся до контрольной точки).

## Конфигурация

Пример — `config.example.yaml`. Минимум:

```yaml
server_a:
  host: mail-a.corp.ru
  port: 993
  master_user: svc_sync
  master_pass: "SECRET_A"
server_b:
  host: mail-b.corp.ru
  port: 993
  master_user: svc_sync
  master_pass: "SECRET_B"

folders:
  - a: "Sent"            # имя папки на сервере A
    b: "Отправленные"    # имя папки на сервере B

users:
  - name: ivanov
    user_a: ivanov@corp.ru
    user_b: ivanov@corp.ru

workers: 2               # МЕНЬШЕ числа юзеров
sync_interval: 5m
stats_interval: 1m
per_user_timeout: 10m
dial_timeout: 30s
fetch_batch_size: 200
insecure_tls: false
hash_header: "X-Imapsync-Hash"
```

Параметры серверов, тайминги и `workers` всегда берутся из YAML. Списки папок и
юзеров могут храниться в SQLite.

### Конфигурация из SQLite

```yaml
source: sqlite
sqlite_path: /var/lib/imapsync/imapsync.db
# секции folders/users в YAML при этом необязательны и игнорируются
```

В БД читаются только **включённые** юзеры. Наполнение БД — отдельными командами:

```sh
# по одному, через параметры
imapsync db-add-folder -db imapsync.db -a Sent -b "Отправленные"
imapsync db-add-user   -db imapsync.db -name ivanov -a ivanov@corp.ru -b ivanov@corp.ru [-disabled]

# перенести всё из YAML
imapsync db-import-yaml -db imapsync.db -config config.yaml

# импорт из CSV
imapsync db-import-csv  -db imapsync.db -users users.csv -folders folders.csv

# посмотреть содержимое
imapsync db-list -db imapsync.db
```

Форматы CSV (строки с `#` и строка-заголовок пропускаются):

```
# users.csv
name,user_a,user_b,enabled
ivanov,ivanov@corp.ru,ivanov@corp.ru,1

# folders.csv
folder_a,folder_b
Sent,Отправленные
```

## Логика дедупликации

Письмо в папке A считается уже присутствующим в папке B, если совпадает
**хотя бы один** из его ключей сопоставления:

1. `Message-ID` (нормализованный) — если есть;
2. значение заголовка `X-Imapsync-Hash` — если письмо помечалось нами при
   прошлом копировании;
3. **суррогатный хеш** — `sha256(Date_UTC_unix + Subject + From)` — вычисляется
   всегда.

При копировании письма в целевую папку в него вставляется заголовок
`X-Imapsync-Hash` с суррогатным хешем. За счёт того, что суррогатный хеш
стабилен, позднее появление `Message-ID` на одной стороне (частый случай на
Exchange) **не приводит к повторному копированию**: копия на другой стороне
по-прежнему находится по `X-Imapsync-Hash`.

Оригинальные `INTERNALDATE` и флаги (`\Seen`, `\Answered`, `\Flagged`,
`\Draft`) при копировании сохраняются. Локальная БД состояния не нужна —
источник истины сами папки плюс заголовок `X-Imapsync-Hash`.

## Архитектура

```
cmd/imapsync/        точка входа: диспетчер подкоманд, демон, сигналы
config/              YAML-конфиг: загрузка, валидация, дефолты
internal/
  mailbox/           обёртка над go-imap: connect+TLS, master-login,
                     list/select/fetch/append; разбор полей письма, хеши
  dedup/             мультиключевой индекс папки, вычисление дельты
  stats/             потокобезопасные счётчики, периодический вывод
  syncer/            синк одного юзера + worker-pool
  store/             локальная БД SQLite (источник конфигурации)
```

## Тесты

```sh
go test -race ./...
```

Интеграционные тесты синкера и пула поднимают in-memory IMAP-серверы поверх
самоподписанного TLS и проверяют схождение обеих сторон и идемпотентность
повторных проходов.
