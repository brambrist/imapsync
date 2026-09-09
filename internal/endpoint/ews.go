package endpoint

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"imapsync/config"
)

// ewsBackend - the ews endpoint type (Exchange Web Services, SOAP over HTTPS).
// Impersonation: a service account with the ApplicationImpersonation role plus
// the SOAP header ExchangeImpersonation. Authentication: HTTP Basic.
type ewsBackend struct {
	url         string
	user        string
	pass        string
	insecureTLS bool
	timeout     time.Duration
	batch       int
}

func newEWSBackend(srv config.Server, ioTimeout time.Duration, insecureTLS bool, batch int) *ewsBackend {
	url := strings.TrimSpace(srv.EWSUrl)
	if url == "" {
		url = "https://" + srv.Host + "/EWS/Exchange.asmx"
	}
	if batch < 1 {
		batch = 50
	}
	return &ewsBackend{url: url, user: srv.MasterUser, pass: srv.MasterPass, insecureTLS: insecureTLS, timeout: ioTimeout, batch: batch}
}

func (b *ewsBackend) Addr() string { return b.url }

func (b *ewsBackend) Connect(_ context.Context, user string) (Endpoint, error) {
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: b.insecureTLS}, //nolint:gosec
	}
	cl := &ewsClient{
		http:        &http.Client{Transport: tr, Timeout: b.timeout},
		url:         b.url,
		user:        b.user,
		pass:        b.pass,
		impersonate: user,
	}
	return &ewsEndpoint{cl: cl, batch: b.batch}, nil
}

// --- SOAP client ---

type ewsClient struct {
	http        *http.Client
	url         string
	user        string
	pass        string
	impersonate string
}

const soapEnvelope = `<?xml version="1.0" encoding="utf-8"?>
<soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:t="http://schemas.microsoft.com/exchange/services/2006/types" xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages">
<soap:Header><t:RequestServerVersion Version="Exchange2013"/>%s</soap:Header>
<soap:Body>%s</soap:Body>
</soap:Envelope>`

// call wraps body in a SOAP envelope (with ExchangeImpersonation) and POSTs it.
func (c *ewsClient) call(ctx context.Context, body string) ([]byte, error) {
	imp := ""
	if c.impersonate != "" {
		imp = fmt.Sprintf(`<t:ExchangeImpersonation><t:ConnectingSID><t:PrimarySmtpAddress>%s</t:PrimarySmtpAddress></t:ConnectingSID></t:ExchangeImpersonation>`, xesc(c.impersonate))
	}
	env := fmt.Sprintf(soapEnvelope, imp, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, strings.NewReader(env))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "text/xml; charset=utf-8")
	req.SetBasicAuth(c.user, c.pass)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("EWS %s: %w", c.url, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("EWS %s: HTTP %d: %s", c.url, resp.StatusCode, snippet(data))
	}
	return data, nil
}

func xesc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// --- response parsing ---

type respMsg struct {
	ResponseClass string `xml:"ResponseClass,attr"`
	ResponseCode  string `xml:"ResponseCode"`
	MessageText   string `xml:"MessageText"`
}

func (r respMsg) err(op string) error {
	if r.ResponseCode != "" && r.ResponseCode != "NoError" {
		return fmt.Errorf("EWS %s: %s: %s", op, r.ResponseCode, r.MessageText)
	}
	if r.ResponseClass == "Error" {
		return fmt.Errorf("EWS %s: %s", op, r.MessageText)
	}
	return nil
}

type itemIDAttr struct {
	ID        string `xml:"Id,attr"`
	ChangeKey string `xml:"ChangeKey,attr"`
}

// --- Endpoint ---

type ewsEndpoint struct {
	cl        *ewsClient
	batch     int
	folderXML string // <t:DistinguishedFolderId .../> or <t:FolderId .../>
	folder    string
}

var ewsDistinguished = map[string]string{
	"": "inbox", "inbox": "inbox",
	"sent": "sentitems", "sent items": "sentitems", `\sent`: "sentitems",
	"drafts": "drafts", `\drafts`: "drafts",
	"deleted items": "deleteditems", "trash": "deleteditems", `\trash`: "deleteditems",
	"junk email": "junkemail", `\junk`: "junkemail",
	"archive": "archive", `\archive`: "archive",
	"outbox": "outbox",
}

func (e *ewsEndpoint) Select(name string) (string, string, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if d, ok := ewsDistinguished[key]; ok {
		e.folderXML = fmt.Sprintf(`<t:DistinguishedFolderId Id="%s"/>`, d)
		e.folder = name
		if name == "" {
			e.folder = "INBOX"
		}
		return e.folder, "ews", nil
	}

	id, disp, err := e.findFolder(context.Background(), name)
	if err != nil {
		return "", "", err
	}
	e.folderXML = fmt.Sprintf(`<t:FolderId Id="%s"/>`, xesc(id))
	e.folder = disp
	return disp, "ews", nil
}

func (e *ewsEndpoint) findFolder(ctx context.Context, name string) (id, disp string, err error) {
	body := `<m:FindFolder Traversal="Deep"><m:FolderShape><t:BaseShape>Default</t:BaseShape></m:FolderShape>` +
		`<m:ParentFolderIds><t:DistinguishedFolderId Id="msgfolderroot"/></m:ParentFolderIds></m:FindFolder>`
	data, err := e.cl.call(ctx, body)
	if err != nil {
		return "", "", err
	}
	var out struct {
		Msg struct {
			respMsg
			Folders struct {
				Entries []struct {
					FolderID    itemIDAttr `xml:"FolderId"`
					DisplayName string     `xml:"DisplayName"`
				} `xml:",any"`
			} `xml:"RootFolder>Folders"`
		} `xml:"Body>FindFolderResponse>ResponseMessages>FindFolderResponseMessage"`
	}
	if err := xml.Unmarshal(data, &out); err != nil {
		return "", "", fmt.Errorf("EWS FindFolder: parsing response: %w", err)
	}
	if err := out.Msg.err("FindFolder"); err != nil {
		return "", "", err
	}
	var ci string
	for _, f := range out.Msg.Folders.Entries {
		if f.DisplayName == name {
			return f.FolderID.ID, f.DisplayName, nil
		}
		if strings.EqualFold(f.DisplayName, name) {
			ci = f.FolderID.ID
			disp = f.DisplayName
		}
	}
	if ci != "" {
		return ci, disp, nil
	}
	return "", "", fmt.Errorf("EWS: folder %q not found", name)
}

func (e *ewsEndpoint) ListIDs() ([]string, error) {
	ctx := context.Background()
	var ids []string
	for offset := 0; ; offset += e.batch {
		body := fmt.Sprintf(
			`<m:FindItem Traversal="Shallow"><m:ItemShape><t:BaseShape>IdOnly</t:BaseShape></m:ItemShape>`+
				`<m:IndexedPageItemView MaxEntriesReturned="%d" Offset="%d" BasePoint="Beginning"/>`+
				`<m:ParentFolderIds>%s</m:ParentFolderIds></m:FindItem>`, e.batch, offset, e.folderXML)
		data, err := e.cl.call(ctx, body)
		if err != nil {
			return nil, err
		}
		var out struct {
			Msg struct {
				respMsg
				Root struct {
					Last  bool `xml:"IncludesLastItemInRange,attr"`
					Items struct {
						Entries []struct {
							ItemID itemIDAttr `xml:"ItemId"`
						} `xml:",any"`
					} `xml:"Items"`
				} `xml:"RootFolder"`
			} `xml:"Body>FindItemResponse>ResponseMessages>FindItemResponseMessage"`
		}
		if err := xml.Unmarshal(data, &out); err != nil {
			return nil, fmt.Errorf("EWS FindItem: parsing response: %w", err)
		}
		if err := out.Msg.err("FindItem"); err != nil {
			return nil, err
		}
		for _, it := range out.Msg.Root.Items.Entries {
			if it.ItemID.ID != "" {
				ids = append(ids, it.ItemID.ID)
			}
		}
		if out.Msg.Root.Last || len(out.Msg.Root.Items.Entries) == 0 {
			break
		}
	}
	return ids, nil
}

type ewsItem struct {
	ItemID           itemIDAttr `xml:"ItemId"`
	Subject          string     `xml:"Subject"`
	DateTimeReceived string     `xml:"DateTimeReceived"`
	Size             uint32     `xml:"Size"`
	IsRead           string     `xml:"IsRead"`
	IsDraft          string     `xml:"IsDraft"`
	InternetMsgID    string     `xml:"InternetMessageId"`
	MimeContent      string     `xml:"MimeContent"`
	Headers          struct {
		H []struct {
			Name  string `xml:"HeaderName,attr"`
			Value string `xml:",chardata"`
		} `xml:"InternetMessageHeader"`
	} `xml:"InternetMessageHeaders"`
}

func (e *ewsEndpoint) getItems(ctx context.Context, ids []string, mime bool) ([]ewsItem, error) {
	var shape string
	if mime {
		shape = `<t:BaseShape>IdOnly</t:BaseShape><t:IncludeMimeContent>true</t:IncludeMimeContent>`
	} else {
		shape = `<t:BaseShape>IdOnly</t:BaseShape><t:AdditionalProperties>` +
			`<t:FieldURI FieldURI="item:InternetMessageHeaders"/>` +
			`<t:FieldURI FieldURI="item:DateTimeReceived"/>` +
			`<t:FieldURI FieldURI="item:Size"/>` +
			`<t:FieldURI FieldURI="message:IsRead"/>` +
			`<t:FieldURI FieldURI="item:Subject"/>` +
			`<t:FieldURI FieldURI="message:InternetMessageId"/>` +
			`</t:AdditionalProperties>`
	}
	var b strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&b, `<t:ItemId Id="%s"/>`, xesc(id))
	}
	body := fmt.Sprintf(`<m:GetItem><m:ItemShape>%s</m:ItemShape><m:ItemIds>%s</m:ItemIds></m:GetItem>`, shape, b.String())

	data, err := e.cl.call(ctx, body)
	if err != nil {
		return nil, err
	}
	var out struct {
		Msgs []struct {
			respMsg
			Items struct {
				Entries []ewsItem `xml:",any"`
			} `xml:"Items"`
		} `xml:"Body>GetItemResponse>ResponseMessages>GetItemResponseMessage"`
	}
	if err := xml.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("EWS GetItem: parsing response: %w", err)
	}
	var items []ewsItem
	for _, m := range out.Msgs {
		if err := m.err("GetItem"); err != nil {
			return nil, err
		}
		items = append(items, m.Items.Entries...)
	}
	return items, nil
}

func (e *ewsEndpoint) FetchMeta(ids []string) ([]Message, error) {
	if ids == nil {
		var err error
		if ids, err = e.ListIDs(); err != nil {
			return nil, err
		}
	}
	ctx := context.Background()
	out := make([]Message, 0, len(ids))
	for start := 0; start < len(ids); start += e.batch {
		end := min(start+e.batch, len(ids))
		items, err := e.getItems(ctx, ids[start:end], false)
		if err != nil {
			return nil, err
		}
		for _, it := range items {
			out = append(out, Message{
				ID:           it.ItemID.ID,
				Flags:        ewsFlags(it),
				InternalDate: parseEWSTime(it.DateTimeReceived),
				Size:         it.Size,
				Header:       ewsHeaderBlock(it),
			})
		}
	}
	return out, nil
}

func (e *ewsEndpoint) Open(id string) (Literal, error) {
	items, err := e.getItems(context.Background(), []string{id}, true)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 || items[0].MimeContent == "" {
		return nil, fmt.Errorf("EWS: message %q has no MIME content", id)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(items[0].MimeContent))
	if err != nil {
		return nil, fmt.Errorf("EWS: decoding MIME of message %q: %w", id, err)
	}
	return bytes.NewBuffer(raw), nil
}

func (e *ewsEndpoint) Append(flags []string, _ time.Time, body Literal) (string, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	b64 := base64.StdEncoding.EncodeToString(raw)

	isRead := "false"
	for _, f := range flags {
		if f == `\Seen` {
			isRead = "true"
		}
	}
	xmlBody := fmt.Sprintf(
		`<m:CreateItem MessageDisposition="SaveOnly"><m:SavedItemFolderId>%s</m:SavedItemFolderId>`+
			`<m:Items><t:Message><t:MimeContent CharacterSet="UTF-8">%s</t:MimeContent>`+
			`<t:IsRead>%s</t:IsRead></t:Message></m:Items></m:CreateItem>`,
		e.folderXML, b64, isRead)

	data, err := e.cl.call(context.Background(), xmlBody)
	if err != nil {
		return "", err
	}
	var out struct {
		Msg struct {
			respMsg
			Items struct {
				Entries []struct {
					ItemID itemIDAttr `xml:"ItemId"`
				} `xml:",any"`
			} `xml:"Items"`
		} `xml:"Body>CreateItemResponse>ResponseMessages>CreateItemResponseMessage"`
	}
	if err := xml.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("EWS CreateItem: parsing response: %w", err)
	}
	if err := out.Msg.err("CreateItem"); err != nil {
		return "", err
	}
	if len(out.Msg.Items.Entries) > 0 {
		return out.Msg.Items.Entries[0].ItemID.ID, nil
	}
	return "", nil
}

func (e *ewsEndpoint) Close() {}

// --- helpers ---

func ewsFlags(it ewsItem) []string {
	var out []string
	if strings.EqualFold(it.IsRead, "true") {
		out = append(out, `\Seen`)
	}
	if strings.EqualFold(it.IsDraft, "true") {
		out = append(out, `\Draft`)
	}
	return out
}

// ewsHeaderBlock assembles a raw RFC 822 header block from InternetMessageHeaders.
func ewsHeaderBlock(it ewsItem) []byte {
	var b strings.Builder
	seenMsgID, seenSubject := false, false
	for _, h := range it.Headers.H {
		name := strings.TrimSpace(h.Name)
		if strings.EqualFold(name, "Message-ID") {
			seenMsgID = true
		}
		if strings.EqualFold(name, "Subject") {
			seenSubject = true
		}
		fmt.Fprintf(&b, "%s: %s\r\n", name, oneLine(h.Value))
	}
	if !seenMsgID && it.InternetMsgID != "" {
		fmt.Fprintf(&b, "Message-ID: %s\r\n", it.InternetMsgID)
	}
	if !seenSubject && it.Subject != "" {
		fmt.Fprintf(&b, "Subject: %s\r\n", oneLine(it.Subject))
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

func oneLine(s string) string {
	return strings.NewReplacer("\r", " ", "\n", " ").Replace(strings.TrimSpace(s))
}

func parseEWSTime(s string) time.Time {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05Z", "2006-01-02T15:04:05"} {
		if t, err := time.Parse(layout, strings.TrimSpace(s)); err == nil {
			return t
		}
	}
	return time.Time{}
}
