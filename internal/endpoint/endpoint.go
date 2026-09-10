// Package endpoint is the "sync endpoint" abstraction. The syncer works only
// through these interfaces and does not know whether it is IMAP, Maildir or EWS.
package endpoint

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"time"

	"github.com/emersion/go-imap"

	"imapsync/config"
	"imapsync/internal/mailbox"
)

// Message is message metadata for building the deduplication index.
type Message struct {
	ID           string // UID (IMAP) / filename (Maildir) / ItemId (EWS) - opaque string
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Header       []byte // raw RFC 822 header block
}

// Literal is a message body with a known length (for Append).
type Literal = imap.Literal

// Endpoint is an open session for one side and one user. At any moment at most
// one folder is selected (Select switches).
type Endpoint interface {
	// Select resolves a folder name (exact / SPECIAL-USE token "\Sent" /
	// case-insensitive) and makes it current. Returns the real name and a
	// validity token: when it changes, all previously obtained IDs are stale.
	Select(name string) (folder, validity string, err error)

	// ListIDs - IDs of every message in the current folder.
	ListIDs() ([]string, error)

	// FetchMeta - metadata of messages by ID. ids == nil means "all messages in
	// the folder".
	FetchMeta(ids []string) ([]Message, error)

	// Open - the whole message body by ID.
	Open(id string) (Literal, error)

	// Append adds a message to the current folder. Returns the new message's ID
	// ("" if the transport does not report it - then it is picked up next cycle).
	Append(flags []string, date time.Time, body Literal) (string, error)

	// ReadOnly reports whether Append is unsupported for this transport (e.g. a
	// PST archive). The syncer will not copy toward a read-only endpoint.
	ReadOnly() bool

	// Close closes the session.
	Close()
}

// Backend is a session factory for one side (server + transport type).
type Backend interface {
	// Connect establishes a session for user (authzid for IMAP).
	Connect(ctx context.Context, user string) (Endpoint, error)
	// Addr - a human-readable address for logs.
	Addr() string
}

// NewBackend builds a backend from the server config.
func NewBackend(srv config.Server, dialTimeout, ioTimeout time.Duration, insecureTLS bool, fetchBatch int) (Backend, error) {
	switch srv.Type {
	case "", config.EndpointIMAP:
		return newIMAPBackend(srv, dialTimeout, ioTimeout, insecureTLS, fetchBatch), nil
	case config.EndpointMaildir:
		return newMaildirBackend(srv), nil
	case config.EndpointEWS:
		return newEWSBackend(srv, ioTimeout, insecureTLS, fetchBatch), nil
	case config.EndpointPST:
		return newPSTBackend(srv), nil
	default:
		return nil, fmt.Errorf("endpoint type %q is not supported (%q, %q, %q, %q)",
			srv.Type, config.EndpointIMAP, config.EndpointMaildir, config.EndpointEWS, config.EndpointPST)
	}
}

// prefixedLiteral - a Literal of "prefix + body" without copying the body.
type prefixedLiteral struct {
	r      io.Reader
	length int
}

func (p *prefixedLiteral) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *prefixedLiteral) Len() int                   { return p.length }

// WithHeader returns the message literal with a "name: value" header prepended
// (without copying the body). If such a header is already present the literal is
// returned as is.
func WithHeader(body Literal, name, value string) Literal {
	if buf, ok := body.(*bytes.Buffer); ok && mailbox.HasHeader(buf.Bytes(), name) {
		return body
	}
	hdr := fmt.Appendf(nil, "%s: %s\r\n", name, value)
	return &prefixedLiteral{
		r:      io.MultiReader(bytes.NewReader(hdr), body),
		length: len(hdr) + body.Len(),
	}
}
