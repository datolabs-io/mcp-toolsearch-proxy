// Command mint writes the end-to-end tests' signing keys and tokens: a
// fresh ES256 key, its public half as a JWKS for the gateway, and a token
// per test user. The private key is not kept, so the tokens can only be
// replaced, never added to. Run it from the repository root:
//
//	go run ./tests/e2e/mint -dir tests/e2e/testdata
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// Issuer and Audience are what the gateway of the tests expects.
const (
	Issuer   = "https://issuer.e2e.test"
	Audience = "http://localhost:3000/mcp/search"
)

// coordinateBytes is the size of a P-256 coordinate and signature half.
const coordinateBytes = 32

// lifetime is how long the tokens stay valid.
const lifetime = 100 * 365 * 24 * time.Hour

func main() {
	dir := flag.String("dir", "tests/e2e/testdata", "directory to write jwks.json and <user>.jwt to")
	flag.Parse()
	if err := mint(*dir, []string{"alice", "bob"}, time.Now()); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func mint(dir string, users []string, now time.Time) error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate key: %w", err)
	}
	pub, err := key.PublicKey.Bytes()
	if err != nil {
		return fmt.Errorf("public key: %w", err)
	}
	x, y := pub[1:1+coordinateBytes], pub[1+coordinateBytes:]
	kid := thumbprint(x, y)
	jwks, err := json.MarshalIndent(map[string]any{"keys": []map[string]string{{
		"kty": "EC", "crv": "P-256", "alg": "ES256", "use": "sig", "kid": kid,
		"x": b64(x), "y": b64(y),
	}}}, "", "  ")
	if err != nil {
		return fmt.Errorf("jwks: %w", err)
	}
	if err = os.WriteFile(filepath.Join(dir, "jwks.json"), append(jwks, '\n'), 0o600); err != nil {
		return fmt.Errorf("write jwks: %w", err)
	}
	for _, user := range users {
		tok, terr := token(key, kid, map[string]any{
			"iss": Issuer, "aud": Audience, "sub": user,
			"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(lifetime).Unix(),
		})
		if terr != nil {
			return terr
		}
		if terr = os.WriteFile(filepath.Join(dir, user+".jwt"), []byte(tok+"\n"), 0o600); terr != nil {
			return fmt.Errorf("write token: %w", terr)
		}
	}
	return nil
}

func token(key *ecdsa.PrivateKey, kid string, claims map[string]any) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "ES256", "typ": "JWT", "kid": kid})
	if err != nil {
		return "", fmt.Errorf("header: %w", err)
	}
	body, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("claims: %w", err)
	}
	signing := b64(header) + "." + b64(body)
	sum := sha256.Sum256([]byte(signing))
	r, s, err := ecdsa.Sign(rand.Reader, key, sum[:])
	if err != nil {
		return "", fmt.Errorf("sign: %w", err)
	}
	sig := append(pad(r), pad(s)...)
	return signing + "." + b64(sig), nil
}

// pad returns n as 32 big-endian bytes, as a JWS ES256 signature holds it.
func pad(n *big.Int) []byte {
	out := make([]byte, coordinateBytes)
	n.FillBytes(out)
	return out
}

// thumbprint is the RFC 7638 thumbprint of the key, used as its id.
func thumbprint(x, y []byte) string {
	sum := sha256.Sum256([]byte(`{"crv":"P-256","kty":"EC","x":"` + b64(x) + `","y":"` + b64(y) + `"}`))
	return b64(sum[:])
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
