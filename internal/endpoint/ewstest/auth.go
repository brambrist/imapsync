package ewstest

import (
	"bytes"
	"crypto/hmac"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math/bits"
	"net/http"
	"strings"
	"unicode/utf16"
)

// Auth modes of the fake server. The zero value accepts any Basic credentials
// (the original behaviour, kept for the existing tests).
type authMode int

const (
	authBasicAny authMode = iota
	authNTLM
	authOAuth2
)

// RequireNTLM makes the server demand an NTLM handshake and verify the NTLMv2
// response against user (DOMAIN\user or a UPN) and pass - a wrong password is
// rejected, not just a missing header. Authentication sticks to the TCP
// connection, as on IIS.
func (s *Server) RequireNTLM(user, pass string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = authNTLM
	s.ntlmUser, s.ntlmPass = user, pass
}

// RequireOAuth2 makes the server accept only "Bearer <token>" issued by its own
// token endpoint (client credentials grant for clientID/secret) and returns
// that endpoint's URL.
func (s *Server) RequireOAuth2(clientID, secret string) (tokenURL string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.mode = authOAuth2
	s.clientID, s.secret = clientID, secret
	return s.URL + "/token"
}

// RevokeTokens invalidates every token issued so far (a revoked token that has
// not reached its expiry yet).
func (s *Server) RevokeTokens() {
	s.mu.Lock()
	s.validToken = ""
	s.mu.Unlock()
}

// authorized checks the request's credentials for the current mode; on
// failure it has already written the 401.
func (s *Server) authorized(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	mode := s.mode
	s.mu.Unlock()
	switch mode {
	case authNTLM:
		return s.ntlm(w, r)
	case authOAuth2:
		s.mu.Lock()
		ok := s.validToken != "" && r.Header.Get("Authorization") == "Bearer "+s.validToken
		s.LastAnchor = r.Header.Get("X-AnchorMailbox")
		s.mu.Unlock()
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer error="invalid_token"`)
			http.Error(w, "invalid token", http.StatusUnauthorized)
		}
		return ok
	default:
		if _, _, ok := r.BasicAuth(); !ok {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return false
		}
		return true
	}
}

// --- OAuth2 token endpoint ---

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = r.ParseForm()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.TokenRequests++
	if r.PostForm.Get("grant_type") != "client_credentials" ||
		r.PostForm.Get("client_id") != s.clientID || r.PostForm.Get("client_secret") != s.secret {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":             "invalid_client",
			"error_description": "AADSTS7000215: Invalid client secret provided.",
		})
		return
	}
	s.LastScope = r.PostForm.Get("scope")
	s.validToken = fmt.Sprintf("tok-%d", s.TokenRequests)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"token_type":   "Bearer",
		"expires_in":   3599,
		"access_token": s.validToken,
	})
}

// --- NTLM (server side, NTLMv2 only) ---

const (
	ntlmFlagUnicode    = 0x00000001
	ntlmFlagNTLM       = 0x00000200
	ntlmFlagExtSession = 0x00080000
	ntlmFlagTargetInfo = 0x00800000
)

func (s *Server) ntlm(w http.ResponseWriter, r *http.Request) bool {
	s.mu.Lock()
	authed := s.ntlmConns[r.RemoteAddr]
	s.mu.Unlock()
	if authed {
		return true
	}

	msg, ok := ntlmToken(r.Header.Get("Authorization"))
	if !ok || len(msg) < 12 {
		w.Header().Set("WWW-Authenticate", "NTLM")
		http.Error(w, "ntlm required", http.StatusUnauthorized)
		return false
	}
	switch binary.LittleEndian.Uint32(msg[8:12]) {
	case 1: // NEGOTIATE -> CHALLENGE
		var chal [8]byte
		_, _ = rand.Read(chal[:])
		s.mu.Lock()
		s.ntlmChal[r.RemoteAddr] = chal
		s.mu.Unlock()
		w.Header().Set("WWW-Authenticate", "NTLM "+base64.StdEncoding.EncodeToString(challengeMsg(chal)))
		http.Error(w, "challenge", http.StatusUnauthorized)
		return false
	case 3: // AUTHENTICATE
		s.mu.Lock()
		chal, had := s.ntlmChal[r.RemoteAddr]
		delete(s.ntlmChal, r.RemoteAddr)
		user, pass := s.ntlmUser, s.ntlmPass
		s.mu.Unlock()
		if had && verifyNTLMv2(msg, chal, user, pass) {
			s.mu.Lock()
			s.ntlmConns[r.RemoteAddr] = true
			s.NTLMHandshakes++
			s.mu.Unlock()
			return true
		}
	}
	w.Header().Set("WWW-Authenticate", "NTLM")
	http.Error(w, "ntlm failed", http.StatusUnauthorized)
	return false
}

func ntlmToken(h string) ([]byte, bool) {
	for _, scheme := range []string{"NTLM ", "Negotiate "} {
		if strings.HasPrefix(h, scheme) {
			b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(h, scheme))
			return b, err == nil && bytes.HasPrefix(b, []byte("NTLMSSP\x00"))
		}
	}
	return nil, false
}

// challengeMsg builds a CHALLENGE message: no target name, a TargetInfo that
// holds only MsvAvEOL.
func challengeMsg(chal [8]byte) []byte {
	m := make([]byte, 48+4)
	copy(m, "NTLMSSP\x00")
	binary.LittleEndian.PutUint32(m[8:], 2)
	binary.LittleEndian.PutUint32(m[16:], 48) // TargetName offset (empty)
	binary.LittleEndian.PutUint32(m[20:], ntlmFlagUnicode|ntlmFlagNTLM|ntlmFlagExtSession|ntlmFlagTargetInfo)
	copy(m[24:32], chal[:])
	binary.LittleEndian.PutUint16(m[40:], 4) // TargetInfo len
	binary.LittleEndian.PutUint16(m[42:], 4) // TargetInfo maxlen
	binary.LittleEndian.PutUint32(m[44:], 48)
	return m
}

func field(msg []byte, at int) []byte {
	if len(msg) < at+8 {
		return nil
	}
	n := int(binary.LittleEndian.Uint16(msg[at:]))
	off := int(binary.LittleEndian.Uint32(msg[at+4:]))
	if off+n > len(msg) {
		return nil
	}
	return msg[off : off+n]
}

func verifyNTLMv2(msg []byte, chal [8]byte, wantUser, pass string) bool {
	nt := field(msg, 20)
	domain := fromUTF16(field(msg, 28))
	user := fromUTF16(field(msg, 36))
	if len(nt) <= 16 {
		return false
	}
	got := user
	if domain != "" {
		got = domain + `\` + user
	}
	if !strings.EqualFold(got, wantUser) {
		return false
	}
	key := hmacMD5(md4(toUTF16(pass)), toUTF16(strings.ToUpper(user)+domain))
	proof := hmacMD5(key, chal[:], nt[16:])
	return hmac.Equal(proof, nt[:16])
}

func hmacMD5(key []byte, data ...[]byte) []byte {
	m := hmac.New(md5.New, key)
	for _, d := range data {
		m.Write(d)
	}
	return m.Sum(nil)
}

func toUTF16(s string) []byte {
	u := utf16.Encode([]rune(s))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func fromUTF16(b []byte) string {
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = binary.LittleEndian.Uint16(b[2*i:])
	}
	return string(utf16.Decode(u))
}

// md4 (RFC 1320) - only for the NT hash; not in the standard library.
func md4(data []byte) []byte {
	msg := append([]byte{}, data...)
	msg = append(msg, 0x80)
	for len(msg)%64 != 56 {
		msg = append(msg, 0)
	}
	msg = binary.LittleEndian.AppendUint64(msg, uint64(len(data))*8)

	a, b, c, d := uint32(0x67452301), uint32(0xefcdab89), uint32(0x98badcfe), uint32(0x10325476)
	var x [16]uint32
	for blk := 0; blk < len(msg); blk += 64 {
		for i := range x {
			x[i] = binary.LittleEndian.Uint32(msg[blk+4*i:])
		}
		aa, bb, cc, dd := a, b, c, d
		f := func(x, y, z uint32) uint32 { return x&y | ^x&z }
		g := func(x, y, z uint32) uint32 { return x&y | x&z | y&z }
		h := func(x, y, z uint32) uint32 { return x ^ y ^ z }
		for _, i := range []int{0, 4, 8, 12} {
			a = bits.RotateLeft32(a+f(b, c, d)+x[i], 3)
			d = bits.RotateLeft32(d+f(a, b, c)+x[i+1], 7)
			c = bits.RotateLeft32(c+f(d, a, b)+x[i+2], 11)
			b = bits.RotateLeft32(b+f(c, d, a)+x[i+3], 19)
		}
		for _, i := range []int{0, 1, 2, 3} {
			a = bits.RotateLeft32(a+g(b, c, d)+x[i]+0x5a827999, 3)
			d = bits.RotateLeft32(d+g(a, b, c)+x[i+4]+0x5a827999, 5)
			c = bits.RotateLeft32(c+g(d, a, b)+x[i+8]+0x5a827999, 9)
			b = bits.RotateLeft32(b+g(c, d, a)+x[i+12]+0x5a827999, 13)
		}
		for _, i := range []int{0, 2, 1, 3} {
			a = bits.RotateLeft32(a+h(b, c, d)+x[i]+0x6ed9eba1, 3)
			d = bits.RotateLeft32(d+h(a, b, c)+x[i+8]+0x6ed9eba1, 9)
			c = bits.RotateLeft32(c+h(d, a, b)+x[i+4]+0x6ed9eba1, 11)
			b = bits.RotateLeft32(b+h(c, d, a)+x[i+12]+0x6ed9eba1, 15)
		}
		a, b, c, d = a+aa, b+bb, c+cc, d+dd
	}
	out := make([]byte, 16)
	binary.LittleEndian.PutUint32(out[0:], a)
	binary.LittleEndian.PutUint32(out[4:], b)
	binary.LittleEndian.PutUint32(out[8:], c)
	binary.LittleEndian.PutUint32(out[12:], d)
	return out
}
