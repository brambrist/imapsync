# IMAP Sync — двусторонний синхронизатор ящиков

## Что это

CLI-демон на Go для **двусторонней** синхронизации почтовых папок между двумя
IMAP-серверами. Основной сценарий — синк папки Sent (Отправленные) между двумя
инсталляциями почты, где один и тот же пользователь имеет ящики на обоих серверах.

Пишем инкрементально через Claude Code, компилируем и тестируем по ходу.
Стиль: инкрементальные правки, а не переписывание целиком. Дефисы, не длинные тире.

## Требования (согласованы)

- **Язык:** Go
- **IMAP-библиотека:** `github.com/emersion/go-imap` **v1** (стабильная ветка v1.2.x,
  НЕ v2). Для разбора писем — `github.com/emersion/go-message`.
- **Направление синка:** двусторонний, но **только копирование недостающих писем**
  (append). Флаги (прочитано/удалено) и удаления НЕ синхронизируем — только
  дописываем на каждую сторону письма, которых на ней нет.
- **Многопоточность:** worker-pool. **Число воркеров МЕНЬШЕ числа пользователей**
  (потоков меньше, чем юзеров) — воркеры разбирают юзеров из очереди. Один юзер
  обрабатывается одним воркером целиком (не дробим юзера между потоками).
- **Многопользовательность:** **мастер-доступ** — одна сервисная УЗ (master user)
  на каждом сервере имперсонирует целевых пользователей. Логин имперсонации
  задаётся шаблоном (по умолчанию Dovecot-стиль `{user}*{master}`, пароль —
  мастера).
- **Логирование:**
  - ошибки — по мере возникновения, с контекстом (юзер, папка, сервер);
  - периодическая **сводная статистика по всем юзерам** (раз в StatsInterval);
  - статистика по **текущему обрабатываемому** юзеру (сколько скопировано в
    каждую сторону, сколько пропущено как дубли, ошибки).

## Ключевая логика дедупликации (важно, особенность Exchange)

Письма нельзя сравнивать только по `Message-ID`, потому что:
1. `Message-ID` может **отсутствовать**;
2. на Exchange `Message-ID` может **появиться позже** (не сразу после появления
   письма в папке) — то есть на одной стороне он уже есть, на другой того же
   письма ещё нет или без ID.

Алгоритм идентичности письма:
1. Если у письма есть `Message-ID` — сравниваем по нему (нормализованному).
2. Если `Message-ID` нет — вычисляем **суррогатный ключ**: хеш от значимых полей.
   Предложенный состав: **дата отправки (Date) + Subject** (можно расширить From/To
   при коллизиях). Хеш пишем в **отдельный кастомный заголовок** на "той" стороне
   при append (например `X-Imapsync-Hash: <hex>`), чтобы при следующих проходах
   находить уже скопированное письмо по этому заголовку, а не пересчитывать.
3. Итоговый индекс письма для сравнения: `Message-ID` (если есть) ИЛИ значение
   `X-Imapsync-Hash` (если проставляли) ИЛИ вычисленный суррогатный хеш.

Это защищает от:
- дублей при отсутствии Message-ID;
- повторного копирования, когда Message-ID появился позже (суррогатный хеш
  остаётся стабильным и уже записан в заголовок на целевой стороне).

**Нормализация полей для хеша:** тримить пробелы, привести Subject к единому виду
(убрать возможные `Re:/Fwd:` префиксы? — обсудить, по умолчанию НЕ трогаем, берём
как есть), Date приводить к UTC unix-времени. Точный состав полей и нормализацию
финализируем при реализации dedup-модуля.

## Архитектура (предложенная структура пакетов)

```
imapsync/
  CLAUDE.md
  go.mod
  cmd/
    imapsync/main.go        — точка входа: загрузка конфига, запуск пула, сигналы
  config/
    config.go               — YAML-конфиг: серверы, юзеры, папки, worker/тайминги
  internal/
    endpoint/
      endpoint.go            — интерфейсы Backend/Endpoint (абстракция «конец
                              синхронизации»); синкер знает только о них
      imap.go                — IMAP-реализация поверх mailbox
      maildir.go             — Maildir/Maildir++ на диске (type: maildir, root)
    mailbox/
      client.go             — IMAP-примитивы поверх go-imap v1: connect+TLS,
                              master-login (имперсонация), resolve/select/fetch/append
      message.go            — разбор письма: извлечение Message-ID, Date, Subject,
                              вычисление суррогатного хеша, чтение/запись
                              X-Imapsync-Hash
    dedup/
      index.go              — построение индекса писем папки (ключ->наличие),
                              сравнение двух папок, вычисление "чего не хватает
                              на каждой стороне"
    syncer/
      syncer.go             — логика синка одного юзера: подключиться к A и B,
                              для каждой папки построить индексы, вычислить дельту,
                              append недостающих в обе стороны, собрать статистику
      pool.go               — worker-pool: очередь юзеров, N воркеров (N<юзеров),
                              per-user timeout, сбор статистики, graceful shutdown
    stats/
      stats.go              — счётчики per-user и агрегат, потокобезопасно
                              (atomic/mutex), периодический вывод сводки и
                              текущего юзера
    store/
      store.go              — локальная БД SQLite (modernc.org/sqlite, без CGO):
                              пары папок и пользователи. Альтернативный источник
                              конфигурации при source: sqlite, чтобы не держать
                              большие списки в YAML.
      state.go              — кэш инкрементальной сверки (sync_endpoint,
                              sync_msg_cache) и статус синка по юзерам
                              (user_status). Включается флагом state_cache.
```

## CLI

```
imapsync run -config cfg.yaml               запуск демона (подкоманда по умолчанию)
imapsync db-add-user   -db x.db -name ivanov -a ivanov@a -b ivanov@b [-disabled]
imapsync db-add-folder -db x.db -a Sent -b "Отправленные"
imapsync db-import-yaml -db x.db -config cfg.yaml     перенести списки из YAML в БД
imapsync db-import-csv  -db x.db [-users u.csv] [-folders f.csv]
imapsync db-list       -db x.db
```

CSV: `users` - `name,user_a,user_b[,enabled]`; `folders` - `folder_a,folder_b`.
Строки с '#' и строка-заголовок пропускаются.

## Конфиг (YAML) — черновой вид

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

# Имперсонация: SASL PLAIN с authzid (authcid = master_user, authzid = user_a/user_b).
# Формат Dovecot "{user}*{master}" НЕ используем.

source: yaml            # yaml (по умолчанию) | sqlite
# sqlite_path: /var/lib/imapsync/imapsync.db   # при source: sqlite или state_cache
# state_cache: false     # инкрементальная сверка (UID SEARCH + кэш) + user_status

# Явные пары папок (открытый вопрос 1 решён - явный маппинг). При source: sqlite
# секции folders/users необязательны, читаются из БД.
folders:
  - a: "Sent"
    b: "Отправленные"

users:
  - name: ivanov
    user_a: ivanov@corp.ru
    user_b: ivanov@corp.ru
  - name: petrov
    user_a: petrov@corp.ru
    user_b: petrov@corp.ru

workers: 4                 # МЕНЬШЕ числа юзеров
sync_interval: 5m          # пауза между полными циклами
stats_interval: 1m         # периодичность сводной статистики
per_user_timeout: 10m
dial_timeout: 30s
io_timeout: 5m             # таймаут на одну IMAP-операцию
connect_retries: 3         # повторы подключения (0 = дефолт 3)
retry_backoff: 5s
full_resync_every: 24h     # при state_cache: полный пере-скан папки раз в N
max_fail_streak: 10        # стоп-синк юзера после N ошибок подряд (нужна БД; <0 = выкл)
fetch_batch_size: 200
insecure_tls: false
```

CLI управления БД: `db-remove-user`/`db-remove-folder`, `db-forget-user` (сброс
кэша+статуса+истории), `db-resume-user` (снять стоп-синк), `db-history`,
`db-vacuum`. История прогонов - `user_run` (ретенция 200/юзера).
При `source: sqlite` демон перечитывает списки из БД перед каждым циклом
(`Pool.reload`) - `db-*` подхватываются без рестарта.

Имена в `folders` резолвятся через `mailbox.ResolveFolder`: точное имя →
SPECIAL-USE токен (`\Sent`) → регистронезависимо. Демон при открытой БД берёт
`flock` на `<sqlite_path>.lock`.

## Открытые вопросы (решить при реализации)

1. **[РЕШЕНО] Сопоставление имён папок A↔B.** Явный список пар
   `folders: [{a: "Sent", b: "Отправленные"}]`, глобально для всех юзеров.
2. **[РЕШЕНО] Состав суррогатного хеша.** `Date (UTC unix) + Subject (trim, как
   есть) + From (нормализованный адрес)`. Re:/Fwd: не трогаем. Письма без Date -
   unix=0.
3. **[РЕШЕНО] Появление Message-ID позже (Exchange).** Отдельный проход не нужен.
   `dedup.mailbox.MatchKeys` отдаёт для письма ВСЕ ключи сразу (mid +
   записанный X-Imapsync-Hash + всегда вычисленный суррогат); письма идентичны
   при совпадении любой пары ключей. Суррогат стабилен, поэтому позднее
   появление Message-ID не приводит к задвоению.
4. **[РЕШЕНО] APPEND и внутренняя дата/флаги.** Сохраняем оригинальный
   INTERNALDATE; из флагов переносим только `\Seen \Answered \Flagged \Draft`
   (`\Recent` нельзя, `\Deleted` не синхронизируем). APPEND через
   `AppendGetUID` - читаем APPENDUID (UIDPLUS), если сервер отдаёт.
5. **[РЕШЕНО] Идемпотентность и рестарты.** По умолчанию индекс строится каждый
   цикл заново из папок — БД состояния НЕ нужна, источник истины папки +
   X-Imapsync-Hash. Опционально `state_cache: true` включает инкрементальную
   сверку (вариант 3): `UID SEARCH` даёт список UID, фетчатся только новые,
   разбор кэшируется в sqlite; при смене UIDVALIDITY кэш эндпоинта сбрасывается.
   Кэш - только ускорение, не источник истины (сброс = полный пере-фетч).
   Заодно при открытой БД пишется `user_status` (когда/с каким результатом
   отработал синк по юзеру).
6. **[РЕШЕНО] Механизм имперсонации.** SASL PLAIN с authzid на ОБОИХ серверах:
   `sasl.NewPlainClient(targetUser, master_user, master_pass)` (identity=authzid=
   целевой юзер). Реализовано в `mailbox.Connect`.
7. **Лимиты и throttling.** Мастер-УЗ ходит по многим ящикам — учесть возможные
   лимиты одновременных соединений на сервере (см. прошлый опыт с сессиями).
   Ограничить число одновременных соединений = числу воркеров (уже так).

## Порядок реализации

Все 8 шагов реализованы (+ SQLite-конфиг, + инкрементальная сверка `state_cache`
и `user_status`), каждый модуль с тестами, `go build ./...` / `go vet ./...` /
`go test -race ./...` зелёные.

1. [x] `config/config.go` — конфиг, загрузка, валидация, дефолты; тип `Duration`
   для YAML-строк ("5m"); `source: yaml|sqlite`; `validateBase` + публичный
   `ValidateEntities`.
2. [x] `internal/mailbox/client.go` — Connect (TLS + SASL PLAIN authzid),
   FindFolder/Select/FetchHeaders/FetchFull/Append.
3. [x] `internal/mailbox/message.go` — ParseFields, NormalizeMessageID/Addr,
   SurrogateHash, Identity, MatchKeys (все ключи письма), InjectHashHeader.
4. [x] `internal/dedup/index.go` — Build (мультиключевой индекс), Missing/Delta,
   счётчик внутренних дублей.
5. [x] `internal/stats/stats.go` — Collector, per-user atomic-счётчики,
   Snapshot/LogSummary/LogUser, StartReporter.
6. [x] `internal/syncer/syncer.go` — SyncUser: обе папки, обе стороны, append
   недостающих, INTERNALDATE + фильтр флагов, ошибки не фатальны.
7. [x] `internal/syncer/pool.go` — Pool.Run/RunCycle, worker-pool через очередь,
   per_user_timeout, graceful shutdown по ctx.
8. [x] `cmd/imapsync/` — main.go (диспетч подкоманд), run.go, daemon.go
   (SIGINT/SIGTERM → context, цикл sync_interval), db.go (подкоманды db-*).

## Зависимости

```
github.com/emersion/go-imap v1.2.x      // IMAP клиент (v1!)
github.com/emersion/go-message          // разбор MIME/заголовков
github.com/emersion/go-sasl             // SASL PLAIN с authzid (имперсонация)
gopkg.in/yaml.v3                         // конфиг
modernc.org/sqlite                      // локальная БД конфигурации (без CGO)
```

## Соглашения

- Комментарии и логи на русском, код/идентификаторы на английском.
- Дефисы, не длинные тире.
- Инкрементальные правки. Компилировать после каждого модуля (`go build ./...`).
- Ошибки оборачивать `fmt.Errorf("...: %w", err)`, логировать с контекстом
  (юзер/папка/сервер).
- Никаких секретов в коде — только через конфиг/переменные окружения.
