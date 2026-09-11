// Package ewstest is a minimal in-memory EWS server for tests
// (FindFolder / FindItem / GetItem / CreateItem). Not for production.
package ewstest

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"regexp"
	"strings"
	"sync"
	"testing"
)

// Server is a fake EWS with an in-memory message store.
type Server struct {
	URL string

	mu      sync.Mutex
	items   map[string][]byte
	next    int
	LastImp string // last ExchangeImpersonation
}

var (
	idAttrRe = regexp.MustCompile(`Id="([^"]+)"`)
	impRe    = regexp.MustCompile(`<t:PrimarySmtpAddress>([^<]+)</t:PrimarySmtpAddress>`)
)

// New starts a fake server and registers its shutdown via t.Cleanup.
// seed - the initial messages (id -> raw RFC822).
func New(t *testing.T, seed map[string]string) *Server {
	t.Helper()
	s := newServer(seed)
	httpSrv := httptest.NewServer(http.HandlerFunc(s.handle))
	t.Cleanup(httpSrv.Close)
	s.URL = httpSrv.URL
	return s
}

// NewTLS is like New but serves HTTPS with a self-signed cert, restricted to
// [tlsCfg.MinVersion, tlsCfg.MaxVersion] - for testing min_tls_version /
// max_tls_version against a "legacy" server (e.g. TLS 1.0/1.1 only, like an
// unpatched Exchange 2013). The client must use InsecureSkipVerify (the cert is
// self-signed and not in any trust store).
func NewTLS(t *testing.T, seed map[string]string, tlsCfg *tls.Config) *Server {
	t.Helper()
	s := newServer(seed)
	httpSrv := httptest.NewUnstartedServer(http.HandlerFunc(s.handle))
	httpSrv.TLS = tlsCfg
	httpSrv.StartTLS()
	t.Cleanup(httpSrv.Close)
	s.URL = httpSrv.URL
	return s
}

func newServer(seed map[string]string) *Server {
	s := &Server{items: map[string][]byte{}}
	for id, b := range seed {
		s.items[id] = []byte(b)
	}
	return s
}

// Count is the number of messages on the server.
func (s *Server) Count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

// Add stores a message directly (simulating external delivery).
func (s *Server) Add(id, raw string) {
	s.mu.Lock()
	s.items[id] = []byte(raw)
	s.mu.Unlock()
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := r.BasicAuth(); !ok {
		http.Error(w, "no auth", http.StatusUnauthorized)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	body := string(raw)
	if m := impRe.FindStringSubmatch(body); m != nil {
		s.mu.Lock()
		s.LastImp = m[1]
		s.mu.Unlock()
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")

	switch {
	case strings.Contains(body, "FindFolder"):
		io.WriteString(w, wrap(`<m:FindFolderResponse><m:ResponseMessages><m:FindFolderResponseMessage ResponseClass="Success">
<m:ResponseCode>NoError</m:ResponseCode><m:RootFolder><t:Folders>
<t:Folder><t:FolderId Id="cust-1" ChangeKey="ck"/><t:DisplayName>Custom</t:DisplayName></t:Folder>
</t:Folders></m:RootFolder></m:FindFolderResponseMessage></m:ResponseMessages></m:FindFolderResponse>`))

	case strings.Contains(body, "FindItem"):
		s.mu.Lock()
		var its strings.Builder
		for id := range s.items {
			fmt.Fprintf(&its, `<t:Message><t:ItemId Id="%s" ChangeKey="ck"/></t:Message>`, esc(id))
		}
		s.mu.Unlock()
		io.WriteString(w, wrap(fmt.Sprintf(`<m:FindItemResponse><m:ResponseMessages><m:FindItemResponseMessage ResponseClass="Success">
<m:ResponseCode>NoError</m:ResponseCode><m:RootFolder IncludesLastItemInRange="true"><t:Items>%s</t:Items></m:RootFolder>
</m:FindItemResponseMessage></m:ResponseMessages></m:FindItemResponse>`, its.String())))

	case strings.Contains(body, "CreateItem"):
		data, _ := base64.StdEncoding.DecodeString(tagText(body, "MimeContent"))
		s.mu.Lock()
		s.next++
		id := fmt.Sprintf("AAMk-%d==", s.next) // looks like a real EWS ItemId (base64-ish)
		s.items[id] = data
		s.mu.Unlock()
		io.WriteString(w, wrap(fmt.Sprintf(`<m:CreateItemResponse><m:ResponseMessages><m:CreateItemResponseMessage ResponseClass="Success">
<m:ResponseCode>NoError</m:ResponseCode><m:Items><t:Message><t:ItemId Id="%s" ChangeKey="ck"/></t:Message></m:Items>
</m:CreateItemResponseMessage></m:ResponseMessages></m:CreateItemResponse>`, esc(id))))

	case strings.Contains(body, "GetItem"):
		mime := strings.Contains(body, "IncludeMimeContent")
		var msgs strings.Builder
		s.mu.Lock()
		for _, id := range reqIDs(body) {
			data, ok := s.items[id]
			if !ok {
				continue
			}
			if mime {
				fmt.Fprintf(&msgs, `<m:GetItemResponseMessage ResponseClass="Success"><m:ResponseCode>NoError</m:ResponseCode>
<m:Items><t:Message><t:ItemId Id="%s"/><t:MimeContent CharacterSet="UTF-8">%s</t:MimeContent></t:Message></m:Items>
</m:GetItemResponseMessage>`, esc(id), base64.StdEncoding.EncodeToString(data))
			} else {
				fmt.Fprintf(&msgs, `<m:GetItemResponseMessage ResponseClass="Success"><m:ResponseCode>NoError</m:ResponseCode>
<m:Items><t:Message><t:ItemId Id="%s"/><t:Size>%d</t:Size>%s<t:InternetMessageHeaders>%s</t:InternetMessageHeaders>
</t:Message></m:Items></m:GetItemResponseMessage>`, esc(id), len(data), readFlag(data), headerXML(data))
			}
		}
		s.mu.Unlock()
		io.WriteString(w, wrap(fmt.Sprintf(`<m:GetItemResponse><m:ResponseMessages>%s</m:ResponseMessages></m:GetItemResponse>`, msgs.String())))

	default:
		http.Error(w, "unknown op", http.StatusBadRequest)
	}
}

func wrap(inner string) string {
	return `<?xml version="1.0"?><soap:Envelope xmlns:soap="http://schemas.xmlsoap.org/soap/envelope/" xmlns:t="http://schemas.microsoft.com/exchange/services/2006/types" xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages"><soap:Body>` + inner + `</soap:Body></soap:Envelope>`
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

func reqIDs(body string) []string {
	i := strings.Index(body, "<m:ItemIds>")
	if i < 0 {
		return nil
	}
	var out []string
	for _, m := range idAttrRe.FindAllStringSubmatch(body[i:], -1) {
		out = append(out, unesc(m[1]))
	}
	return out
}

func unesc(s string) string {
	return strings.NewReplacer("&lt;", "<", "&gt;", ">", "&amp;", "&", "&quot;", `"`, "&#39;", "'", "&#x9;", "\t").Replace(s)
}

func tagText(body, tag string) string {
	open, close := "<t:"+tag, "</t:"+tag+">"
	i := strings.Index(body, open)
	if i < 0 {
		return ""
	}
	j := strings.Index(body[i:], ">")
	k := strings.Index(body[i:], close)
	if j < 0 || k < 0 {
		return ""
	}
	return body[i+j+1 : i+k]
}

func headerXML(raw []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return ""
	}
	var b strings.Builder
	for k, vs := range m.Header {
		for _, v := range vs {
			fmt.Fprintf(&b, `<t:InternetMessageHeader HeaderName="%s">%s</t:InternetMessageHeader>`, k, esc(v))
		}
	}
	return b.String()
}

func readFlag(raw []byte) string {
	m, err := mail.ReadMessage(bytes.NewReader(raw))
	if err == nil && strings.EqualFold(m.Header.Get("X-Test-Read"), "yes") {
		return "<t:IsRead>true</t:IsRead>"
	}
	return "<t:IsRead>false</t:IsRead>"
}
