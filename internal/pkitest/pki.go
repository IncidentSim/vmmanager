// Package pkitest creates ephemeral in-memory PKI for tests. Never use it for
// real deployments; production keys are generated outside Git.
package pkitest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"
)

// CA is an ephemeral certificate authority.
type CA struct {
	Cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	CertPEM []byte
}

// Pair is a PEM certificate and private key.
type Pair struct{ CertPEM, KeyPEM []byte }

func serial(t testing.TB) *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 100))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func NewCA(t testing.TB, name string) *CA {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(t), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	return &CA{Cert: cert, key: key, CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (c *CA) issue(t testing.TB, cn string, dns []string, ips []net.IP, eku x509.ExtKeyUsage) Pair {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: serial(t), Subject: pkix.Name{CommonName: cn}, DNSNames: dns, IPAddresses: ips,
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{eku},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, c.Cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatal(err)
	}
	kd, _ := x509.MarshalECPrivateKey(key)
	return Pair{
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		KeyPEM:  pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kd}),
	}
}

// Server issues a serverAuth certificate for the DNS names (and 127.0.0.1).
func (c *CA) Server(t testing.TB, dns ...string) Pair {
	return c.issue(t, "server", dns, []net.IP{net.ParseIP("127.0.0.1")}, x509.ExtKeyUsageServerAuth)
}

// Client issues a clientAuth certificate.
func (c *CA) Client(t testing.TB, cn string) Pair {
	return c.issue(t, cn, nil, nil, x509.ExtKeyUsageClientAuth)
}
