// Package endpoint - абстракция «конец синхронизации». Синкер работает только
// через эти интерфейсы и не знает, IMAP это, Maildir или EWS. Сейчас
// реализован только IMAP (imap.go).
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

// Message - метаданные письма для построения индекса дедупликации.
type Message struct {
	ID           string // UID (IMAP) / имя файла (Maildir) / ItemId (EWS) - непрозрачная строка
	Flags        []string
	InternalDate time.Time
	Size         uint32
	Header       []byte // сырой блок заголовков RFC 822
}

// Literal - тело письма с известной длиной (для Append).
type Literal = imap.Literal

// Endpoint - открытая сессия одной стороны для одного пользователя. В каждый
// момент выбрана не более одной папки (Select переключает).
type Endpoint interface {
	// Select резолвит имя папки (точное / SPECIAL-USE токен "\Sent" /
	// регистронезависимо) и делает её текущей. Возвращает реальное имя и токен
	// валидности: при его изменении все ранее полученные ID считаются невалидными.
	Select(name string) (folder, validity string, err error)

	// ListIDs - ID всех писем в текущей папке.
	ListIDs() ([]string, error)

	// FetchMeta - метаданные писем по ID. ids == nil означает «все письма папки».
	FetchMeta(ids []string) ([]Message, error)

	// Open - тело письма целиком по ID.
	Open(id string) (Literal, error)

	// Append дописывает письмо в текущую папку. Возвращает ID нового письма
	// ("" если транспорт его не сообщает - тогда оно подхватится в следующем цикле).
	Append(flags []string, date time.Time, body Literal) (string, error)

	// Close закрывает сессию.
	Close()
}

// Backend - фабрика сессий одной стороны (сервер + тип транспорта).
type Backend interface {
	// Connect устанавливает сессию для пользователя user (authzid при IMAP).
	Connect(ctx context.Context, user string) (Endpoint, error)
	// Addr - человекочитаемый адрес для логов.
	Addr() string
}

// NewBackend строит backend по конфигу сервера.
func NewBackend(srv config.Server, dialTimeout, ioTimeout time.Duration, insecureTLS bool, fetchBatch int) (Backend, error) {
	switch srv.Type {
	case "", config.EndpointIMAP:
		return newIMAPBackend(srv, dialTimeout, ioTimeout, insecureTLS, fetchBatch), nil
	case config.EndpointMaildir:
		return newMaildirBackend(srv), nil
	case config.EndpointEWS:
		return newEWSBackend(srv, ioTimeout, insecureTLS, fetchBatch), nil
	default:
		return nil, fmt.Errorf("тип эндпоинта %q не поддерживается (%q, %q, %q)",
			srv.Type, config.EndpointIMAP, config.EndpointMaildir, config.EndpointEWS)
	}
}

// prefixedLiteral - Literal из «префикс + тело» без копирования тела.
type prefixedLiteral struct {
	r      io.Reader
	length int
}

func (p *prefixedLiteral) Read(b []byte) (int, error) { return p.r.Read(b) }
func (p *prefixedLiteral) Len() int                   { return p.length }

// WithHeader возвращает литерал письма с добавленным в начало заголовком
// name: value (без копирования тела). Если такой заголовок уже есть - литерал
// возвращается как есть.
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
