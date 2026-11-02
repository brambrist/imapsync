package endpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	ntlmssp "github.com/Azure/go-ntlmssp"

	"imapsync/config"
)

// ewsAuth authorizes one EWS request. Implementations are shared by all
// connections of an ewsBackend (one per server), so they must be thread-safe.
type ewsAuth interface {
	// transport wraps the base transport (NTLM needs to drive the handshake
	// on the connection itself; the others return it unchanged).
	transport(base http.RoundTripper) http.RoundTripper
	// authorize sets the credentials on req; impersonate is the target mailbox.
	authorize(ctx context.Context, req *http.Request, impersonate string) error
	// rejected is called on HTTP 401; true means "retry once" (e.g. a cached
	// OAuth2 token was revoked before its expiry).
	rejected(req *http.Request) bool
	// who describes the identity for logs (never a secret).
	who() string
}

func newEWSAuth(srv config.Server, timeout time.Duration, debug bool, logf func(string, ...any)) ewsAuth {
	switch srv.EWSAuth {
	case config.EWSAuthNTLM:
		return ntlmAuth{user: srv.MasterUser, pass: srv.MasterPass}
	case config.EWSAuthOAuth2:
		scope := strings.TrimSpace(srv.OAuthScope)
		if scope == "" {
			scope = config.DefaultOAuthScope
		}
		return &oauthAuth{
			// The token endpoint is a different host (login.microsoftonline.com)
			// with a public certificate - the EWS server's TLS settings
			// (ca_cert, ServerName, TLS version caps) do not apply to it.
			http:     &http.Client{Timeout: timeout},
			tokenURL: srv.OAuthTokenEndpoint(),
			clientID: srv.OAuthClientID,
			secret:   srv.OAuthClientSecret,
			scope:    scope,
			debug:    debug,
			logf:     logf,
		}
	default:
		return basicAuth{user: srv.MasterUser, pass: srv.MasterPass}
	}
}

// --- Basic ---

type basicAuth struct{ user, pass string }

func (a basicAuth) transport(base http.RoundTripper) http.RoundTripper { return base }
func (a basicAuth) authorize(_ context.Context, req *http.Request, _ string) error {
	req.SetBasicAuth(a.user, a.pass)
	return nil
}
func (a basicAuth) rejected(*http.Request) bool { return false }
func (a basicAuth) who() string                 { return "basic " + a.user }

// --- NTLM ---

// ntlmAuth: the credentials are set as Basic on the request and
// ntlmssp.Negotiator converts them into the NTLM handshake on a 401. Basic is
// never sent to the server (AllowBasicAuth stays false), so a server that only
// offers Basic fails with HTTP 401 instead of silently downgrading. The user
// is DOMAIN\user or user@domain (UPN).
type ntlmAuth struct{ user, pass string }

func (a ntlmAuth) transport(base http.RoundTripper) http.RoundTripper {
	return ntlmssp.Negotiator{RoundTripper: base}
}
func (a ntlmAuth) authorize(_ context.Context, req *http.Request, _ string) error {
	req.SetBasicAuth(a.user, a.pass)
	return nil
}
func (a ntlmAuth) rejected(*http.Request) bool { return false }
func (a ntlmAuth) who() string                 { return "ntlm " + a.user }

// --- OAuth2 client credentials ---

// tokenSkew - a token is refreshed this long before its reported expiry.
const tokenSkew = 2 * time.Minute

// oauthAuth gets an app-only token (client credentials grant) and caches it
// until shortly before expiry. App-only EWS access still needs
// ExchangeImpersonation in the SOAP header (ewsClient.call adds it) plus the
// X-AnchorMailbox HTTP header, which routes the request to the mailbox's
// backend server.
type oauthAuth struct {
	http     *http.Client
	tokenURL string
	clientID string
	secret   string
	scope    string
	debug    bool
	logf     func(string, ...any)

	mu  sync.Mutex
	tok string
	exp time.Time
}

func (a *oauthAuth) transport(base http.RoundTripper) http.RoundTripper { return base }
func (a *oauthAuth) who() string                                        { return "oauth2 app " + a.clientID }

func (a *oauthAuth) authorize(ctx context.Context, req *http.Request, impersonate string) error {
	tok, err := a.token(ctx)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	if impersonate != "" {
		req.Header.Set("X-AnchorMailbox", impersonate)
	}
	return nil
}

// rejected drops the cached token if req carried it, so the retry fetches a
// fresh one. Retrying only makes sense when there was a cached token to drop.
func (a *oauthAuth) rejected(req *http.Request) bool {
	used := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok == "" || a.tok != used {
		return false
	}
	a.tok = ""
	return true
}

func (a *oauthAuth) token(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.tok != "" && time.Now().Before(a.exp) {
		return a.tok, nil
	}
	tok, ttl, err := a.fetch(ctx)
	if err != nil {
		return "", err
	}
	a.tok = tok
	a.exp = time.Now().Add(ttl - tokenSkew)
	if a.debug {
		a.logf("ews oauth2: token for app %s acquired, expires in %s", a.clientID, ttl)
	}
	return tok, nil
}

// fetch performs the client credentials grant against tokenURL.
func (a *oauthAuth) fetch(ctx context.Context) (string, time.Duration, error) {
	form := url.Values{
		"grant_type":    {"client_credentials"},
		"client_id":     {a.clientID},
		"client_secret": {a.secret},
		"scope":         {a.scope},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, a.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", 0, fmt.Errorf("oauth2 token %s: %w", a.tokenURL, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("oauth2 token %s: %w", a.tokenURL, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var tr struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.Unmarshal(data, &tr); err != nil {
		return "", 0, fmt.Errorf("oauth2 token %s: HTTP %d: %s", a.tokenURL, resp.StatusCode, snippet(data))
	}
	if resp.StatusCode != http.StatusOK || tr.AccessToken == "" {
		if tr.Error != "" {
			return "", 0, fmt.Errorf("oauth2 token %s: HTTP %d: %s: %s", a.tokenURL, resp.StatusCode, tr.Error, oneLine(tr.Description))
		}
		return "", 0, fmt.Errorf("oauth2 token %s: HTTP %d: no access_token", a.tokenURL, resp.StatusCode)
	}
	ttl := time.Duration(tr.ExpiresIn) * time.Second
	if ttl <= tokenSkew {
		ttl = tokenSkew + time.Minute // missing/odd expires_in: cache briefly
	}
	return tr.AccessToken, ttl, nil
}
