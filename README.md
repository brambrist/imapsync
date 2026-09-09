# imapsync — two-way mail folder synchronizer

A Go CLI daemon for **two-way** synchronization of mail folders between two
endpoints. The main scenario is syncing the Sent folder between two mail
installations where the same user has mailboxes on both.

Synchronization is **append only**: each side gets the messages it lacks. Flags,
read state and deletions are **not** synced.

## Features

- **Master access.** One service account per server impersonates the target
  users. IMAP impersonation is SASL PLAIN with authzid
  (`authcid` = master, `authzid` = target mailbox, master's password).
- **Worker pool.** Fewer workers than users; workers pull users from a queue,
  one user is processed by one worker end to end.
- **Multiple endpoint types.** Each side is `imap` (default), `maildir` or `ews`,
  independently - so IMAP<->Maildir, Exchange<->Dovecot etc. all work.
- **Robust deduplication** that accounts for Exchange quirks (Message-ID may be
  absent or appear later) - see below.
- **Config source** - YAML or a local SQLite DB (for large lists).
- **Logging** - errors with context (user/folder/server) as they happen, a
  periodic summary and a per-user result.

## Quick start

1. **Build the binary** (needs Go 1.25+):

   ```sh
   git clone git@github.com:brambrist/imapsync.git
   cd imapsync
   go build -o imapsync ./cmd/imapsync
   ```

2. **Make a config** from the example and fill in your servers, master
   accounts, folder pairs and users:

   ```sh
   cp config.example.yaml config.yaml
   $EDITOR config.yaml
   ```

   Minimum: `server_a`/`server_b` (host + master_user + master_pass), one
   `folders` pair, at least one `users` entry, and `workers` **fewer** than the
   number of users.

3. **First run on a couple of test mailboxes.** Pick 1-2 unimportant users, set
   short intervals and watch the log:

   ```sh
   ./imapsync run -config config.yaml
   ```

   The log shows: cycle start, per-user errors with context
   (user/folder/server), a summary every `stats_interval` and a per-user result
   (how many copied each way, how many skipped as duplicates). Stop with Ctrl+C
   (users in progress are finished to a checkpoint).

4. **Check the result.** Run the cycle twice: the second pass over the same
   mailboxes should show `A->B=0 B->A=0`, meaning deduplication works and
   messages are not duplicated.

5. **Production run.** Restore normal intervals (`sync_interval: 5m` etc.), add
   all users, run under a supervisor (systemd/`nohup`). The daemon runs forever,
   pauses between cycles on its own and shuts down cleanly on SIGTERM.

**If there are many mailboxes** - keep the folder and user lists in SQLite
rather than YAML: see [Config from SQLite](#config-from-sqlite).

### Developer onboarding

```sh
go test -race ./...      # all tests, including integration ones (in-memory IMAP+TLS, fake EWS)
go vet ./...
gofmt -l .               # must be empty
```

Entry point - `cmd/imapsync` (subcommand dispatch). Sync logic - in
`internal/syncer`; deduplication - `internal/dedup` + `internal/mailbox/message.go`;
transports - `internal/endpoint`. Style: incremental edits, compile after each
module, comments and code in English.

## Build

```sh
go build -o imapsync ./cmd/imapsync
```

Needs Go 1.25+. Dependencies: `emersion/go-imap` v1, `emersion/go-message`,
`emersion/go-sasl`, `gopkg.in/yaml.v3`, `modernc.org/sqlite` (pure Go, no CGO).

## Run

```sh
imapsync run -config config.yaml
```

The daemon runs full sync cycles with a `sync_interval` pause between them, and
shuts down on SIGINT/SIGTERM (users in progress are finished to a checkpoint).

Example systemd unit (`/etc/systemd/system/imapsync.service`):

```ini
[Unit]
Description=IMAP two-way folder sync
After=network-online.target

[Service]
ExecStart=/opt/imapsync/imapsync run -config /opt/imapsync/config.yaml
Restart=on-failure
RestartSec=30
User=imapsync
# the config holds secrets - restrict it: chmod 600

[Install]
WantedBy=multi-user.target
```

## Configuration

See `config.example.yaml`. Minimum:

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
  - a: "Sent"            # folder name on server A
    b: "Sent Items"      # folder name on server B

users:
  - name: ivanov
    user_a: ivanov@corp.ru
    user_b: ivanov@corp.ru

workers: 2               # FEWER than the number of users
sync_interval: 5m
stats_interval: 1m
per_user_timeout: 10m
dial_timeout: 30s
fetch_batch_size: 200
insecure_tls: false
hash_header: "X-Imapsync-Hash"
```

Server fields, timings and `workers` always come from YAML. The folder and user
lists may live in SQLite.

**Endpoint type** (`type`) is set per side independently - `imap` (default),
`maildir` or `ews`. Mixed pairs work (Dovecot -> new IMAP migration, Exchange ->
Dovecot, etc.):

- `maildir` - `root`: path template to the Maildir (`%u` - whole
  `user_a`/`user_b`, `%n` - before `@`, `%d` - domain).
- `ews` - `ews_url` (or `host` -> `https://<host>/EWS/Exchange.asmx`),
  `master_user`/`master_pass` is a service account with the
  `ApplicationImpersonation` role; impersonation via the `ExchangeImpersonation`
  SOAP header. Auth: **Basic only** (O365 needs OAuth2 - see `TECHDEBT.md`).
  `\Seen` is carried, `INTERNALDATE` is not.

```yaml
server_a:
  type: maildir
  root: /var/vmail/%d/%n/Maildir
server_b:
  type: ews
  ews_url: https://exch.corp.ru/EWS/Exchange.asmx
  master_user: svc_sync
  master_pass: "SECRET"
```

Maildir: message ID = the unique part of the filename (stable across flag
changes and `new`<->`cur`); flags `\Seen \Answered \Flagged \Draft` <-> letters
`S R F D` in the `:2,` suffix; `INTERNALDATE` = file mtime; subfolders (`.Sent`
etc.) are created automatically.

EWS: ID = `ItemId`; metadata from `InternetMessageHeaders` + `IsRead`; body -
`GetItem` with `IncludeMimeContent`; write - `CreateItem` with `MimeContent`.
Folder names -> distinguished folder id (`sentitems`, `inbox`, ...) or a
`DisplayName` lookup via `FindFolder`.

### Config from SQLite

```yaml
source: sqlite
sqlite_path: /var/lib/imapsync/imapsync.db
# the folders/users sections in YAML are then optional and ignored
```

Only **enabled** users are read from the DB. The daemon re-reads the lists from
the DB **before every cycle** - changes via `db-*` (add/remove/disable a user,
edit folder pairs) are picked up without a restart. Populate the DB with
separate commands:

```sh
# one at a time, via parameters
imapsync db-add-folder -db imapsync.db -a Sent -b "Sent Items"
imapsync db-add-user   -db imapsync.db -name ivanov -a ivanov@corp.ru -b ivanov@corp.ru [-disabled]

# move everything from YAML
imapsync db-import-yaml -db imapsync.db -config config.yaml

# import from CSV
imapsync db-import-csv  -db imapsync.db -users users.csv -folders folders.csv

# view content / history
imapsync db-list    -db imapsync.db
imapsync db-history -db imapsync.db -user ivanov

# management
imapsync db-remove-user   -db imapsync.db -name ivanov
imapsync db-remove-folder -db imapsync.db -a Sent -b "Sent Items"
imapsync db-forget-user   -db imapsync.db -name ivanov   # reset state, sync from scratch
imapsync db-resume-user   -db imapsync.db -name ivanov   # lift the stop after an error streak
imapsync db-vacuum        -db imapsync.db
```

CSV formats (lines with `#` and a header row are skipped):

```
# users.csv
name,user_a,user_b,enabled
ivanov,ivanov@corp.ru,ivanov@corp.ru,1

# folders.csv
folder_a,folder_b
Sent,Sent Items
```

## Deduplication logic

A message in folder A is considered already present in folder B if **at least
one** of its match keys matches:

1. `Message-ID` (normalized) - if present;
2. the `X-Imapsync-Hash` header value - if we tagged the message on a previous
   copy;
3. the **surrogate hash** - `sha256(Date_UTC_unix + Subject + From)` - always
   computed.

When copying a message into the target folder an `X-Imapsync-Hash` header with
the surrogate hash is inserted. Because the surrogate hash is stable, a
`Message-ID` appearing on one side later (common on Exchange) **does not cause a
re-copy**: the copy on the other side is still found by `X-Imapsync-Hash`.

The original `INTERNALDATE` and flags (`\Seen`, `\Answered`, `\Flagged`,
`\Draft`) are preserved on copy.

## Incremental reconciliation (`state_cache`)

By default every cycle re-reads and re-parses the headers of **all** messages in
both folders - simple, self-healing, but expensive on large folders.

With `state_cache: true` (needs `sqlite_path`):

1. `UID SEARCH` - one request, returns the ID list of every message in the
   folder;
2. headers are fetched **only for new** IDs (not in the cache);
3. message parsing is cached in SQLite (`sync_msg_cache`); IDs of deleted
   messages are removed from the cache;
4. the folder validity (IMAP UIDVALIDITY) is compared with the stored one - on a
   mismatch that endpoint's cache is reset and a full rescan is done;
5. every `full_resync_every` (default 24h) the endpoint is re-read in full
   anyway - a safeguard against cache drift.

Deduplication is the same (the same multi-key index, just from cache + new
messages). `X-Imapsync-Hash` stays the ground truth: if the DB is
lost/reset the worst case is one full re-fetch. If the server supports UIDPLUS
the copied message goes into the cache immediately (via `APPENDUID`).

The daemon takes an exclusive lock on `<sqlite_path>.lock` - a second instance
on the same DB will not start.

**Sync status in the DB.** When a DB is open (with `state_cache: true` **or**
`source: sqlite` **or** `max_fail_streak > 0` with `sqlite_path` set), after
every pass over a user a row is written to `user_status` (run time, last-success
time, status, counters, last error, consecutive-error streak) and to `user_run`
- the run history (last 200 per user). View: `imapsync db-list` (current),
`imapsync db-history -user X` (history).

**Stopping a problem user.** After `max_fail_streak` (default 10) consecutive
failed runs a user's sync stops - so a broken mailbox does not burn resources
every cycle. Resume: `imapsync db-resume-user -name X` (reset the counter only)
or `imapsync db-forget-user -name X` (also reset the cache).

**DB management:** `db-remove-user` / `db-remove-folder` (remove),
`db-forget-user` (reset a user's cache+status+history, sync from scratch),
`db-vacuum` (compact the file).

The local state DB is not the source of truth for messages - only a cache; the
folders plus the `X-Imapsync-Hash` header are.

## Architecture

```
cmd/imapsync/        entry point: subcommand dispatch, daemon, signals
config/              YAML config: loading, validation, defaults
internal/
  endpoint/          the "sync endpoint" abstraction (Backend/Endpoint):
                     imap.go (over mailbox), maildir.go, ews.go
  mailbox/           IMAP primitives over go-imap: connect+TLS (ctx-aware),
                     master login, resolve-folder, fetch/UID SEARCH, append; hashes
  dedup/             multi-key folder index, delta computation
  stats/             thread-safe counters, periodic output
  syncer/            one-user sync (full and incremental reconciliation) + pool
  store/             local SQLite DB: config, state cache, per-user sync status
```

The syncer does not know about IMAP - only about `endpoint.Endpoint`. Adding
Maildir / EWS = a new file in `internal/endpoint` + a branch in
`config.Server.Type`.

## Tests

```sh
go test -race ./...
```

Integration tests for the syncer and pool start in-memory IMAP servers over a
self-signed TLS cert (and a fake EWS server), and check both sides converge and
that repeated passes are idempotent.

## Known limitations

The tech-debt list is in [`TECHDEBT.md`](TECHDEBT.md). Key open items: secrets
only in plaintext YAML (no `${VAR}` / file / secret manager); EWS auth is Basic
only (no OAuth2 for O365); `store` serializes all DB operations through one
connection.
