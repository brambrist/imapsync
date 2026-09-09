// Package mailbox - обёртка над github.com/emersion/go-imap v1: подключение с
// TLS, мастер-логин (имперсонация через SASL PLAIN authzid), операции с папками
// (list/select/fetch/append).
package mailbox

import (
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-sasl"

	"imapsync/config"
)

// Client - тонкая обёртка над imap-клиентом с привязкой к конкретному
// (сервер, целевой пользователь) для контекста в ошибках и логах.
type Client struct {
	c      *client.Client
	server string // host:port - для сообщений об ошибках
	user   string // целевой пользователь (authzid)
}

// Connect устанавливает TLS-соединение и авторизуется как targetUser через
// мастер-учётку сервера (SASL PLAIN: authcid = master_user, authzid = targetUser).
func Connect(srv config.Server, targetUser string, dialTimeout time.Duration, insecureTLS bool) (*Client, error) {
	addr := srv.Addr()

	dialer := &net.Dialer{Timeout: dialTimeout}
	tlsCfg := &tls.Config{
		ServerName:         srv.Host,
		InsecureSkipVerify: insecureTLS, //nolint:gosec // управляется конфигом insecure_tls
	}

	imapCli, err := client.DialWithDialerTLS(dialer, addr, tlsCfg)
	if err != nil {
		return nil, fmt.Errorf("подключение к %s (юзер %s): %w", addr, targetUser, err)
	}

	// Таймаут на последующие операции ввода-вывода.
	imapCli.Timeout = dialTimeout

	// SASL PLAIN с authzid: identity(authzid)=целевой юзер, username(authcid)=мастер.
	auth := sasl.NewPlainClient(targetUser, srv.MasterUser, srv.MasterPass)
	if err := imapCli.Authenticate(auth); err != nil {
		_ = imapCli.Logout()
		return nil, fmt.Errorf("имперсонация %s на %s через мастера %s: %w", targetUser, addr, srv.MasterUser, err)
	}

	return &Client{c: imapCli, server: addr, user: targetUser}, nil
}

// Logout закрывает сессию. Ошибку логаута считаем некритичной.
func (cl *Client) Logout() {
	_ = cl.c.Logout()
}

// FindFolder ищет папку по точному имени среди LIST "" "*" и возвращает её имя
// как его вернул сервер (важно для регистра/разделителей). Второе значение -
// найдена ли папка.
func (cl *Client) FindFolder(name string) (string, bool, error) {
	ch := make(chan *imap.MailboxInfo, 32)
	done := make(chan error, 1)
	go func() { done <- cl.c.List("", "*", ch) }()

	found := ""
	ok := false
	for m := range ch {
		if m.Name == name {
			found, ok = m.Name, true
		}
	}
	if err := <-done; err != nil {
		return "", false, fmt.Errorf("LIST на %s (юзер %s): %w", cl.server, cl.user, err)
	}
	return found, ok, nil
}

// Select открывает папку для чтения-записи и возвращает её статус.
func (cl *Client) Select(name string) (*imap.MailboxStatus, error) {
	st, err := cl.c.Select(name, false)
	if err != nil {
		return nil, fmt.Errorf("SELECT %q на %s (юзер %s): %w", name, cl.server, cl.user, err)
	}
	return st, nil
}

// headerSection - секция BODY.PEEK[HEADER] (без снятия флага \Seen).
var headerSection = &imap.BodySectionName{
	BodyPartName: imap.BodyPartName{Specifier: imap.HeaderSpecifier},
	Peek:         true,
}

// fullSection - секция BODY.PEEK[] (всё письмо целиком).
var fullSection = &imap.BodySectionName{Peek: true}

// FetchedMessage - письмо с метаданными и сырыми заголовками, достаточными для
// построения индекса дедупликации.
type FetchedMessage struct {
	SeqNum       uint32
	Uid          uint32
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Header       []byte // сырой блок заголовков (RFC 822)
}

// FetchHeaders забирает заголовки, флаги, INTERNALDATE, UID и размер всех писем
// папки батчами по batchSize. Папка должна быть уже выбрана через Select.
func (cl *Client) FetchHeaders(total uint32, batchSize int) ([]FetchedMessage, error) {
	if total == 0 {
		return nil, nil
	}
	items := []imap.FetchItem{
		imap.FetchUid,
		imap.FetchFlags,
		imap.FetchInternalDate,
		imap.FetchRFC822Size,
		headerSection.FetchItem(),
	}

	out := make([]FetchedMessage, 0, total)
	for from := uint32(1); from <= total; from += uint32(batchSize) {
		to := min(from+uint32(batchSize)-1, total)
		seqset := new(imap.SeqSet)
		seqset.AddRange(from, to)

		ch := make(chan *imap.Message, batchSize)
		done := make(chan error, 1)
		go func() { done <- cl.c.Fetch(seqset, items, ch) }()

		for msg := range ch {
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
					return nil, fmt.Errorf("чтение заголовков seq=%d на %s (юзер %s): %w", msg.SeqNum, cl.server, cl.user, err)
				}
				fm.Header = raw
			}
			out = append(out, fm)
		}
		if err := <-done; err != nil {
			return nil, fmt.Errorf("FETCH заголовков %d:%d на %s (юзер %s): %w", from, to, cl.server, cl.user, err)
		}
	}
	return out, nil
}

// FetchFull возвращает письмо целиком (заголовки + тело) по его UID.
func (cl *Client) FetchFull(uid uint32) ([]byte, error) {
	seqset := new(imap.SeqSet)
	seqset.AddNum(uid)

	items := []imap.FetchItem{fullSection.FetchItem()}
	ch := make(chan *imap.Message, 1)
	done := make(chan error, 1)
	go func() { done <- cl.c.UidFetch(seqset, items, ch) }()

	var raw []byte
	for msg := range ch {
		body := msg.GetBody(fullSection)
		if body == nil {
			continue
		}
		b, err := io.ReadAll(body)
		if err != nil {
			return nil, fmt.Errorf("чтение тела uid=%d на %s (юзер %s): %w", uid, cl.server, cl.user, err)
		}
		raw = b
	}
	if err := <-done; err != nil {
		return nil, fmt.Errorf("FETCH тела uid=%d на %s (юзер %s): %w", uid, cl.server, cl.user, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("письмо uid=%d не найдено на %s (юзер %s)", uid, cl.server, cl.user)
	}
	return raw, nil
}

// Append дописывает письмо в папку, сохраняя флаги и внутреннюю дату оригинала.
func (cl *Client) Append(folder string, flags []string, date time.Time, body []byte) error {
	// *bytes.Buffer реализует imap.Literal (io.Reader + Len() int).
	if err := cl.c.Append(folder, flags, date, bytes.NewBuffer(body)); err != nil {
		return fmt.Errorf("APPEND в %q на %s (юзер %s): %w", folder, cl.server, cl.user, err)
	}
	return nil
}
