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

// maildirBackend - the maildir endpoint type. No network and no auth: Connect
// only expands the path template to the user's Maildir.
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
		return nil, fmt.Errorf("maildir %s (user %s): %w", root, user, err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("maildir %s (user %s): not a directory", root, user)
	}
	return &maildirEndpoint{root: root, user: user}, nil
}

// expandUser expands %u/%n/%d in the path template.
func expandUser(tmpl, user string) string {
	local, domain := user, ""
	if i := strings.LastIndexByte(user, '@'); i >= 0 {
		local, domain = user[:i], user[i+1:]
	}
	r := strings.NewReplacer("%u", user, "%n", local, "%d", domain)
	return r.Replace(tmpl)
}

// maildirEndpoint - an open Maildir with one selected folder.
type maildirEndpoint struct {
	root   string
	user   string
	folder string            // absolute path of the selected folder (with tmp/new/cur inside)
	files  map[string]string // message ID -> absolute file path
}

// SPECIAL-USE tokens -> conventional Maildir++ subfolder name.
var maildirSpecial = map[string]string{
	`\Sent`: "Sent", `\Drafts`: "Drafts", `\Trash`: "Trash",
	`\Junk`: "Junk", `\Archive`: "Archive",
}

// mapping of Maildir flag letters (":2," info) <-> IMAP flags.
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
	// case-insensitive among existing .subfolders
	if ents, err := os.ReadDir(e.root); err == nil {
		for _, ent := range ents {
			if ent.IsDir() && strings.HasPrefix(ent.Name(), ".") &&
				strings.EqualFold(ent.Name()[1:], n) {
				return filepath.Join(e.root, ent.Name())
			}
		}
	}
	return exact // does not exist - will be created in Select
}

func (e *maildirEndpoint) Select(name string) (string, string, error) {
	path := e.folderPath(name)
	for _, sub := range []string{"tmp", "new", "cur"} {
		if err := os.MkdirAll(filepath.Join(path, sub), 0o700); err != nil {
			return "", "", fmt.Errorf("maildir %s: creating %s: %w", path, sub, err)
		}
	}
	e.folder = path
	if err := e.scan(); err != nil {
		return "", "", err
	}
	// Folder name in IMAP terms: INBOX for the root, otherwise the name without
	// the leading dot.
	folder := "INBOX"
	if path != e.root {
		folder = strings.TrimPrefix(filepath.Base(path), ".")
	}
	// Maildir has no validity: IDs (the unique part of the filename) are stable
	// per spec, and drift is self-healed by the diff in ListIDs. Constant.
	return folder, "maildir", nil
}

// scan builds the ID -> path index from new/ and cur/.
func (e *maildirEndpoint) scan() error {
	e.files = map[string]string{}
	for _, sub := range []string{"new", "cur"} {
		dir := filepath.Join(e.folder, sub)
		ents, err := os.ReadDir(dir)
		if err != nil {
			return fmt.Errorf("maildir %s: reading %s: %w", e.folder, sub, err)
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

// uniqueID - the part of the filename before the info suffix (":2,..."). Stable
// per the Maildir spec.
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
			continue // message vanished between scan and fetch
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
		return nil, fmt.Errorf("maildir: message %q not found in %s", id, e.folder)
	}
	// Maildir messages are local and usually small - read the whole file (1x
	// copy), which also gives Open a *bytes.Buffer like the IMAP path (needed
	// for WithHeader).
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("maildir reading %s: %w", path, err)
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
		return "", fmt.Errorf("maildir creating tmp %s: %w", tmp, err)
	}
	if _, err := io.Copy(f, body); err != nil {
		f.Close()
		os.Remove(tmp)
		return "", fmt.Errorf("maildir writing %s: %w", tmp, err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("maildir closing %s: %w", tmp, err)
	}
	if !date.IsZero() {
		_ = os.Chtimes(tmp, date, date)
	}

	dst := filepath.Join(e.folder, sub, final)
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		return "", fmt.Errorf("maildir moving to %s: %w", sub, err)
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

// flagsFromName parses the ":2,FRS" info suffix into IMAP flags.
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

// flagSuffix builds ":2,<letters>" from IMAP flags (letters sorted per spec).
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

// readHeaderBlock reads the file up to the end of the header block (blank line).
func readHeaderBlock(path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("maildir reading %s: %w", path, err)
	}
	if i := bytes.Index(data, []byte("\r\n\r\n")); i >= 0 {
		return data[:i+4], nil
	}
	if i := bytes.Index(data, []byte("\n\n")); i >= 0 {
		return data[:i+2], nil
	}
	return data, nil
}
