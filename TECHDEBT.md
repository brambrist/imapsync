# Technical debt

Grouped by priority. `[DONE]` - closed; everything else is open.

Closed: A1 (IMAP + Maildir + EWS), A3 (PST import), P1, P2, P4, P5, M1, M2, M3,
M4, M7, M8, L4.

Added beyond the list: `max_fail_streak` - stop a user's sync after N
consecutive errors (lift with `db-resume-user`); `direction` config knob +
`Endpoint.ReadOnly()` (a read-only endpoint can only be a sync source).

## Architecture and extensibility

### A1. [DONE] Variable synchronization sources

Package `internal/endpoint`: `Backend` (session factory) + `Endpoint`
(`Select` / `ListIDs` / `FetchMeta` / `Open` / `Append` / `Close`). The `syncer`
works only through these. Implementations: `imap.go` (on top of `mailbox.Client`),
`maildir.go`, `ews.go`. The message ID and validity token are strings.
`config.Server.Type` (`"" | imap | maildir | ews`).

EWS - what's left to polish:

- **Auth is Basic only.** O365 disabled it - OAuth2 is needed (client
  credentials: tenant/client_id/client_secret + the `full_access_as_app` scope).
  NTLM for old on-prem - the `go-ntlmssp` transport.
- **INTERNALDATE is not carried** - `CreateItem` with `MimeContent` sets the
  current date; the extended MAPI property `PR_MESSAGE_DELIVERY_TIME`
  (0x0E060040) is needed.
- **Flags** - only `\Seen` is carried (via `<t:IsRead>` next to MimeContent,
  best-effort; Exchange may ignore it). `\Answered`/`\Flagged` are not.
- `FindItem` has no sorting / CONDSTORE analogue; with >1e5 messages in a folder
  Offset paging can "slide" - not critical for the incremental mode (ListIDs
  returns the full set).

**Maildir** - `endpoint/maildir.go` is done: `type: maildir`, `root` is a path
template with `%u`/`%n`/`%d`; ID = the unique part of the filename (stable
across flag changes / new<->cur); validity is a constant (drift is self-healed
by the diff); `Append` writes `tmp/` -> `new/` (no flags) or `cur/...:2,FRS`
(with flags), `INTERNALDATE` via mtime; SPECIAL-USE tokens -> `.Sent`/`.Drafts`/...;
subfolders are created on Select. Tests: unit + Maildir<->Maildir +
IMAP<->Maildir (full and incremental sync).

Maildir - what's left: does not read `subscriptions`; does not support the `:1,`
info suffix or `;2,` (the non-Linux separator); `Open` reads the whole file
(fine for local disk); `dovecot-uidvalidity` is not used.

### A3. [DONE] Local mail archives (PST/OST) as a read-only source

Scoped to PST/OST (the "real" Outlook local archive; MSG/mbox/eml deferred).
Import-only: `type: pst`, `root` path template (`%u`/`%n`/`%d`), `ReadOnly()`
returns true, `Append` errors, and `config.validateBase` rejects a `direction`
that points into a read-only endpoint (and both-read-only).

`internal/endpoint/pst.go` reads via `github.com/mooijtech/go-pst/v6` (pure Go).
The message ID is the node identifier; validity is the constant `"pst"`. Each
message is reserialized to RFC 822: the raw `PidTagTransportMessageHeaders`
block when present (received mail, with the MIME framing headers stripped so the
rebuilt body applies), otherwise headers synthesized from the MAPI properties
(`From` from `PidTagSenderSmtpAddress` / sent-representing / sender, `Date` from
`PidTagClientSubmitTime` -> delivery time, `Subject`, `PidTagInternetMessageId`).
Body preference HTML > plaintext > decoded RTF; `PidTagBodyHtml` is frequently
`PtypBinary`, so the raw property is read and charset-decoded (UTF-16 BOM / valid
UTF-8 / Windows-1252). By-value attachments go into a `multipart/mixed`. Folder
resolution: exact -> SPECIAL-USE candidates -> `INBOX` alias -> case-insensitive.
Tests: `internal/endpoint/pst_test.go` + `internal/syncer/pst_test.go` against a
gzipped copy of the public `support.pst` sample (`testdata/support.pst.gz`).

What's left to polish:

- **DKIM / signatures break** - the body is rebuilt from properties, so the
  original bytes (and any `DKIM-Signature` / S/MIME) no longer verify. Inherent
  to reconstruction.
- **RTF-only bodies** stay as raw RTF markup wrapped in `text/plain` (the LZFu
  decoder is not an RTF-to-text/HTML converter). Rare for modern Outlook.
- **Embedded messages / OLE attachments** (AttachMethod != 1) are skipped, not
  emitted as `message/rfc822`.
- **ANSI (32-bit) PST** is not supported by the reader (`go-pst` fails on the
  folder-name property). Unicode PST/OST only.
- Sent items from Outlook frequently lack a Message-ID -> the surrogate hash is
  the dedup key, which makes M5 (collisions) more pressing: consider folding the
  recipient list + a body hash into the surrogate for MAPI sources.
- `MessageCount == 0` on a folder is trusted (matches `go-pst`'s own iterator
  guard); a folder with a wrong count would be read as empty.

### A2. REST API for management (idea, assessment)

Duplicate the `db-*` commands over HTTP. Assessment: reasonable, but not now.

- **Prerequisite [DONE].** With `source: sqlite` the pool re-reads the
  users/folders lists from the DB before every cycle (`Pool.reload`) - changes
  via `db-*` are picked up without restarting the daemon.
- **Where it lives.** Inside the daemon process (it holds the `flock` on the
  DB). A separate process would fight for the lock. And from the daemon a cycle
  can be triggered directly.
- **Transport.** A unix socket by default (FS permissions, no network, `curl
  --unix-socket`). TCP - only with a token / mTLS.
- **Layer.** Thin handlers over the existing `store` methods (the contract is
  already there and tested). No business logic in HTTP, no OpenAPI ceremony.
- **Scope.** ~10 endpoints: users CRUD + resume/forget/runs, folders, import,
  vacuum, status/healthz. `run` is not part of the API.
- **Alternative.** If only monitoring is needed - `/metrics` (Prometheus, L3) is
  more valuable than a mutation API.
- **Effort estimate:** ~1 day (hot-reload + unix socket + tests); TCP+auth on
  top.

## Critical before production

### P1. [DONE] `per_user_timeout` does not interrupt a hung IMAP call

Added `io_timeout` (default 5m) - a deadline for every IMAP operation. In
addition `mailbox.Connect` takes `ctx` and closes the connection (`Terminate`)
when it is cancelled, interrupting a hung call. The pool cancels `ctx` on
`per_user_timeout` and SIGTERM.

### P2. [DONE] No fallback for a folder name

`mailbox.ResolveFolder`: exact name -> SPECIAL-USE token (`\Sent` etc.) ->
case-insensitive; on a miss - an error listing the folders. The LIST result is
cached for the session.

### P3. Secrets only in plaintext YAML

CLAUDE.md promises "via environment variables" - not done. No `${VAR}`, no
reading a password from a file / secret manager. Only `chmod 600` for now.

### P4. [DONE] One daemon per DB without a lock

`store.OpenExclusive` takes a `flock` on `<sqlite_path>.lock`; the daemon uses
it. A second instance on the same DB fails with a clear error.

### P5. [DONE] No retries on transient errors

`Syncer.dial` retries the connection (`connect_retries`, exponential
`retry_backoff`). On a drop mid-user (`isConnErr`) - reconnect and one retry of
the current folder pair. `connect_retries: 0` = default 3, a negative value
disables retries.

## Medium priority

### M1. [DONE] Incremental mode: periodic full resync

`full_resync_every` (default 24h): once the interval passes the endpoint is
reset and the folder is re-read in full. The last-rescan time is in
`sync_endpoint.full_resync_at`.

### M2. [DONE] A copied message goes into the cache immediately

`mailbox.AppendLiteral` reads `[APPENDUID]` (UIDPLUS). If the server supports it,
the copy goes straight into the cache for the right side and is not re-read next
cycle. Without UIDPLUS the behaviour is as before (re-read).

### M3. [DONE] `store` = `SetMaxOpenConns(1)`

A DSN with pragmas (`busy_timeout=10000`, `journal_mode=WAL`,
`synchronous=NORMAL`, `foreign_keys=1`) applied to every connection; the pool is
raised to 8. Writes serialize via `busy_timeout` (wait instead of failing),
reads go concurrently.

### M4. [DONE, partially] `copyOne` held the message in memory 3 times

`FetchFullLiteral` returns the go-imap literal as is; `endpoint.WithHeader` adds
the custom header in a stream (`io.MultiReader`, no body copy); `AppendLiteral`
sends the literal directly. It was 3x the message size transiently, now 1x
(+ ~40 bytes). Full FETCH->APPEND streaming is impossible: go-imap v1 buffers
the response literal fully in memory (`read.go:ReadLiteral` -> `make([]byte, n)`).
A migration to v2 or a fork is needed - see L6/A1.

### M5. Surrogate hash collisions

Date+Subject+From: messages without a `Message-ID` and with identical fields
(mailing lists, autoreplies) are treated as duplicates and not copied. CLAUDE.md
allows extending the composition - not done.

### M6. `insecure_tls` is global, not per-server

No STARTTLS (implicit TLS 993 only), no client certs, no pinning.

### M7. [DONE] `user_status` without history

The `user_run` table - one row per run (retention: last 200 per user).
`user_status` got `fail_since` (start of the current error streak) and
`fail_streak`. View: `imapsync db-history -user X`; `db-list` shows the streak.

### M8. [DONE] No DB management commands

`db-remove-user`, `db-remove-folder`, `db-forget-user` (reset a user's
cache/status/history), `db-vacuum`. `user_run` retention is automatic.
Left: no auto-retention for `sync_msg_cache` (it self-cleans as messages are
deleted on the server anyway).

## Low priority

### L1. `idx.Dups()` is not surfaced

Internal folder duplicates are counted but end up neither in stats nor in the
log.

### L2. Logs - a flat `log.Printf` to stderr

No levels (debug/info/warn) or structured format. We rely on journald.

### L3. No metrics / healthcheck

No Prometheus, no HTTP status endpoint.

### L4. [DONE] Duplicated `-db` flag parsing

Extracted the common scaffold `withStore(name, args, setup, fn)` in
`cmd/imapsync/db.go`.

### L5. `config.Duration` - Unmarshal only

The config does not serialize back.

### L6. go-imap v1 is in maintenance mode

v2 is more active, but the migration is large (and CLAUDE.md explicitly requires
v1).

## Tests

### T1. No tests in `cmd/imapsync`

CSV import, subcommand dispatch, `db-list` with status - manual checks only.

### T2. Not covered

Validity-change reset, a real per-user timeout, cancellation mid-FETCH,
surrogate hash collisions.

### T3. Integration is tied to `go-imap/backend/memory`

It does not reproduce Dovecot/Exchange (SEARCH, APPENDUID, flag behaviour). No
test against a real server (even in docker).

### T4. No benchmarks

The "incremental is faster" hypothesis is not measured quantitatively.
