package remote_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"

	"example.com/firecracker-postgres-builder/vmlib"
	"example.com/firecracker-postgres-builder/vmlib/remote"
	"example.com/firecracker-postgres-builder/vmlib/remote/pkitest"
	"example.com/firecracker-postgres-builder/vmlib/vmlibtest"
)

const host = "vmd.test"

type env struct {
	ca      *pkitest.CA
	server  pkitest.Pair
	client  pkitest.Pair
	addr    string
	backend *vmlibtest.Backend
}

// startBackend serves a fake backend over real loopback gRPC with mandatory mTLS.
func startBackend(t *testing.T) *env {
	t.Helper()
	ca := pkitest.NewCA(t, "test-ca")
	e := &env{ca: ca, server: ca.Server(t, host), client: ca.Client(t, "operator"), backend: vmlibtest.New(2, "base")}
	cfg, err := remote.ServerTLSFromPEM(ca.CertPEM, e.server.CertPEM, e.server.KeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	srv := remote.NewGRPCServer(cfg)
	remote.RegisterBackend(srv, e.backend)
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(lis)
	t.Cleanup(srv.Stop)
	e.addr = lis.Addr().String()
	return e
}

func dial(t *testing.T, e *env, ca []byte, pair pkitest.Pair, serverName string) (*remote.BackendClient, error) {
	t.Helper()
	cfg, err := remote.ClientTLSFromPEM(ca, pair.CertPEM, pair.KeyPEM, serverName)
	if err != nil {
		return nil, err
	}
	return remote.DialBackend(e.addr, cfg)
}

func caps(c vmlib.Backend) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := c.Capabilities(ctx)
	return err
}

func TestMutualTLSSucceeds(t *testing.T) {
	e := startBackend(t)
	c, err := dial(t, e, e.ca.CertPEM, e.client, host)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := caps(c); err != nil {
		t.Fatal(err)
	}
}

func TestRejectsClientWithoutCertificate(t *testing.T) {
	e := startBackend(t)
	if _, err := remote.ClientTLSFromPEM(e.ca.CertPEM, nil, nil, host); err == nil {
		t.Error("client config without identity must be refused by the constructor")
	}
	// Bypass the constructor to prove the SERVER enforces client auth itself.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(e.ca.CertPEM)
	c, err := remote.DialBackend(e.addr, &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: host})
	if err == nil {
		defer c.Close()
		err = caps(c)
	}
	if err == nil {
		t.Fatal("server accepted a client without a certificate")
	}
}

func TestRejectsClientCertFromWrongCA(t *testing.T) {
	e := startBackend(t)
	rogue := pkitest.NewCA(t, "rogue")
	c, err := dial(t, e, e.ca.CertPEM, rogue.Client(t, "evil"), host)
	if err == nil {
		defer c.Close()
		err = caps(c)
	}
	if err == nil {
		t.Fatal("server accepted a client certificate from an untrusted CA")
	}
}

func TestClientRejectsServerFromWrongCA(t *testing.T) {
	e := startBackend(t)
	rogue := pkitest.NewCA(t, "rogue")
	c, err := dial(t, e, rogue.CertPEM, e.client, host) // client trusts the wrong CA
	if err == nil {
		defer c.Close()
		err = caps(c)
	}
	if err == nil {
		t.Fatal("client trusted a server not signed by its CA")
	}
}

func TestClientRejectsHostnameMismatch(t *testing.T) {
	e := startBackend(t)
	c, err := dial(t, e, e.ca.CertPEM, e.client, "other.test")
	if err == nil {
		defer c.Close()
		err = caps(c)
	}
	if err == nil {
		t.Fatal("client accepted a server certificate for a different hostname")
	}
}

func TestConfigConstructorsFailClosed(t *testing.T) {
	ca := pkitest.NewCA(t, "ca")
	s, c := ca.Server(t, host), ca.Client(t, "c")
	if _, err := remote.ClientTLSFromPEM(ca.CertPEM, c.CertPEM, c.KeyPEM, ""); err == nil {
		t.Error("empty server name must be rejected (no hostname bypass)")
	}
	if _, err := remote.ClientTLSFromPEM([]byte("junk"), c.CertPEM, c.KeyPEM, host); err == nil {
		t.Error("bad CA accepted")
	}
	if _, err := remote.ServerTLSFromPEM(nil, s.CertPEM, s.KeyPEM); err == nil {
		t.Error("server without client CA accepted")
	}
	if _, err := remote.ServerTLSFromPEM(ca.CertPEM, nil, nil); err == nil {
		t.Error("server without certificate accepted")
	}
	cfg, _ := remote.ServerTLSFromPEM(ca.CertPEM, s.CertPEM, s.KeyPEM)
	if cfg.ClientAuth.String() != "RequireAndVerifyClientCert" || cfg.MinVersion < 0x0304 {
		t.Errorf("weak server TLS config: %v %x", cfg.ClientAuth, cfg.MinVersion)
	}
	ccfg, _ := remote.ClientTLSFromPEM(ca.CertPEM, c.CertPEM, c.KeyPEM, host)
	if ccfg.InsecureSkipVerify || ccfg.ServerName != host {
		t.Error("client config weakens verification")
	}
}
