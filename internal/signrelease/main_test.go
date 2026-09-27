package main

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func pemKey(t *testing.T, key any) string {
	t.Helper()
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

// trusting verifies the way the CLI does, with pub as its only key.
func trusting(pub ed25519.PublicKey) func(sums, sig []byte) error {
	return func(sums, sig []byte) error {
		if len(sig) == ed25519.SignatureSize && ed25519.Verify(pub, sums, sig) {
			return nil
		}
		return errors.New("untrusted")
	}
}

func checksums(t *testing.T) (in, out string) {
	t.Helper()
	dir := t.TempDir()
	in = filepath.Join(dir, "checksums.txt")
	if err := os.WriteFile(in, []byte("abc123  firmfact_1.2.3_linux_amd64.tar.gz\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return in, in + ".sig"
}

func TestSignWritesARawSignature(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	in, out := checksums(t)
	if err := sign(pemKey(t, priv), in, out, trusting(pub)); err != nil {
		t.Fatalf("sign: %v", err)
	}
	sums, _ := os.ReadFile(in)
	sig, _ := os.ReadFile(out)
	if len(sig) != ed25519.SignatureSize || !ed25519.Verify(pub, sums, sig) {
		t.Fatalf("signature %x does not verify", sig)
	}
}

// Every refusal writes no signature file, so GoReleaser fails the release
// instead of publishing one that no installed CLI accepts.
func TestSignRefuses(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	cases := map[string]struct {
		key  string
		want string
	}{
		"no key":           {"", "not set"},
		"not PEM":          {"c2VjcmV0", "not a PEM"},
		"not PKCS #8":      {string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")})), "no readable"},
		"not Ed25519":      {pemKey(t, ec), "not an Ed25519 key"},
		"an untrusted key": {pemKey(t, other), "would refuse"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			in, out := checksums(t)
			err := sign(c.key, in, out, trusting(pub))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want an error containing %q, got %v", c.want, err)
			}
			if c.key != "" && strings.Contains(err.Error(), strings.TrimSpace(c.key)) {
				t.Errorf("the error quotes the key: %v", err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Errorf("a signature file was written: %v", err)
			}
		})
	}
}
