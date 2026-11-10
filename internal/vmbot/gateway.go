package vmbot

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

const maxGatewayConns = 64

// NewGatewayServer wraps h with bounded timeouts. Hijacked (WebSocket)
// connections have their deadlines cleared by net/http, so these bounds apply to
// request parsing and ordinary responses only; open sockets are tracked and
// closed by the Manager.
func NewGatewayServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          log.New(io.Discard, "", 0), // never log request lines (they carry the secret)
		TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// limitListener bounds concurrently open connections (hijacked ones included).
type limitListener struct {
	net.Listener
	sem chan struct{}
}

func newLimitListener(l net.Listener, n int) net.Listener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n)}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

// ServeGateway serves mgr.Handler() on ln (TLS when certFile is set) until ctx
// ends, then closes all terminals (killing ttyd/ssh and open WebSockets) before
// shutting the HTTP server down. It does not touch any VM.
func ServeGateway(ctx context.Context, ln net.Listener, certFile, keyFile string, mgr *Manager) error {
	srv := NewGatewayServer(mgr.Handler())
	ln = newLimitListener(ln, maxGatewayConns)
	errc := make(chan error, 1)
	go func() {
		if certFile != "" {
			errc <- srv.ServeTLS(ln, certFile, keyFile)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	select {
	case err := <-errc:
		mgr.Close()
		return err
	case <-ctx.Done():
	}
	mgr.Close()
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		srv.Close()
	}
	if err := <-errc; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
