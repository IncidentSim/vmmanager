package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/remote"
)

// No CommandContext: SIGKILL would bypass receipt-based cleanup. The slot stays
// occupied until Wait returns, including when a child cannot finish cleanup.
func buildChild(executable, base, key, socket string) remote.Runner {
	return func(ctx context.Context, output string, logs io.Writer) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		cmd := exec.Command(executable, "-base", base, "-ssh-key", key, "-socket", socket, "-output", output)
		cmd.Stdout = logs
		cmd.Stderr = logs
		if err := cmd.Start(); err != nil {
			return err
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err := <-done:
			return err
		case <-ctx.Done():
			// Signal only the child, not its SSH subprocesses or a replacement VMM.
			_ = cmd.Process.Signal(syscall.SIGTERM)
			return <-done
		}
	}
}

func serveBuilds(ctx context.Context, listen, ca, cert, key, jobDir, base, sshKey, socket string) error {
	tlsConfig, err := remote.ServerTLS(ca, cert, key)
	if err != nil {
		return err
	}
	if jobDir == "" {
		return fmt.Errorf("job-dir is required")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	manager, err := remote.NewManager(jobDir, buildChild(executable, base, sshKey, socket))
	if err != nil {
		return err
	}
	defer manager.Close()
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: manager, TLSConfig: tlsConfig, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16384}
	done := make(chan error, 1)
	go func() { done <- server.ServeTLS(listener, "", "") }()
	select {
	case err = <-done:
	case <-ctx.Done():
		// Closing HTTP does not kill jobs; Manager.Close signals and waits for cleanup.
		_ = server.Close()
		err = <-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
