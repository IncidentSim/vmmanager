// Package jwtauth issues and verifies the RS256 user API tokens. The bot holds
// the RSA private key (Signer); the API only ever loads the public key
// (Verifier). Verification is strict: one algorithm, required issuer, audience,
// expiry, not-before and issued-at claims, a canonical positive decimal subject
// and a bounded token length. Nothing in an unverified token is ever trusted.
package jwtauth

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/petruekhin/vibe-firecracker/internal/identity"
	"golang.org/x/sys/unix"
)

const (
	// DefaultTTL is the default lifetime of a user token (28800 seconds).
	DefaultTTL = 8 * time.Hour
	// MaxTTL bounds any token ever accepted or issued.
	MaxTTL = 7 * 24 * time.Hour
	// MaxTokenLength bounds the bearer accepted for verification.
	MaxTokenLength = 4096
	// MinRSABits is the minimum accepted modulus size (3072 is recommended).
	MinRSABits = 2048

	leeway        = 5 * time.Second
	maxKeyFile    = 16 * 1024
	usernameClaim = "tg_username"
)

// Claims are the verified token contents.
type Claims struct {
	Subject   int64     // Telegram user ID (immutable resource owner)
	Username  string    // canonical bootstrap username claim, may be empty
	IssuedAt  time.Time // iat
	ExpiresAt time.Time // exp
}

type wireClaims struct {
	jwt.RegisteredClaims
	Username string `json:"tg_username,omitempty"`
}

var errInvalid = errors.New("invalid token")

// Signer holds the RSA private key. It lives only in the bot.
type Signer struct {
	key      *rsa.PrivateKey
	iss, aud string
}

// Verifier holds only the RSA public key.
type Verifier struct {
	key      *rsa.PublicKey
	iss, aud string
}

func readKeyFile(path string, private bool) ([]byte, error) {
	if !filepath.IsAbs(path) {
		return nil, errors.New("key file path must be absolute")
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, errors.New("cannot open key file")
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil || st.Mode&unix.S_IFMT != unix.S_IFREG {
		return nil, errors.New("key file must be a regular file")
	}
	if private && (st.Mode&0o077 != 0 || st.Uid != uint32(os.Geteuid())) {
		return nil, errors.New("private key file must be owned with mode 0600")
	}
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFile+1))
	if err != nil || len(raw) > maxKeyFile {
		return nil, errors.New("invalid key file size")
	}
	return raw, nil
}

// LoadSigner reads the private key (PKCS#8 or PKCS#1 PEM, RSA, >= 2048 bits).
func LoadSigner(privateKeyFile, issuer, audience string) (*Signer, error) {
	if issuer == "" || audience == "" {
		return nil, errors.New("issuer and audience are required")
	}
	raw, err := readKeyFile(privateKeyFile, true)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("private key is not PEM")
	}
	var key *rsa.PrivateKey
	switch block.Type {
	case "PRIVATE KEY":
		k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		rk, ok := k.(*rsa.PrivateKey)
		if err != nil || !ok {
			return nil, errors.New("private key must be RSA")
		}
		key = rk
	case "RSA PRIVATE KEY":
		key, err = x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid RSA private key")
		}
	default:
		return nil, errors.New("not an RSA private key")
	}
	if key.N.BitLen() < MinRSABits || key.Validate() != nil {
		return nil, errors.New("RSA private key too small or invalid")
	}
	return &Signer{key: key, iss: issuer, aud: audience}, nil
}

// LoadVerifier reads a PEM RSA public key (PKIX or PKCS#1). Private key
// material is refused: the API must never be given it.
func LoadVerifier(publicKeyFile, issuer, audience string) (*Verifier, error) {
	if issuer == "" || audience == "" {
		return nil, errors.New("issuer and audience are required")
	}
	raw, err := readKeyFile(publicKeyFile, false)
	if err != nil {
		return nil, err
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, errors.New("public key is not PEM")
	}
	var pub *rsa.PublicKey
	switch block.Type {
	case "PUBLIC KEY":
		k, err := x509.ParsePKIXPublicKey(block.Bytes)
		rk, ok := k.(*rsa.PublicKey)
		if err != nil || !ok {
			return nil, errors.New("public key must be RSA")
		}
		pub = rk
	case "RSA PUBLIC KEY":
		pub, err = x509.ParsePKCS1PublicKey(block.Bytes)
		if err != nil {
			return nil, errors.New("invalid RSA public key")
		}
	default:
		return nil, errors.New("not a public key (private key material is refused)")
	}
	if pub.N.BitLen() < MinRSABits {
		return nil, errors.New("RSA public key too small")
	}
	return &Verifier{key: pub, iss: issuer, aud: audience}, nil
}

// Issue signs an RS256 token for a Telegram user. username is the already
// verified sender handle (optional); it is stored canonicalized as the
// bootstrap claim.
func (s *Signer) Issue(userID int64, username string, ttl time.Duration, now time.Time) (string, time.Time, error) {
	if userID <= 0 || ttl <= 0 || ttl > MaxTTL {
		return "", time.Time{}, errors.New("invalid token request")
	}
	canon := ""
	if username != "" {
		var err error
		if canon, err = identity.CanonicalUsername(username); err != nil {
			return "", time.Time{}, errors.New("invalid token request")
		}
	}
	exp := now.Add(ttl)
	c := wireClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    s.iss,
			Audience:  jwt.ClaimStrings{s.aud},
			Subject:   strconv.FormatInt(userID, 10),
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
		Username: canon,
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodRS256, c).SignedString(s.key)
	if err != nil {
		return "", time.Time{}, errors.New("cannot sign token")
	}
	return tok, exp.Truncate(time.Second), nil
}

func tokenCharsOK(t string) bool {
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == '.') {
			return false
		}
	}
	return true
}

// Verify validates signature and claims against the supplied clock. Errors are
// deliberately uninformative and never contain the token.
func (v *Verifier) Verify(token string, now time.Time) (Claims, error) {
	if token == "" || len(token) > MaxTokenLength || !tokenCharsOK(token) {
		return Claims{}, errInvalid
	}
	var wc wireClaims
	parser := jwt.NewParser(
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithIssuer(v.iss),
		jwt.WithAudience(v.aud),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(),
		jwt.WithLeeway(leeway),
		jwt.WithStrictDecoding(),
		jwt.WithTimeFunc(func() time.Time { return now }),
	)
	t, err := parser.ParseWithClaims(token, &wc, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok || t.Method.Alg() != "RS256" {
			return nil, errInvalid
		}
		return v.key, nil
	})
	if err != nil || !t.Valid {
		return Claims{}, errInvalid
	}
	if wc.NotBefore == nil || wc.IssuedAt == nil || wc.ExpiresAt == nil || len(wc.Audience) != 1 {
		return Claims{}, errInvalid
	}
	if wc.ExpiresAt.Time.Sub(wc.IssuedAt.Time) > MaxTTL {
		return Claims{}, errInvalid
	}
	id, err := strconv.ParseInt(wc.Subject, 10, 64)
	if err != nil || id <= 0 || strconv.FormatInt(id, 10) != wc.Subject {
		return Claims{}, errInvalid
	}
	if wc.Username != "" {
		canon, err := identity.CanonicalUsername(wc.Username)
		if err != nil || canon != wc.Username {
			return Claims{}, errInvalid
		}
	}
	return Claims{Subject: id, Username: wc.Username, IssuedAt: wc.IssuedAt.Time, ExpiresAt: wc.ExpiresAt.Time}, nil
}
