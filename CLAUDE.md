# imapsync — two-way mailbox synchronizer

## What it is

A Go CLI daemon for **two-way** synchronization of mail folders between two
endpoints. The main scenario is syncing the Sent folder between two mail
installations where the same user has mailboxes on both.

Built incrementally via Claude Code, compiled and tested as we go.
Style: incremental edits, not wholesale rewrites. Hyphens, not em dashes.

## Requirements (agreed)

- **Language:** Go
- **IMAP library:** `github.com/emersion/go-imap` **v1** (the stable v1.2.x
  branch, NOT v2). For message parsing - `github.com/emersion/go-message`.
- **Sync direction:** two-way, but **copy missing messages only** (append). Flags
  (read/deleted) and deletions are NOT synced - we only append to each side the
  messages it lacks.
- **Concurrency:** a worker pool. **Fewer workers than users** - workers pull
  users from a queue. One user is processed by one worker end to end (a user is
  not split across workers).
- **Multi-user:** **master access** - one service account (master user) per
  server impersonates the target users. IMAP impersonation is SASL PLAIN with
  authzid (authcid+password are the master's, authzid is the target user).
- **Logging:**
  - errors - as they happen, with context (user, folder, server);
  - a periodic **summary over all users** (every StatsInterval);
  - per-user stats for the **currently processed** user (how many copied each
    way, how many skipped as duplicates, errors).

## Key deduplication logic (important, an Exchange quirk)

Messages cannot be compared by `Message-ID` alone because:
1. `Message-ID` may be **absent**;
2. on Exchange `Message-ID` may **appear later** (not right after the message
   shows up in the folder) - so one side already has it while the other does not
   have that message yet, or has it without an ID.

Message identity algorithm:
1. If the message has a `Message-ID` - compare by it (normalized).
2. If there is no `Message-ID` - compute a **surrogate key**: a hash of the
   significant fields: `Date (UTC unix) + Subject (trimmed, as is) + From
   (normalized address)`. The hash is written into a **separate custom header**
   on the target side on append (`X-Imapsync-Hash: <hex>`) so later passes find
   the already-copied message by that header instead of recomputing.
3. The final match set for a message: `Message-ID` (if present), the
   `X-Imapsync-Hash` value (if we set it), and the always-computed surrogate
   hash. Two messages are identical if any pair of keys matches.

This guards against:
- duplicates when there is no Message-ID;
- re-copying when the Message-ID appeared later (the surrogate hash stays stable
  and is already written into the header on the target side).

## Architecture

```
imapsync/
  CLAUDE.md
  go.mod
  cmd/
    imapsync/               entry point: subcommand dispatch, daemon, signals
  config/
    config.go               YAML config: servers, users, folders, workers/timings
  internal/
    endpoint/
      endpoint.go            Backend/Endpoint interfaces (the "sync endpoint"
                             abstraction); the syncer only knows about these
      imap.go                IMAP implementation on top of mailbox
      maildir.go             Maildir/Maildir++ on disk (type: maildir, root)
      ews.go                 Exchange Web Services (type: ews, SOAP, Basic auth)
      ewstest/               a fake EWS server for tests
    mailbox/
      client.go             IMAP primitives on top of go-imap v1: connect+TLS,
                             master login (impersonation), resolve/select/fetch/append
      message.go            message parsing: Message-ID, Date, Subject, surrogate
                             hash, reading/writing X-Imapsync-Hash
    dedup/
      index.go              build a folder message index (key -> presence),
                             compare two folders, compute what's missing on each side
    syncer/
      syncer.go             one-user sync logic: connect to both endpoints, build
                             indexes per folder, compute the delta, append the
                             missing messages both ways, collect stats
      pool.go               worker pool: user queue, N workers (N < users),
                             per-user timeout, stats collection, graceful shutdown
    stats/
      stats.go              per-user and aggregate counters, thread-safe
                             (atomic/mutex), periodic summary and per-user output
    store/
      store.go              local SQLite DB (modernc.org/sqlite, no CGO): folder
                             pairs and users. An alternative config source with
                             source: sqlite so large lists don't live in YAML.
      state.go              incremental-reconciliation cache (sync_endpoint,
                             sync_msg_cache) and per-user sync status
                             (user_status, user_run). Enabled by state_cache.
```

## CLI

```
imapsync run -config cfg.yaml               run the daemon (default subcommand)
imapsync db-add-user     -db x.db -name ivanov -a ivanov@a -b ivanov@b [-disabled]
imapsync db-add-folder   -db x.db -a Sent -b "Sent Items"
imapsync db-import-yaml   -db x.db -config cfg.yaml     move the YAML lists into the DB
imapsync db-import-csv    -db x.db [-users u.csv] [-folders f.csv]
imapsync db-list         -db x.db
imapsync db-history      -db x.db -user ivanov [-limit 20]
imapsync db-remove-user  -db x.db -name ivanov      (removes the user + its state)
imapsync db-remove-folder -db x.db -a Sent -b "Sent Items"
imapsync db-forget-user  -db x.db -name ivanov      (reset cache/status/history)
imapsync db-resume-user  -db x.db -name ivanov      (lift the error-streak stop)
imapsync db-vacuum       -db x.db
```

CSV: `users` - `name,user_a,user_b[,enabled]`; `folders` - `folder_a,folder_b`.
Lines with '#' and a header row are skipped.

With `source: sqlite` the daemon re-reads the lists from the DB before every
cycle (`Pool.reload`) - `db-*` changes are picked up without a restart. Run
history lives in `user_run` (retention 200/user).

Folder names in `folders` are resolved: exact name -> SPECIAL-USE token (`\Sent`)
-> case-insensitive. The daemon takes a `flock` on `<sqlite_path>.lock` while a
DB is open.

## Config (YAML)

See `config.example.yaml`. Server/timing/`workers` fields always come from YAML;
the folder and user lists may live in SQLite (`source: sqlite`). `type` on each
server is `imap` (default), `maildir` or `ews`, and may differ between sides.

## Resolved design questions

1. **Folder name mapping A<->B.** An explicit list of pairs
   `folders: [{a: "Sent", b: "Sent Items"}]`, global for all users.
2. **Surrogate hash composition.** `Date (UTC unix) + Subject (trim, as is) +
   From (normalized address)`. Re:/Fwd: are left alone. Messages without a Date
   use unix=0.
3. **Message-ID appearing later (Exchange).** No separate pass is needed.
   `mailbox.MatchKeys` returns ALL of a message's keys at once (mid + the
   written X-Imapsync-Hash + the always-computed surrogate); messages are
   identical if any pair of keys matches. The surrogate is stable, so a late
   Message-ID does not cause duplication.
4. **APPEND and internal date/flags.** The original INTERNALDATE is preserved;
   of the flags only `\Seen \Answered \Flagged \Draft` are carried (`\Recent`
   cannot be set, `\Deleted` is not synced). APPEND reads the APPENDUID response
   (UIDPLUS) when the server returns it.
5. **Idempotency and restarts.** By default the index is rebuilt from the
   folders every cycle - no state DB is needed, the folders + X-Imapsync-Hash
   are the source of truth. Optionally `state_cache: true` enables incremental
   reconciliation: the ID list comes from UID SEARCH, only new messages are
   fetched, parsing is cached in sqlite; when the folder validity
   (IMAP UIDVALIDITY) changes the endpoint cache is reset. The cache is only a
   speed-up, not a source of truth (a reset means a full re-fetch). While a DB
   is open `user_status` is also written (when and with what result each user's
   sync ran).
6. **Impersonation mechanism (IMAP).** SASL PLAIN with authzid on both servers:
   `sasl.NewPlainClient(targetUser, master_user, master_pass)` (identity=authzid
   = target user). Implemented in `mailbox.Connect`.
7. **Limits and throttling.** The master account touches many mailboxes - the
   number of simultaneous connections is capped at the worker count.

## Dependencies

```
github.com/emersion/go-imap v1.2.x      // IMAP client (v1!)
github.com/emersion/go-message          // MIME/header parsing
github.com/emersion/go-sasl             // SASL PLAIN with authzid (impersonation)
gopkg.in/yaml.v3                         // config
modernc.org/sqlite                      // local config/state DB (no CGO)
```

## Conventions

- Comments, logs and docs in English; code/identifiers in English.
- Hyphens, not em dashes.
- Incremental edits. Compile after each module (`go build ./...`).
- Wrap errors with `fmt.Errorf("...: %w", err)`, log with context
  (user/folder/server).
- No secrets in code - only via config / environment variables.
