package endpoint

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"imapsync/config"
)

func TestWithHeaderPrefixesStreaming(t *testing.T) {
	body := "Subject: hi\r\nFrom: a@b\r\n\r\nтело письма"
	lit := WithHeader(bytes.NewBufferString(body), "X-Imapsync-Hash", "cafe")

	want := len("X-Imapsync-Hash: cafe\r\n") + len(body)
	if lit.Len() != want {
		t.Errorf("Len = %d, ожидали %d", lit.Len(), want)
	}
	got, _ := io.ReadAll(lit)
	if len(got) != want {
		t.Errorf("прочитано %d байт, Len обещал %d", len(got), want)
	}
	if !strings.HasPrefix(string(got), "X-Imapsync-Hash: cafe\r\nSubject: hi") {
		t.Errorf("не тот префикс: %q", got[:40])
	}
}

func TestWithHeaderIdempotent(t *testing.T) {
	orig := "X-Imapsync-Hash: old\r\nSubject: hi\r\n\r\nx"
	lit := WithHeader(bytes.NewBufferString(orig), "X-Imapsync-Hash", "new")
	got, _ := io.ReadAll(lit)
	if string(got) != orig {
		t.Errorf("литерал с существующим заголовком изменён: %q", got)
	}
}

func TestNewBackendRejectsUnknownType(t *testing.T) {
	if _, err := NewBackend(config.Server{Type: "ews", Host: "x"}, 0, 0, false, 10); err == nil {
		t.Error("ожидали ошибку для неизвестного типа эндпоинта")
	}
	if _, err := NewBackend(config.Server{Host: "x"}, 0, 0, false, 10); err != nil {
		t.Errorf("пустой тип должен трактоваться как imap: %v", err)
	}
}
