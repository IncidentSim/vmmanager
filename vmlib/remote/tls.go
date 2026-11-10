// Package remote carries vmlib over generated gRPC with mandatory, verified
// mutual TLS: a backend server/client (the client implements vmlib.Backend) and
// a facade server/client (the client implements vmlib.Lifecycle). There is no
// plaintext or certificate/hostname verification bypass.
package remote

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
)

func caPool(caPEM []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	count := 0
	rest := caPEM
	for len(rest) > 0 {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			if len(trimSpace(rest)) == 0 {
				break
			}
			return nil, errors.New("invalid CA PEM")
		}
		if block.Type != "CERTIFICATE" {
			return nil, errors.New("invalid CA PEM type")
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if !c.IsCA || !c.BasicConstraintsValid || c.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, errors.New("CA file contains a non-CA certificate")
		}
		pool.AddCert(c)
		count++
	}
	if count == 0 {
		return nil, errors.New("empty or invalid CA PEM")
	}
	return pool, nil
}

func trimSpace(b []byte) []byte {
	for len(b) > 0 && (b[0] == '\n' || b[0] == ' ' || b[0] == '\r' || b[0] == '\t') {
		b = b[1:]
	}
	return b
}

// ServerTLSFromPEM builds a TLS 1.3 server config that REQUIRES and verifies a
// client certificate signed by clientCA.
func ServerTLSFromPEM(clientCA, cert, key []byte) (*tls.Config, error) {
	pool, err := caPool(clientCA)
	if err != nil {
		return nil, fmt.Errorf("client CA: %w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("server certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair},
		ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert}, nil
}

// ClientTLSFromPEM builds a TLS 1.3 client config that verifies the server chain
// against serverCA AND the hostname serverName, and presents a client identity.
// An empty serverName is an error: hostname verification cannot be skipped.
func ClientTLSFromPEM(serverCA, cert, key []byte, serverName string) (*tls.Config, error) {
	if serverName == "" {
		return nil, errors.New("server name is required for hostname verification")
	}
	pool, err := caPool(serverCA)
	if err != nil {
		return nil, fmt.Errorf("server CA: %w", err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return nil, fmt.Errorf("client certificate: %w", err)
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: serverName,
		Certificates: []tls.Certificate{pair}}, nil
}

// ServerTLSFromFiles loads PEM files (private keys must live outside Git).
func ServerTLSFromFiles(clientCA, cert, key string) (*tls.Config, error) {
	ca, c, k, err := read3(clientCA, cert, key)
	if err != nil {
		return nil, err
	}
	return ServerTLSFromPEM(ca, c, k)
}

// ClientTLSFromFiles loads PEM files.
func ClientTLSFromFiles(serverCA, cert, key, serverName string) (*tls.Config, error) {
	ca, c, k, err := read3(serverCA, cert, key)
	if err != nil {
		return nil, err
	}
	return ClientTLSFromPEM(ca, c, k, serverName)
}

func read3(a, b, c string) ([]byte, []byte, []byte, error) {
	var out [3][]byte
	for i, p := range []string{a, b, c} {
		if p == "" {
			return nil, nil, nil, errors.New("CA, certificate and key paths are all required")
		}
		d, err := os.ReadFile(p)
		if err != nil {
			return nil, nil, nil, err
		}
		out[i] = d
	}
	return out[0], out[1], out[2], nil
}
