package gsuite

// White-box tests: the security-critical bits (JWT-bearer assertion,
// token caching, the blast-radius gate) and the wire shapes are the
// parts that MUST be right. There is no live Google here — every Google
// endpoint is an httptest server returning canned API JSON, which is
// exactly the wire contract the official clients speak. Requests reach
// the server via the rewriteRT transport (see drive_test.go).

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"testing"
)

// testRSA makes a fresh key + a service-account JSON blob using it.
func testRSA(t *testing.T, tokenURI string) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der := x509.MarshalPKCS1PrivateKey(key)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: der})
	saj, _ := json.Marshal(map[string]string{
		"type":         "service_account",
		"client_email": "robot@proj.iam.gserviceaccount.com",
		"private_key":  string(pemBytes),
		"token_uri":    tokenURI,
	})
	return key, saj
}
