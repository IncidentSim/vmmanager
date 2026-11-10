package remote

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testPKI(t *testing.T) (ca, cert, key string) {
	t.Helper()
	dir := t.TempDir()
	k, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	root := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, e := x509.CreateCertificate(rand.Reader, root, root, &k.PublicKey, k)
	if e != nil {
		t.Fatal(e)
	}
	ca = filepath.Join(dir, "ca.pem")
	os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	root, _ = x509.ParseCertificate(der)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"localhost"}, NotBefore: root.NotBefore, NotAfter: root.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der, e = x509.CreateCertificate(rand.Reader, leaf, root, &k.PublicKey, k)
	if e != nil {
		t.Fatal(e)
	}
	cert = filepath.Join(dir, "cert.pem")
	os.WriteFile(cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600)
	kb, _ := x509.MarshalECPrivateKey(k)
	key = filepath.Join(dir, "key.pem")
	os.WriteFile(key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: kb}), 0600)
	return
}
func TestMutualTLS(t *testing.T) {
	ca, cert, key := testPKI(t)
	serverTLS, e := ServerTLS(ca, cert, key)
	if e != nil {
		t.Fatal(e)
	}
	m, _ := NewManager(t.TempDir(), func(ctx context.Context, p string, w io.Writer) error {
		return os.WriteFile(p, []byte("real authenticated result"), 0600)
	})
	defer m.Close()
	s := httptest.NewUnstartedServer(m)
	s.TLS = serverTLS
	s.StartTLS()
	defer s.Close()
	good, e := ClientTLS(ca, cert, key)
	if e != nil {
		t.Fatal(e)
	}
	good.ServerName = "localhost"
	c := &Client{Endpoint: s.URL, HTTP: &http.Client{Transport: &http.Transport{TLSClientConfig: good}}}
	j, e := c.Start(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	if waitJob(t, c, j.ID).State != "succeeded" {
		t.Fatal("build failed")
	}
	_, rogueCert, rogueKey := testPKI(t)
	for _, tc := range []struct {
		name   string
		config *tls.Config
	}{
		{"TLS 1.2", func() *tls.Config {
			v := good.Clone()
			v.MinVersion = tls.VersionTLS12
			v.MaxVersion = tls.VersionTLS12
			return v
		}()},
		{"no certificate", &tls.Config{RootCAs: good.RootCAs, ServerName: "localhost", MinVersion: tls.VersionTLS13}},
		{"rogue certificate", func() *tls.Config {
			v := good.Clone()
			pair, _ := tls.LoadX509KeyPair(rogueCert, rogueKey)
			v.Certificates = []tls.Certificate{pair}
			return v
		}()}, {"wrong hostname", func() *tls.Config { v := good.Clone(); v.ServerName = "evil.invalid"; return v }()}, {"rogue server CA", func() *tls.Config { v := good.Clone(); v.RootCAs = x509.NewCertPool(); return v }()}} {
		t.Run(tc.name, func(t *testing.T) {
			hc := &http.Client{Transport: &http.Transport{TLSClientConfig: tc.config}, Timeout: time.Second}
			resp, err := hc.Get(s.URL + "/v1/builds/" + j.ID)
			if err == nil {
				resp.Body.Close()
				t.Fatal("TLS accepted invalid peer")
			}
		})
	}
}
func TestTLSFailClosed(t *testing.T) {
	ca, cert, key := testPKI(t)
	for _, paths := range [][3]string{{"", cert, key}, {ca, "", key}, {ca, cert, ""}, {cert, cert, key}, {"/missing", cert, key}} {
		if _, e := ServerTLS(paths[0], paths[1], paths[2]); e == nil {
			t.Fatal("accepted invalid TLS files", paths)
		}
	}
}
