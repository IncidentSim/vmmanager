package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/remote"
)

func clientPKI(t *testing.T) (string, string, string) {
	t.Helper()
	dir := t.TempDir()
	key, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	root := &x509.Certificate{SerialNumber: big.NewInt(1), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	ca, e := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, NotBefore: root.NotBefore, NotAfter: root.NotAfter}
	cert, e := x509.CreateCertificate(rand.Reader, leaf, root, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	kb, e := x509.MarshalECPrivateKey(key)
	if e != nil {
		t.Fatal(e)
	}
	paths := []string{filepath.Join(dir, "ca"), filepath.Join(dir, "cert"), filepath.Join(dir, "key")}
	for i, b := range []*pem.Block{{Type: "CERTIFICATE", Bytes: ca}, {Type: "CERTIFICATE", Bytes: cert}, {Type: "EC PRIVATE KEY", Bytes: kb}} {
		if e := os.WriteFile(paths[i], pem.EncodeToMemory(b), 0600); e != nil {
			t.Fatal(e)
		}
	}
	return paths[0], paths[1], paths[2]
}

func TestClientStartsPollsAndDownloads(t *testing.T) {
	ca, cert, key := clientPKI(t)
	cfg, e := remote.ServerTLS(ca, cert, key)
	if e != nil {
		t.Fatal(e)
	}
	m, e := remote.NewManager(t.TempDir(), func(ctx context.Context, p string, w io.Writer) error {
		io.WriteString(w, "provisioning\n")
		return os.WriteFile(p, []byte("verified image"), 0600)
	})
	if e != nil {
		t.Fatal(e)
	}
	defer m.Close()
	s := httptest.NewUnstartedServer(m)
	s.TLS = cfg
	s.StartTLS()
	defer s.Close()
	flags := []string{"-endpoint", s.URL, "-tls-ca", ca, "-tls-cert", cert, "-tls-key", key}
	output := filepath.Join(t.TempDir(), "image")
	var log bytes.Buffer
	if e := run(context.Background(), append(flags, "-output", output), &log); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(log.String(), "provisioning") || !strings.Contains(log.String(), "succeeded") {
		t.Fatal(log.String())
	}
	data, e := os.ReadFile(output)
	if e != nil || string(data) != "verified image" {
		t.Fatalf("%q %v", data, e)
	}
}

func TestClientRejectsUnsafeOptions(t *testing.T) {
	for _, args := range [][]string{{}, {"-endpoint", "http://localhost:19443"}, {"-endpoint", "https://localhost:19443/path"}, {"-endpoint", "https://user@localhost:19443"}, {"-endpoint", "https://localhost:19443", "-status", "../bad"}} {
		if e := run(context.Background(), args, io.Discard); e == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}
