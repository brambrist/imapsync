package endpoint

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"imapsync/config"
)

// maildirBackend - конец типа maildir. Сети и авторизации нет: Connect только
// раскрывает шаблон пути к Maildir пользователя.
type maildirBackend struct {
	rootTmpl string
}

func newMaildirBackend(srv config.Server) *maildirBackend {
	return &maildirBackend{rootTmpl: srv.Root}
}

func (b *maildirBackend) Addr() string { return "maildir:" + b.rootTmpl }

func (b *maildirBackend) Connect(_ context.Context, user string) (Endpoint, error) {
	root := expandUser(b.rootTmpl, user)
	fi, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("maildir %s (юзер %s): %w", root, user, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("maildir %s (юзер %s): не каталог", root, user)
	}
	return &maildirEndpoint{root: root, user: user}, nil
}

// expandUser раскрывает %u/%n/%d в шаблоне пути.
func expandUser(tmpl, user string) string {
	local, domain := user, ""
	if i := strings.LastIndexByte(user, '@'); i >= 0 {
		local, domain = user[:i], user[i+1:]
	}
	r := strings.NewReplacer("%u", user, "%n", local, "%d", domain)
	return r.Replace(tmpl)
}

// maildirEndpoint - открытый Maildir с одной выбранной папкой.
type maildirEndpoint struct {
	root   string
	user   string
	folder string            // абсолютный путь выбранной папки (с tmp/new/cur внутри)
	files  map[string]string // ID письма -> абсолютный путь к файлу
}

// SPECIAL-USE токены -> conventional имя подпапки Maildir++.
var maildirSpecial = map[string]string{
	`\Sent`: "Sent", `\Drafts`: "Drafts", `\Trash`: "Trash",
	`\Junk`: "Junk", `\Archive`: "Archive",
}

// маппинг букв флагов Maildir (info ":2,") <-> IMAP-флаги.
var (
	flagLetterToIMAP = map[byte]string{'S': `\Seen`, 'R': `\Answered`, 'F': `\Flagged`, 'D': `\Draft`, 'T': `\Deleted`}
	imapToFlagLetter = map[string]byte{`\Seen`: 'S', `\Answered`: 'R', `\Flagged`: 'F', `\Draft`: 'D', `\Deleted`: 'T'}
)

func (e *maildirEndpoint) folderPath(name string) string {
	n := strings.TrimSpace(name)
	if n == "" || strings.EqualFold(n, "INBOX") {
		return e.root
	}
	if sub, ok := maildirSpecial[n]; ok {
		n = sub
	}
	n = strings.Trim(strings.ReplaceAll(n, "/", "."), ".")

	exact := filepath.Join(e.root, "."+n)
	if isDir(exact) {
		return exact
	}
	// регистронезависимо среди существующих .подпапок
	if ents, err := os.ReadDir(e.root); err == nil {
		for _, ent := range ents {
			if ent.IsDir() && strings.HasPrefix(ent.Name(), ".") &&
				strings.EqualFold(ent.Name()[1:], n) {
				return filepath.Join(e.root, ent.Name())
			}
		}
	}
	return exact // не существует - создадим в Select
}

func (e *maildirEndpoint) Select(name string) (string, string, error) {
	path := e.folderPath(name)
	for _, sub := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(path, sub), 0o700); err != nil {
			return "", "", fmt.Errorf("maildir %s: создание %s: %w", path, sub, err)
		}
	}
	e.folder = path
	if err := e.scan(); err != nil {
		return "", "", err
	}
	// Имя папки как в IMAP-терминах: INBOX для корня, иначе имя без ведущей точки.
	folder := "INBOX"
	if path != e.root {
		folder = strings.TrimPrefix(filepath.Base(path), ".")
	}
	// Валидность у Maildir нет: ID (unique-часть имени файла) стабильны по спеке,
	// а расхождения самолечит diff в ListIDs. Константа.
	return folder, "maildir", nil
}

// scan строит индекс ID -> путь по new/ и cur/.
func (e *maildirEndpoint) scan() error {
	e.files = map[string]string{}
	for _, sub := range []string{"new", "cur"} {
		dir := filepath.Join(e.folder, sub)
		ents, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("maildir %s: чтение %s: %w", e.folder, sub, err)
		}
		for _, ent := range ents {
			if ent.IsDir() || strings.HasPrefix(ent.Name(), ".") {
				continue
			}
			e.files[uniqueID(ent.Name())] = filepath.Join(dir, ent.Name())
		}
	}
	return nil
}

// uniqueID - часть имени файла до info-суффикса (":2,..."). По спеке стабильна.
func uniqueID(filename string) string {
	if i := strings.IndexByte(filename, ':'); i >= 0 {
		return filename[:i]
	}
	return filename
}

func (e *maildirEndpoint) ListIDs() ([]string, error) {
	ids := make([]string, 0, len(e.files))
	for id := range e.files {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, nil
}

func (e *maildirEndpoint) FetchMeta(ids []string) ([]Message, error) {
	if ids == nil {
		ids = make([]string, 0, len(e.files))
		for id := range e.files {
			ids = append(ids, id)
		}
	}
	out := make([]Message, 0, len(ids))
	for _, id := range ids {
		path, ok := e.files[id]
		if !ok {
			continue // письмо исчезло между scan и fetch
		}
		fi, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("maildir stat %s: %w", path, err)
		}
		hdr, err := readHeaderBlock(path)
		if err != nil {
			return nil, err
		}
		out = append(out, Message{
			ID:           id,
			Flags:        flagsFromName(filepath.Base(path)),
			InternalDate: fi.ModTime(),
			Size:         uint32(fi.Size()),
			Header:       hdr,
		})
	}
	return out, nil
}

func (e *maildirEndpoint) Open(id string) (Literal, error) {
	path, ok := e.files[id]
	if !ok {
		return nil, fmt.Errorf("maildir: письмо %q не найдено в %s", id, e.folder)
	}
	// Maildir-письма локальны и обычно небольшие - читаем целиком (1x копия),
	// зато Open даёт *bytes.Buffer, как и IMAP-путь (нужно для WithHeader).
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("maildir чтение %s: %w", path, err)
	}
	return bytes.NewBuffer(data), nil
}

var maildirCounter atomic.Uint64

func (e *maildirEndpoint) Append(flags []string, date time.Time, body Literal) (string, error) {
	host, _ := os.Hostname()
	host = strings.NewReplacer("/", "_", ":", "_").Replace(host)
	unique := fmt.Sprintf("%d.%d_%d.%s", time.Now().Unix(), os.Getpid(), maildirCounter.Add(1), host)

	suffix := flagSuffix(flags)
	sub := "new"
	final := unique
	if suffix != "" {
		sub = "cur"
		final = unique + suffix
	}

	tmp := filepath.Join(e.folder, "tmp", unique)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("maildir создание tmp %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("maildir запись %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("maildir закрытие %s: %w", tmp, err)
	}
	if !date.IsZero() {
		_ = os.Chtimes(tmp, date, date)
	}

	dst := filepath.Join(e.folder, sub, final)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("maildir перемещение в %s: %w", sub, err)
	}
	e.files[unique] = dst
	return unique, nil
}

func (e *maildirEndpoint) Close() {}

// --- helpers ---

func isDir(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && fi.IsDir()
}

// flagsFromName разбирает info-суффикс ":2,FRS" в IMAP-флаги.
func flagsFromName(filename string) []string {
	i := strings.Index(filename, ":2,")
	if i < 0 {
		return nil
	}
	var out []string
	for _, c := range []byte(filename[i+3:]) {
		if f, ok := flagLetterToIMAP[c]; ok {
			out = append(out, f)
		}
	}
	return out
}

// flagSuffix строит ":2,<буквы>" из IMAP-флагов (буквы сортированы по спеке).
func flagSuffix(flags []string) string {
	var letters []byte
	for _, fl := range flags {
		if l, ok := imapToFlagLetter[fl]; ok {
			letters = append(letters, l)
		}
	}
	if len(letters) == 0 {
		return ""
	}
	sort.Slice(letters, func(i, j int) bool { return letters[i] < letters[j] })
	return ":2," + string(letters)
}

// readHeaderBlock читает файл до конца блока заголовков (пустой строки).
func readHeaderBlock(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("maildir чтение %s: %w", path, err)
	}
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		return data[:i+4], nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return data[:i+2], nil
	}
	return data, nil
}
