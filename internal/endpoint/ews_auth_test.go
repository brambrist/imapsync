package endpoint

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"imapsync/config"
	"imapsync/internal/endpoint/ewstest"
)

func ewsBackendWith(t *testing.T, srv config.Server, logf func(string, ...any)) Backend {
	t.Helper()
	srv.Type = config.EndpointEWS
	b, err := NewBackend(srv, 0, 5*time.Second, false, 50, logf != nil, logf)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// ewsRoundTrip does a Select + ListIDs - two SOAP calls on one connection.
func ewsRoundTrip(t *testing.T, b Backend, user string) error {
	t.Helper()
	ep, err := b.Connect(context.Background(), user)
	if err != nil {
		return err
	}
	defer ep.Close()
	if _, _, err := ep.Select("Sent"); err != nil {
		return err
	}
	_, err = ep.ListIDs()
	return err
}

func TestEWSNTLM(t *testing.T) {
	for _, user := range []string{`CORP\svc`, "svc@corp.ru"} {
		t.Run(user, func(t *testing.T) {
			srv := ewstest.New(t, map[string]string{"i-1": ewsMsg})
			srv.RequireNTLM(user, "s3cret")
			b := ewsBackendWith(t, config.Server{EWSUrl: srv.URL, EWSAuth: config.EWSAuthNTLM, MasterUser: user, MasterPass: "s3cret"}, nil)
			if err := ewsRoundTrip(t, b, "ivanov@corp.ru"); err != nil {
				t.Fatal(err)
			}
			// The second call reuses the authenticated keep-alive connection.
			if srv.NTLMHandshakes != 1 {
				t.Errorf("NTLM handshakes = %d, want 1 (connection-level auth)", srv.NTLMHandshakes)
			}
			if srv.LastImp != "ivanov@corp.ru" {
				t.Errorf("ExchangeImpersonation = %q", srv.LastImp)
			}
		})
	}
}

func TestEWSNTLMWrongPassword(t *testing.T) {
	srv := ewstest.New(t, nil)
	srv.RequireNTLM(`CORP\svc`, "s3cret")
	b := ewsBackendWith(t, config.Server{EWSUrl: srv.URL, EWSAuth: config.EWSAuthNTLM, MasterUser: `CORP\svc`, MasterPass: "wrong"}, nil)
	err := ewsRoundTrip(t, b, "u@d")
	if err == nil || !strings.Contains(err.Error(), "401") || !strings.Contains(err.Error(), `ntlm CORP\svc`) {
		t.Fatalf("expected a 401 naming the ntlm identity, got %v", err)
	}
}

func TestEWSBasicAgainstNTLMServer(t *testing.T) {
	// The default (basic) does not speak NTLM - a clear 401, not a hang.
	srv := ewstest.New(t, nil)
	srv.RequireNTLM(`CORP\svc`, "s3cret")
	b := ewsBackendWith(t, config.Server{EWSUrl: srv.URL, MasterUser: `CORP\svc`, MasterPass: "s3cret"}, nil)
	if err := ewsRoundTrip(t, b, "u@d"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("expected 401, got %v", err)
	}
}

func oauthServer(t *testing.T, srv *ewstest.Server, secret string) config.Server {
	tokenURL := srv.RequireOAuth2("app-1", "s3cret")
	return config.Server{EWSUrl: srv.URL, EWSAuth: config.EWSAuthOAuth2,
		OAuthClientID: "app-1", OAuthClientSecret: secret, OAuthTokenURL: tokenURL}
}

func TestEWSOAuth2TokenIsCachedAcrossConnections(t *testing.T) {
	srv := ewstest.New(t, map[string]string{"i-1": ewsMsg})
	var mu sync.Mutex
	var lines []string
	logf := func(f string, a ...any) { mu.Lock(); lines = append(lines, fmt.Sprintf(f, a...)); mu.Unlock() }
	b := ewsBackendWith(t, oauthServer(t, srv, "s3cret"), logf)

	for _, u := range []string{"ivanov@corp.ru", "petrov@corp.ru"} {
		if err := ewsRoundTrip(t, b, u); err != nil {
			t.Fatal(err)
		}
		if srv.LastAnchor != u || srv.LastImp != u {
			t.Errorf("X-AnchorMailbox = %q, ExchangeImpersonation = %q, want %q", srv.LastAnchor, srv.LastImp, u)
		}
	}
	if srv.TokenRequests != 1 {
		t.Errorf("token requests = %d, want 1 (cached, shared by connections)", srv.TokenRequests)
	}
	if srv.LastScope != config.DefaultOAuthScope {
		t.Errorf("scope = %q", srv.LastScope)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, l := range lines {
		if strings.Contains(l, "tok-1") || strings.Contains(l, "s3cret") {
			t.Errorf("debug log leaked a credential: %q", l)
		}
	}
}

func TestEWSOAuth2RevokedTokenIsRefreshed(t *testing.T) {
	srv := ewstest.New(t, nil)
	b := ewsBackendWith(t, oauthServer(t, srv, "s3cret"), nil)
	if err := ewsRoundTrip(t, b, "u@d"); err != nil {
		t.Fatal(err)
	}
	srv.RevokeTokens()
	if err := ewsRoundTrip(t, b, "u@d"); err != nil {
		t.Fatalf("a revoked token should be dropped and refetched: %v", err)
	}
	if srv.TokenRequests != 2 {
		t.Errorf("token requests = %d, want 2", srv.TokenRequests)
	}
}

func TestEWSOAuth2BadSecret(t *testing.T) {
	srv := ewstest.New(t, nil)
	b := ewsBackendWith(t, oauthServer(t, srv, "wrong"), nil)
	err := ewsRoundTrip(t, b, "u@d")
	if err == nil || !strings.Contains(err.Error(), "invalid_client") || !strings.Contains(err.Error(), "AADSTS7000215") {
		t.Fatalf("expected the token endpoint's error, got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("error leaked the secret: %v", err)
	}
}
