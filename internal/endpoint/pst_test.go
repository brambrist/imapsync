package endpoint

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/mailbox"
)

// testPST decompresses testdata/support.pst.gz into a temp file and returns its
// path. support.pst is the public sample archive bundled with go-pst (a
// Unicode/Mac Outlook PST with a handful of real messages).
func testPST(t *testing.T) string {
	t.Helper()
	f, err := os.Open("testdata/support.pst.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()

	path := filepath.Join(t.TempDir(), "support.pst")
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(out, gz); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func openPST(t *testing.T, path string) Endpoint {
	t.Helper()
	b, err := NewBackend(config.Server{Type: config.EndpointPST, Root: path}, 0, 0, false, 10, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	ep, err := b.Connect(context.Background(), "u@corp")
	if err != nil {
		t.Fatal(err)
	}
	return ep
}

func TestPSTSelectAndList(t *testing.T) {
	ep := openPST(t, testPST(t))
	defer ep.Close()

	// "Sent Messages" holds 11 real messages; "\Sent" resolves to a "Sent"
	// folder that is empty in this archive - the SPECIAL-USE candidate list
	// tries "Sent Items" / "Sent" / "Sent Messages" in order.
	folder, validity, err := ep.Select("Sent Messages")
	if err != nil {
		t.Fatal(err)
	}
	if folder != "Sent Messages" || validity != "pst" {
		t.Fatalf("select: folder=%q validity=%q", folder, validity)
	}
	ids, err := ep.ListIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 11 {
		t.Fatalf("ListIDs = %d, want 11", len(ids))
	}
}

func TestPSTFolderResolution(t *testing.T) {
	ep := openPST(t, testPST(t))
	defer ep.Close()

	// Drafts by exact name.
	if f, _, err := ep.Select("Drafts"); err != nil || f != "Drafts" {
		t.Fatalf("Drafts: %q %v", f, err)
	}
	// A missing folder reports the available names.
	_, _, err := ep.Select("No Such Folder")
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected a not-found error, got %v", err)
	}
}

func TestPSTFetchMetaAndOpen(t *testing.T) {
	ep := openPST(t, testPST(t))
	defer ep.Close()
	if _, _, err := ep.Select("Sent Messages"); err != nil {
		t.Fatal(err)
	}

	metas, err := ep.FetchMeta(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 11 {
		t.Fatalf("FetchMeta = %d, want 11", len(metas))
	}

	for _, m := range metas {
		f, err := mailbox.ParseFields(m.Header, "X-Imapsync-Hash")
		if err != nil {
			t.Fatalf("id=%s: parse header: %v", m.ID, err)
		}
		if f.Subject == "" {
			t.Errorf("id=%s: empty subject", m.ID)
		}
		if m.InternalDate.IsZero() {
			t.Errorf("id=%s: zero internal date", m.ID)
		}

		lit, err := ep.Open(m.ID)
		if err != nil {
			t.Fatalf("id=%s: open: %v", m.ID, err)
		}
		raw, _ := io.ReadAll(lit.(*bytes.Buffer))
		if !bytes.Contains(raw, []byte("\r\n\r\n")) {
			t.Errorf("id=%s: no header/body separator", m.ID)
		}
		if !bytes.Contains(bytes.ToLower(raw), []byte("content-type:")) {
			t.Errorf("id=%s: no Content-Type", m.ID)
		}
	}
}

func TestPSTIsReadOnly(t *testing.T) {
	ep := openPST(t, testPST(t))
	defer ep.Close()
	if !ep.ReadOnly() {
		t.Fatal("PST endpoint must report ReadOnly() == true")
	}
	if _, _, err := ep.Select("Drafts"); err != nil {
		t.Fatal(err)
	}
	if _, err := ep.Append(nil, time.Time{}, bytes.NewBufferString("x")); err == nil {
		t.Fatal("Append must fail on a read-only PST")
	}
}
