// Package mailbox - обёртка над github.com/emersion/go-imap v1: подключение с
// TLS, мастер-логин (имперсонация через SASL PLAIN authzid), операции с папками
// (list/select/fetch/append).
package mailbox

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-sasl"

	"imapsync/config"
)

// Client - тонкая обёртка над imap-клиентом с привязкой к конкретному
// (сервер, целевой пользователь) для контекста в ошибках и логах.
type Client struct {
	c      *client.Client
	server string // host:port - для сообщений об ошибках
	user   string // целевой пользователь (authzid)

	closeOnce sync.Once
	closed    chan struct{}

	folders []*imap.MailboxInfo // кэш LIST на время жизни соединения
}

// Connect устанавливает TLS-соединение и авторизуется как targetUser через
// мастер-учётку сервера (SASL PLAIN: authcid = master_user, authzid = targetUser).
//
// ioTimeout - дедлайн на одну IMAP-операцию. Кроме того, при отмене ctx
// TCP-соединение принудительно закрывается (Terminate), что прерывает висящий
// вызов - go-imap v1 сам по себе не реагирует на context.
func Connect(ctx context.Context, srv config.Server, targetUser string, dialTimeout, ioTimeout time.Duration, insecureTLS bool) (*Client, error) {
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

	// Дедлайн на каждую последующую операцию ввода-вывода.
	imapCli.Timeout = ioTimeout

	// SASL PLAIN с authzid: identity(authzid)=целевой юзер, username(authcid)=мастер.
	auth := sasl.NewPlainClient(targetUser, srv.MasterUser, srv.MasterPass)
	if err := imapCli.Authenticate(auth); err != nil {
		_ = imapCli.Logout()
		return nil, fmt.Errorf("имперсонация %s на %s через мастера %s: %w", targetUser, addr, srv.MasterUser, err)
	}

	cl := &Client{c: imapCli, server: addr, user: targetUser, closed: make(chan struct{})}
	go cl.watch(ctx)
	return cl, nil
}

// watch закрывает соединение при отмене ctx, прерывая любой висящий вызов.
func (cl *Client) watch(ctx context.Context) {
	select {
	case <-ctx.Done():
		_ = cl.c.Terminate()
	case <-cl.closed:
	}
}

// Logout закрывает сессию. Ошибку логаута считаем некритичной.
func (cl *Client) Logout() {
	cl.closeOnce.Do(func() { close(cl.closed) })
	_ = cl.c.Logout()
}

// известные SPECIAL-USE атрибуты (RFC 6154).
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
		return nil, fmt.Errorf("LIST на %s (юзер %s): %w", cl.server, cl.user, err)
	}
	cl.folders = out
	return out, nil
}

// ResolveFolder приводит имя папки из конфига к реальному имени на сервере:
//  1. точное совпадение;
//  2. если name - это SPECIAL-USE токен (например "\Sent") - ищем папку с таким
//     атрибутом;
//  3. регистронезависимое совпадение.
//
// Если папка не найдена - ошибка со списком доступных папок.
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
	return "", fmt.Errorf("папка %q не найдена на %s (юзер %s); доступны: %s",
		name, cl.server, cl.user, strings.Join(avail, ", "))
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

var headerFetchItems = []imap.FetchItem{
	imap.FetchUid,
	imap.FetchFlags,
	imap.FetchInternalDate,
	imap.FetchRFC822Size,
	headerSection.FetchItem(),
}

// decodeHeaderMsg превращает *imap.Message в FetchedMessage, читая блок заголовков.
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
			return fm, fmt.Errorf("чтение заголовков uid=%d на %s (юзер %s): %w", msg.Uid, cl.server, cl.user, err)
		}
		fm.Header = raw
	}
	return fm, nil
}

// FetchHeaders забирает заголовки, флаги, INTERNALDATE, UID и размер всех писем
// папки батчами по batchSize (по порядковым номерам). Папка должна быть уже
// выбрана через Select.
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
			return nil, fmt.Errorf("FETCH заголовков %d:%d на %s (юзер %s): %w", from, to, cl.server, cl.user, err)
		}
	}
	return out, nil
}

// UIDSearchAll возвращает UID всех писем выбранной папки (SEARCH UID 1:*).
// Дёшево даже для крупных папок: один round-trip, ответ - список чисел.
func (cl *Client) UIDSearchAll() ([]uint32, error) {
	crit := imap.NewSearchCriteria()
	crit.Uid = new(imap.SeqSet)
	crit.Uid.AddRange(1, 0) // 1:*
	uids, err := cl.c.UidSearch(crit)
	if err != nil {
		return nil, fmt.Errorf("UID SEARCH на %s (юзер %s): %w", cl.server, cl.user, err)
	}
	return uids, nil
}

// FetchHeadersByUID забирает заголовки, флаги, INTERNALDATE и размер писем с
// указанными UID батчами по batchSize. Папка должна быть выбрана через Select.
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
			return nil, fmt.Errorf("FETCH заголовков по UID (%d шт.) на %s (юзер %s): %w", end-start, cl.server, cl.user, err)
		}
	}
	return out, nil
}

// FetchFullLiteral возвращает письмо целиком (заголовки + тело) по UID как
// imap.Literal. go-imap v1 всё равно буферизует литерал в памяти, но так мы не
// делаем поверх этого ещё одну копию (io.ReadAll).
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
		return nil, fmt.Errorf("FETCH тела uid=%d на %s (юзер %s): %w", uid, cl.server, cl.user, err)
	}
	if lit == nil {
		return nil, fmt.Errorf("письмо uid=%d не найдено на %s (юзер %s)", uid, cl.server, cl.user)
	}
	return lit, nil
}

// Append дописывает письмо в папку, сохраняя флаги и внутреннюю дату оригинала.
func (cl *Client) Append(folder string, flags []string, date time.Time, body []byte) error {
	_, err := cl.AppendLiteral(folder, flags, date, bytes.NewBuffer(body))
	return err
}

// AppendLiteral дописывает письмо (переданное как imap.Literal) и пытается
// вернуть присвоенный ему UID из ответа [APPENDUID] (UIDPLUS, RFC 4315). Если
// сервер его не поддерживает - uid == 0 и ошибки нет.
func (cl *Client) AppendLiteral(folder string, flags []string, date time.Time, msg imap.Literal) (uint32, error) {
	cmd := &commands.Append{Mailbox: folder, Flags: flags, Date: date, Message: msg}
	status, err := cl.c.Execute(cmd, nil)
	if err != nil {
		return 0, fmt.Errorf("APPEND в %q на %s (юзер %s): %w", folder, cl.server, cl.user, err)
	}
	if err := status.Err(); err != nil {
		return 0, fmt.Errorf("APPEND в %q на %s (юзер %s): %w", folder, cl.server, cl.user, err)
	}
	if status.Code == "APPENDUID" && len(status.Arguments) == 2 {
		if uid, err := imap.ParseNumber(status.Arguments[1]); err == nil {
			return uid, nil
		}
	}
	return 0, nil
}
