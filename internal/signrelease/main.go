// Command signrelease signs a release's checksums.txt, which is what lets
// `firmfact update` tell a genuine release from replaced assets. GoReleaser
// runs it for the checksum artifact (.goreleaser.yaml, signs):
//
//	go run ./internal/signrelease dist/checksums.txt dist/checksums.txt.sig
//
// The key comes from RELEASE_SIGNING_KEY, a PEM "PRIVATE KEY" (PKCS #8)
// holding an Ed25519 key; the release workflow passes it from a secret. The
// signature is the raw 64 bytes, which `openssl pkeyutl -verify -rawin`
// also checks.
package main

import (
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"

	"github.com/firmfact/cli/internal/update"
)

const keyVar = "RELEASE_SIGNING_KEY"

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: signrelease <checksums.txt> <signature file>")
		os.Exit(2)
	}
	if err := sign(os.Getenv(keyVar), os.Args[1], os.Args[2], update.VerifyChecksums); err != nil {
		fmt.Fprintln(os.Stderr, "signrelease:", err)
		os.Exit(1)
	}
}

// sign writes the signature of the file at in to out. verify is how the
// CLI checks it: a release signed with a key the CLI does not trust would
// install nowhere, so that stops the release here instead of failing every
// update later.
func sign(pemKey, in, out string, verify func(sums, sig []byte) error) error {
	key, err := parseKey(pemKey)
	if err != nil {
		return err
	}
	sums, err := os.ReadFile(in) //nolint:gosec // G703: GoReleaser names its own checksums file
	if err != nil {
		return err
	}
	sig := ed25519.Sign(key, sums)
	if err := verify(sums, sig); err != nil {
		return fmt.Errorf("the CLI would refuse this signature (%w); add the key's public half to internal/update/signature.go first", err)
	}
	// Published next to checksums.txt, so readable by all.
	return os.WriteFile(out, sig, 0o644) //nolint:gosec // G306, see above
}

// parseKey reads the private key. Its errors never quote the key.
func parseKey(pemKey string) (ed25519.PrivateKey, error) {
	if pemKey == "" {
		return nil, errors.New(keyVar + " is not set; a release must not go out unsigned")
	}
	block, _ := pem.Decode([]byte(pemKey))
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New(keyVar + ` is not a PEM "PRIVATE KEY" block`)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New(keyVar + " holds no readable PKCS #8 key")
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("%s holds a %T, not an Ed25519 key", keyVar, parsed)
	}
	return key, nil
}
