// Package mailbox wraps github.com/emersion/go-imap v1: TLS connection, master
// login (impersonation via SASL PLAIN authzid) and folder operations
// (list/select/fetch/append).
package mailbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-sasl"

	"imapsync/config"
)

// debugLogWriter adapts an io.Writer (as expected by client.Client.SetDebug)
// to a logf sink: it buffers partial writes and emits one logf call per
// complete line, prefixed. See Connect's debug parameter for what ends up in
// this log (including credentials for IMAP).
type debugLogWriter struct {
	logf   func(string, ...any)
	prefix string
	buf    []byte
}

func (w *debugLogWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		line := bytes.TrimRight(w.buf[:i], "\r")
		if len(line) > 0 {
			w.logf("%s%s", w.prefix, line)
		}
		w.buf = w.buf[i+1:]
	}
	return len(p), nil
}

// Client is a thin wrapper over the imap client bound to a specific
// (server, target user) for context in errors and logs.
type Client struct {
	c      *client.Client
	server string // host:port - for error messages
	user   string // target user (authzid)

	closeOnce sync.Once
	closed    chan struct{}

	folders []*imap.MailboxInfo // LIST cache for the connection lifetime
}

// Connect establishes a TLS connection and authenticates as targetUser via the
// server's master account (SASL PLAIN: authcid = master_user, authzid = targetUser).
//
// ioTimeout is the deadline for one IMAP operation. In addition, when ctx is
// cancelled the TCP connection is force-closed (Terminate), which interrupts a
// hung call - go-imap v1 does not react to context on its own.
//
// If debug is set, logf receives the server's advertised capabilities and the
// raw IMAP wire traffic (go-imap's Client.SetDebug). WARNING: the wire log
// includes the AUTHENTICATE PLAIN payload - the master account's credentials,
// base64 encoded but trivially decodable - so treat it as a secret, the same
// way you would Dovecot's auth_debug_passwords.
func Connect(ctx context.Context, srv config.Server, targetUser string, dialTimeout, ioTimeout time.Duration, insecureTLS, debug bool, logf func(string, ...any)) (*Client, error) {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	addr := srv.Addr()

	dialer := &net.Dialer{Timeout: dialTimeout}
	tlsCfg, err := srv.TLSConfig(insecureTLS)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s (user %s): %w", addr, targetUser, err)
	}

	imapCli, err := client.DialWithDialerTLS(dialer, addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("connecting to %s (user %s): %w", addr, targetUser, err)
	}

	// Deadline for every subsequent I/O operation.
	imapCli.Timeout = ioTimeout

	if debug {
		logf("imap %s (user %s): WARNING - wire debug logging is on, the log below "+
			"includes the AUTHENTICATE payload (credentials, base64-encoded) - treat it as a secret", addr, targetUser)
		imapCli.SetDebug(&debugLogWriter{logf: logf, prefix: fmt.Sprintf("imap %s (user %s): ", addr, targetUser)})
		if caps, capErr := imapCli.Capability(); capErr == nil {
			names := make([]string, 0, len(caps))
			for c := range caps {
				names = append(names, c)
			}
			sort.Strings(names)
			logf("imap %s: server capabilities: %s", addr, strings.Join(names, " "))
		} else {
			logf("imap %s: CAPABILITY failed: %v", addr, capErr)
		}
	}

	// SASL PLAIN with authzid: identity(authzid)=target user, username(authcid)=master.
	auth := sasl.NewPlainClient(targetUser, srv.MasterUser, srv.MasterPass)
	if err := imapCli.Authenticate(auth); err != nil {
		_ = imapCli.Logout()
		return nil, fmt.Errorf("impersonating %s on %s via master %s: %w", targetUser, addr, srv.MasterUser, err)
	}

	cl := &Client{c: imapCli, server: addr, user: targetUser, closed: make(chan struct{})}
	go cl.watch(ctx)
	return cl, nil
}

// watch closes the connection when ctx is cancelled, interrupting any hung call.
func (cl *Client) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = cl.c.Terminate()
	case <-cl.closed:
	}
}

// Logout closes the session. A logout error is treated as non-critical.
func (cl *Client) Logout() {
	cl.closeOnce.Do(func() { close(cl.closed) })
	_ = cl.c.Logout()
}

// known SPECIAL-USE attributes (RFC 6154).
var specialUseAttrs = map[string]bool{
	`\All`: true, `\Archive`: true, `\Drafts`: true, `\Flagged`: true,
	`\Junk`: true, `\Sent`: true, `\Trash`: true,
}

func (cl *Client) listFolders() ([]*imap.MailboxInfo, error) {
	if cl.folders != nil {
		return cl.folders, nil
	}
	ch := make(chan *imap.MailboxInfo, 64)
	done := make(chan error, 1)
	go func() { done <- cl.c.List("", "*", ch) }()

	var out []*imap.MailboxInfo
	for m := range ch {
		out = append(out, m)
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("LIST on %s (user %s): %w", cl.server, cl.user, err)
	}
	cl.folders = out
	return out, nil
}

// ResolveFolder maps a folder name from the config to the real name on the
// server:
//  1. exact match;
//  2. if name is a SPECIAL-USE token (e.g. "\Sent") - find a folder with that
//     attribute;
//  3. case-insensitive match.
//
// If the folder is not found - an error listing the available folders.
func (cl *Client) ResolveFolder(name string) (string, error) {
	infos, err := cl.listFolders()
	if err != nil {
		return "", err
	}

	if specialUseAttrs[name] {
		for _, m := range infos {
			for _, a := range m.Attributes {
				if strings.EqualFold(a, name) {
					return m.Name, nil
				}
			}
		}
	}
	for _, m := range infos {
		if m.Name == name {
			return m.Name, nil
		}
	}
	for _, m := range infos {
		if strings.EqualFold(m.Name, name) {
			return m.Name, nil
		}
	}

	avail := make([]string, 0, len(infos))
	for _, m := range infos {
		avail = append(avail, m.Name)
	}
	return "", fmt.Errorf("folder %q not found on %s (user %s); available: %s",
		name, cl.server, cl.user, strings.Join(avail, ", "))
}

// Select opens a folder for read-write and returns its status.
func (cl *Client) Select(name string) (*imap.MailboxStatus, error) {
	st, err := cl.c.Select(name, false)
	if err != nil {
		return nil, fmt.Errorf("SELECT %q on %s (user %s): %w", name, cl.server, cl.user, err)
	}
	return st, nil
}

// headerSection is the BODY.PEEK[HEADER] section (does not clear the \Seen flag).
var headerSection = &imap.BodySectionName{
	BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier},
	Peek:         true,
}

// fullSection is the BODY.PEEK[] section (the whole message).
var fullSection = &imap.BodySectionName{Peek: true}

// FetchedMessage is a message with metadata and raw headers, enough to build
// the dedup index.
type FetchedMessage struct {
	SeqNum       uint32
	Uid          uint32
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Header       []byte // raw header block (RFC 822)
}

var headerFetchItems = []imap.FetchItem{
	imap.FetchUid,
	imap.FetchFlags,
	imap.FetchInternalDate,
	imap.FetchRFC822Size,
	headerSection.FetchItem(),
}

// decodeHeaderMsg turns an *imap.Message into a FetchedMessage, reading the
// header block.
func (cl *Client) decodeHeaderMsg(msg *imap.Message) (FetchedMessage, error) {
	fm := FetchedMessage{
		SeqNum:       msg.SeqNum,
		Uid:          msg.Uid,
		Flags:        msg.Flags,
		InternalDate: msg.InternalDate,
		Size:         msg.Size,
	}
	if body := msg.GetBody(headerSection); body != nil {
		raw, err := io.ReadAll(body)
		if err != nil {
			return fm, fmt.Errorf("reading headers uid=%d on %s (user %s): %w", msg.Uid, cl.server, cl.user, err)
		}
		fm.Header = raw
	}
	return fm, nil
}

// FetchHeaders fetches headers, flags, INTERNALDATE, UID and size of every
// message in the folder in batches of batchSize (by sequence number). The folder
// must already be selected via Select.
func (cl *Client) FetchHeaders(total uint32, batchSize int) ([]FetchedMessage, error) {
	if total == 0 {
		return nil, nil
	}
	out := make([]FetchedMessage, 0, total)
	for from := uint32(1); from <= total; from += uint32(batchSize) {
		to := min(from+uint32(batchSize)-1, total)
		seqset := new(imap.SeqSet)
		seqset.AddRange(from, to)

		ch := make(chan *imap.Message, batchSize)
		done := make(chan error, 1)
		go func() { done <- cl.c.Fetch(seqset, headerFetchItems, ch) }()

		for msg := range ch {
			fm, err := cl.decodeHeaderMsg(msg)
			if err != nil {
				return nil, err
			}
			out = append(out, fm)
		}
		if err := <-done; err != nil {
			return nil, fmt.Errorf("FETCH headers %d:%d on %s (user %s): %w", from, to, cl.server, cl.user, err)
		}
	}
	return out, nil
}

// UIDSearchAll returns the UIDs of every message in the selected folder
// (SEARCH UID 1:*). Cheap even for large folders: one round-trip, the response
// is a list of numbers.
func (cl *Client) UIDSearchAll() ([]uint32, error) {
	crit := imap.NewSearchCriteria()
	crit.Uid = new(imap.SeqSet)
	crit.Uid.AddRange(1, 0) // 1:*
	uids, err := cl.c.UidSearch(crit)
	if err != nil {
		return nil, fmt.Errorf("UID SEARCH on %s (user %s): %w", cl.server, cl.user, err)
	}
	return uids, nil
}

// FetchHeadersByUID fetches headers, flags, INTERNALDATE and size of the
// messages with the given UIDs in batches of batchSize. The folder must be
// selected via Select.
func (cl *Client) FetchHeadersByUID(uids []uint32, batchSize int) ([]FetchedMessage, error) {
	if len(uids) == 0 {
		return nil, nil
	}
	if batchSize < 1 {
		batchSize = len(uids)
	}
	out := make([]FetchedMessage, 0, len(uids))
	for start := 0; start < len(uids); start += batchSize {
		end := min(start+batchSize, len(uids))
		seqset := new(imap.SeqSet)
		for _, u := range uids[start:end] {
			seqset.AddNum(u)
		}

		ch := make(chan *imap.Message, end-start)
		done := make(chan error, 1)
		go func() { done <- cl.c.UidFetch(seqset, headerFetchItems, ch) }()

		for msg := range ch {
			fm, err := cl.decodeHeaderMsg(msg)
			if err != nil {
				return nil, err
			}
			out = append(out, fm)
		}
		if err := <-done; err != nil {
			return nil, fmt.Errorf("FETCH headers by UID (%d of them) on %s (user %s): %w", end-start, cl.server, cl.user, err)
		}
	}
	return out, nil
}

// FetchFullLiteral returns the whole message (headers + body) by UID as an
// imap.Literal. go-imap v1 buffers the literal in memory anyway, but this way we
// avoid one more copy (io.ReadAll) on top of that.
func (cl *Client) FetchFullLiteral(uid uint32) (imap.Literal, error) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)

	items := []imap.FetchItem{fullSection.FetchItem()}
	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() { done <- cl.c.UidFetch(seqset, items, ch) }()

	var lit imap.Literal
	for msg := range ch {
		if body := msg.GetBody(fullSection); body != nil {
			lit = body
		}
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("FETCH body uid=%d on %s (user %s): %w", uid, cl.server, cl.user, err)
	}
	if lit == nil {
		return nil, fmt.Errorf("message uid=%d not found on %s (user %s)", uid, cl.server, cl.user)
	}
	return lit, nil
}

// Append adds a message to a folder, preserving the original flags and internal
// date.
func (cl *Client) Append(folder string, flags []string, date time.Time, body []byte) error {
	_, err := cl.AppendLiteral(folder, flags, date, bytes.NewBuffer(body))
	return err
}

// AppendLiteral adds a message (passed as an imap.Literal) and tries to return
// the UID assigned to it from the [APPENDUID] response (UIDPLUS, RFC 4315). If
// the server does not support it - uid == 0 and no error.
func (cl *Client) AppendLiteral(folder string, flags []string, date time.Time, msg imap.Literal) (uint32, error) {
	cmd := &commands.Append{Mailbox: folder, Flags: flags, Date: date, Message: msg}
	status, err := cl.c.Execute(cmd, nil)
	if err != nil {
		return 0, fmt.Errorf("APPEND to %q on %s (user %s): %w", folder, cl.server, cl.user, err)
	}
	if err := status.Err(); err != nil {
		return 0, fmt.Errorf("APPEND to %q on %s (user %s): %w", folder, cl.server, cl.user, err)
	}
	if status.Code == "APPENDUID" && len(status.Arguments) == 2 {
		if uid, err := imap.ParseNumber(status.Arguments[1]); err == nil {
			return uid, nil
		}
	}
	return 0, nil
}
