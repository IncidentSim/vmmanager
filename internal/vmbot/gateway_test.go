package vmbot

import (
	"bufio"
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestGatewayServerBounds(t *testing.T) {
	s := NewGatewayServer(http.NotFoundHandler())
	if s.ReadHeaderTimeout == 0 || s.ReadTimeout == 0 || s.WriteTimeout == 0 || s.IdleTimeout == 0 || s.MaxHeaderBytes == 0 {
		t.Fatalf("unbounded server: %+v", s)
	}
	if s.ErrorLog == nil {
		t.Fatal("server error log must be redirected (request lines carry the secret)")
	}
}

func TestLimitListenerBoundsConnections(t *testing.T) {
	inner, _ := net.Listen("tcp", "127.0.0.1:0")
	l := newLimitListener(inner, 1)
	defer l.Close()
	accepted := make(chan net.Conn, 2)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	c1, _ := net.Dial("tcp", inner.Addr().String())
	defer c1.Close()
	first := <-accepted
	c2, _ := net.Dial("tcp", inner.Addr().String())
	defer c2.Close()
	select {
	case <-accepted:
		t.Fatal("second connection accepted beyond the limit")
	case <-time.After(200 * time.Millisecond):
	}
	first.Close()
	select {
	case <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("slot not released")
	}
}

func TestServeGatewayShutdownClosesWebSockets(t *testing.T) {
	e := newTerm(t, nil)
	e.srv.Close() // use ServeGateway's own listener instead of httptest's
	e.m.Start(context.Background(), target("bot-1", tokA))
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- ServeGateway(ctx, ln, "", "", e.m) }()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("GET /t/" + tokA + "/ws HTTP/1.1\r\nHost: " + pubHost + "\r\nOrigin: https://" + pubHost +
		"\r\nConnection: Upgrade\r\nUpgrade: websocket\r\nSec-WebSocket-Version: 13\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n"))
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 101 {
		t.Fatalf("upgrade: %v %v", resp, err)
	}
	cancel()
	c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := br.ReadByte(); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Fatalf("shutdown must close open WebSockets: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("ServeGateway did not return")
	}
}
