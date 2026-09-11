package endpoint

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"imapsync/config"
	"imapsync/internal/mailbox"
)

// imapBackend - the IMAP implementation of Backend on top of mailbox.Client.
type imapBackend struct {
	srv         config.Server
	dialTimeout time.Duration
	ioTimeout   time.Duration
	insecureTLS bool
	fetchBatch  int
	debug       bool
	logf        func(string, ...any)
}

func newIMAPBackend(srv config.Server, dial, io time.Duration, insecureTLS bool, fetchBatch int, debug bool, logf func(string, ...any)) *imapBackend {
	return &imapBackend{srv: srv, dialTimeout: dial, ioTimeout: io, insecureTLS: insecureTLS, fetchBatch: fetchBatch, debug: debug, logf: logf}
}

func (b *imapBackend) Addr() string { return b.srv.Addr() }

func (b *imapBackend) Connect(ctx context.Context, user string) (Endpoint, error) {
	cl, err := mailbox.Connect(ctx, b.srv, user, b.dialTimeout, b.ioTimeout, b.insecureTLS, b.debug, b.logf)
	if err != nil {
		return nil, err
	}
	return &imapEndpoint{cl: cl, batch: b.fetchBatch}, nil
}

// imapEndpoint - an open IMAP connection with one selected folder.
type imapEndpoint struct {
	cl     *mailbox.Client
	batch  int
	folder string // currently selected folder
	count  uint32 // message count in it (for FETCH by sequence number)
}

func parseUID(id string) (uint32, error) {
	u, err := strconv.ParseUint(id, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid UID %q: %w", id, err)
	}
	return uint32(u), nil
}

func uidStr(u uint32) string { return strconv.FormatUint(uint64(u), 10) }

func (e *imapEndpoint) Select(name string) (string, string, error) {
	real, err := e.cl.ResolveFolder(name)
	if err != nil {
		return "", "", err
	}
	st, err := e.cl.Select(real)
	if err != nil {
		return "", "", err
	}
	e.folder = real
	e.count = st.Messages
	return real, uidStr(st.UidValidity), nil
}

func (e *imapEndpoint) ListIDs() ([]string, error) {
	uids, err := e.cl.UIDSearchAll()
	if err != nil {
		return nil, err
	}
	ids := make([]string, len(uids))
	for i, u := range uids {
		ids[i] = uidStr(u)
	}
	return ids, nil
}

func (e *imapEndpoint) FetchMeta(ids []string) ([]Message, error) {
	var fm []mailbox.FetchedMessage
	var err error
	if ids == nil {
		fm, err = e.cl.FetchHeaders(e.count, e.batch)
	} else {
		uids := make([]uint32, 0, len(ids))
		for _, id := range ids {
			u, perr := parseUID(id)
			if perr != nil {
				return nil, perr
			}
			uids = append(uids, u)
		}
		fm, err = e.cl.FetchHeadersByUID(uids, e.batch)
	}
	if err != nil {
		return nil, err
	}
	out := make([]Message, len(fm))
	for i, m := range fm {
		out[i] = Message{
			ID:           uidStr(m.Uid),
			Flags:        m.Flags,
			InternalDate: m.InternalDate,
			Size:         m.Size,
			Header:       m.Header,
		}
	}
	return out, nil
}

func (e *imapEndpoint) Open(id string) (Literal, error) {
	u, err := parseUID(id)
	if err != nil {
		return nil, err
	}
	return e.cl.FetchFullLiteral(u)
}

func (e *imapEndpoint) Append(flags []string, date time.Time, body Literal) (string, error) {
	uid, err := e.cl.AppendLiteral(e.folder, flags, date, body)
	if err != nil {
		return "", err
	}
	if uid == 0 {
		return "", nil // server without UIDPLUS
	}
	return uidStr(uid), nil
}

func (e *imapEndpoint) ReadOnly() bool { return false }

func (e *imapEndpoint) Close() { e.cl.Logout() }
