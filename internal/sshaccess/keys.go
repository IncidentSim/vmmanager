// Package sshaccess provides per-VM personal SSH access: key generation and
// parsing, an encrypted-at-rest credential store, and offline provisioning of
// authorized_keys into a per-VM ext4 disk copy before the VM first boots.
//
// Nothing here mounts anything, needs root, or touches a base image. Disk edits
// are performed by e2fsprogs' debugfs on the unmounted per-VM copy.
package sshaccess

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/pem"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
)

// ErrInvalidKey reports a public key that is not exactly one acceptable
// OpenSSH public key. Messages never echo the offending input.
var ErrInvalidKey = errors.New("sshaccess: invalid ssh public key")

const maxPublicKeyLen = 2048 // generous: RSA-4096 is ~730 bytes

// GenerateEd25519 returns a new OpenSSH-format private key (PEM) and its
// canonical authorized_keys public line ("ssh-ed25519 AAAA...", no comment).
func GenerateEd25519() (private []byte, public string, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		return nil, "", err
	}
	sp, err := ssh.NewPublicKey(pub)
	if err != nil {
		return nil, "", err
	}
	return pem.EncodeToMemory(block), canonicalLine(sp), nil
}

func canonicalLine(k ssh.PublicKey) string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
}

// ParsePublicKey accepts exactly one single-line OpenSSH public key and returns
// its canonical form (type + base64, comment dropped). Options, certificates,
// multiple lines, control characters and weak RSA (<2048 bits) are rejected.
func ParsePublicKey(line string) (string, error) {
	s := strings.TrimSuffix(line, "\n")
	if s == "" || len(s) > maxPublicKeyLen || strings.ContainsAny(s, "\r\n\x00") {
		return "", fmt.Errorf("%w: must be one non-empty line", ErrInvalidKey)
	}
	for _, c := range s {
		if unicode.IsControl(c) {
			return "", fmt.Errorf("%w: control character", ErrInvalidKey)
		}
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(s))
	if err != nil {
		return "", fmt.Errorf("%w: not a parseable OpenSSH public key", ErrInvalidKey)
	}
	if len(options) > 0 || len(rest) > 0 {
		return "", fmt.Errorf("%w: key options and extra lines are not allowed", ErrInvalidKey)
	}
	// ParseAuthorizedKey silently skips an unparseable leading field as
	// "options"; insist the first field is the key type we parsed.
	if f := strings.Fields(s); len(f) < 2 || f[0] != key.Type() {
		return "", fmt.Errorf("%w: key type does not match key data", ErrInvalidKey)
	}
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	case ssh.KeyAlgoRSA:
		ck, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return "", fmt.Errorf("%w: unsupported key", ErrInvalidKey)
		}
		rk, ok := ck.CryptoPublicKey().(*rsa.PublicKey)
		if !ok || rk.N.BitLen() < 2048 {
			return "", fmt.Errorf("%w: RSA keys must be at least 2048 bits", ErrInvalidKey)
		}
	default:
		return "", fmt.Errorf("%w: unsupported key type", ErrInvalidKey)
	}
	return canonicalLine(key), nil
}
