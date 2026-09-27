package update

import (
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
)

// A release publishes checksums.txt, the SHA-256 of every archive, and
// checksums.txt.sig, a raw 64-byte Ed25519 signature over it. The release
// workflow signs with the private half of a key below (the
// RELEASE_SIGNING_KEY secret, see internal/signrelease); checksums.txt on
// its own proves nothing, as whoever can replace an archive can replace the
// file beside it.
const (
	checksumsFile = "checksums.txt"
	signatureFile = checksumsFile + ".sig"
)

// releaseKeys are the public keys whose signature makes a release
// installable, base64 of the raw 32 bytes. To rotate, ship a release that
// trusts the new key alongside the old one, then sign with the new key and
// drop the old one a release later: installed copies must already trust a
// key before a release is signed with it.
var releaseKeys = []string{
	"gBd0tWG56OhnOnS2t5ixh1ZWdDUoHWQoUarNgP1Cc+o=", // 2026-09, the first release key
}

// trustedKeys is what verification uses: a variable so tests can sign
// releases with keys of their own.
var trustedKeys = mustPublicKeys(releaseKeys)

// errUntrusted is a signature that no trusted key made over these checksums.
var errUntrusted = errors.New("the signature does not match firmfact's release key")

// VerifyChecksums reports whether sig is a trusted release key's signature
// over sums, the contents of a release's checksums.txt.
func VerifyChecksums(sums, sig []byte) error {
	return verifySignature(sums, sig, trustedKeys)
}

func verifySignature(sums, sig []byte, keys []ed25519.PublicKey) error {
	if len(sig) != ed25519.SignatureSize {
		return fmt.Errorf("the signature is %d bytes, not %d", len(sig), ed25519.SignatureSize)
	}
	for _, key := range keys {
		if ed25519.Verify(key, sums, sig) {
			return nil
		}
	}
	return errUntrusted
}

// mustPublicKeys decodes the embedded keys. A malformed one is a mistake in
// this file, which every test run of the package then reports.
func mustPublicKeys(encoded []string) []ed25519.PublicKey {
	keys := make([]ed25519.PublicKey, 0, len(encoded))
	for _, e := range encoded {
		raw, err := base64.StdEncoding.DecodeString(e)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			panic(fmt.Sprintf("update: release key %q is not a base64 Ed25519 public key", e))
		}
		keys = append(keys, ed25519.PublicKey(raw))
	}
	return keys
}
