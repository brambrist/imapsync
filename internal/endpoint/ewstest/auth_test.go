package ewstest

import (
	"encoding/hex"
	"testing"
)

func TestMD4Vectors(t *testing.T) {
	// RFC 1320, appendix A.5.
	for in, want := range map[string]string{
		"":    "31d6cfe0d16ae931b73c59d7e0c089c0",
		"abc": "a448017aaf21d8525fc10ae87aa6729d",
		"12345678901234567890123456789012345678901234567890123456789012345678901234567890": "e33b4ddc9c38f2199c3e7b164fcc0536",
	} {
		if got := hex.EncodeToString(md4([]byte(in))); got != want {
			t.Errorf("md4(%q) = %s, want %s", in, got, want)
		}
	}
}
