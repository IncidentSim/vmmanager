package remote

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
)

func loadTLS(ca, cert, key string) (*tls.Config, error) {
	if ca == "" || cert == "" || key == "" {
		return nil, fmt.Errorf("tls-ca, tls-cert and tls-key are required")
	}
	data, err := os.ReadFile(ca)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	count := 0
	for len(data) > 0 {
		block, rest := pem.Decode(data)
		if block == nil {
			return nil, fmt.Errorf("invalid CA PEM")
		}
		data = rest
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("invalid CA PEM type")
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		if !c.IsCA || !c.BasicConstraintsValid || c.KeyUsage&x509.KeyUsageCertSign == 0 {
			return nil, fmt.Errorf("CA file contains a non-CA certificate")
		}
		pool.AddCert(c)
		count++
	}
	if count == 0 {
		return nil, fmt.Errorf("empty CA file")
	}
	pair, err := tls.LoadX509KeyPair(cert, key)
	if err != nil {
		return nil, err
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, RootCAs: pool, ClientCAs: pool}, nil
}

func ServerTLS(ca, cert, key string) (*tls.Config, error) {
	c, err := loadTLS(ca, cert, key)
	if err != nil {
		return nil, err
	}
	c.ClientAuth = tls.RequireAndVerifyClientCert
	return c, nil
}
func ClientTLS(ca, cert, key string) (*tls.Config, error) { return loadTLS(ca, cert, key) }
