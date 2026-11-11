// vmapi is the standalone authenticated control API. It alone owns durable
// application state and DBOS execution, and it embeds the single-host VM service
// directly: vmlib.Service over vmlib/local.Backend, opened in-process from the
// private "backend" policy. There is no VM RPC transport. No Telegram credentials
// are read here.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/controlapi"
	"github.com/petruekhin/vibe-firecracker/internal/identity"
	"github.com/petruekhin/vibe-firecracker/internal/localcfg"
	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib"
)

func main() {
	path := flag.String("config", "", "path to API JSON configuration")
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, *path, openLocalBackend); e != nil {
		log.Print("vmapi: ", e)
		os.Exit(1)
	}
}

// backendOpener opens the VM backend for a validated private policy and returns
// the function that releases it. Release only drops the filesystem lifetime
// locks; it never stops or deletes a VM. Production uses openLocalBackend; tests
// substitute an in-memory backend (the deployed binary never contains a fake).
type backendOpener func(*localcfg.JSONConfig) (vmlib.Backend, func() error, error)

func openLocalBackend(c *localcfg.JSONConfig) (vmlib.Backend, func() error, error) {
	b, e := localcfg.Open(c, localcfg.Seams{})
	if e != nil {
		return nil, nil, e
	}
	return b, b.Close, nil
}

type unavailableTerminals struct{}

func (unavailableTerminals) Start(context.Context, vmbot.TerminalTarget) error {
	return errors.New("terminal not configured")
}
func (unavailableTerminals) Revoke(string) {}
func (unavailableTerminals) Close()        {}

type degradeSetter interface{ SetDegraded(bool) }

// recoverOrDegrade runs startup repair. A failure (for example an operation that
// could not be re-enqueued) stays durable as pending/failed state and must remain
// observable through the API, so it is reported and the API serves "degraded"
// instead of exiting into a crash loop that repairs nothing. The error text is
// deliberately not logged: it may embed paths or backend detail.
func recoverOrDegrade(recoverFn func() error, h degradeSetter, logs io.Writer) {
	if recoverFn() != nil {
		h.SetDegraded(true)
		fmt.Fprintln(logs, "vmapi: startup operation recovery incomplete; serving degraded, restart re-runs repair")
		return
	}
	h.SetDegraded(false)
}
func run(ctx context.Context, path string, open backendOpener) error {
	if path == "" {
		return errors.New("-config is required")
	}
	cfg, e := controlapi.LoadConfig(path)
	if e != nil {
		return e
	}
	backend, closeBackend, e := open(cfg.Backend)
	if e != nil {
		return errors.New("cannot open local backend (is a previous vmd or API still holding the VM roots?)")
	}
	// Closed last: the backend outlives the controller and its operations.
	defer closeBackend()
	svc, e := vmlib.NewService(backend)
	if e != nil {
		return errors.New("cannot start VM service")
	}
	st, e := vmbot.OpenControlStore(cfg.StateDir, cfg.LegacyOwnerUserID)
	if e != nil {
		return e
	}
	defer st.Close()
	var term vmbot.Terminals = unavailableTerminals{}
	var manager *vmbot.Manager
	if cfg.Terminal.PublicOrigin != "" {
		tc := cfg.Terminal
		manager, e = vmbot.NewManager(vmbot.ManagerConfig{TTYDPath: tc.TTYDPath, SSHPath: tc.SSHPath, SSHKeyFile: tc.SSHKeyFile, StateDir: cfg.StateDir, PublicOrigin: tc.PublicOrigin, MaxClients: tc.MaxClients, MaxSessions: cfg.MaxVMs, UseBastion: tc.SSHUseBastion})
		if e != nil {
			return errors.New("invalid terminal configuration")
		}
		term = manager
	}
	defer term.Close()
	ctl := vmbot.NewController(svc, st, term, vmbot.ControllerConfig{Profile: cfg.DefaultProfile, PublicOrigin: cfg.Terminal.PublicOrigin})
	ctl.ConfigureControl(cfg.Policy())
	var jwtAuth *controlapi.JWTAuth
	if cfg.JWT != nil {
		// Public key only: the API never receives signing material.
		verifier, e := cfg.Verifier()
		if e != nil {
			return errors.New("invalid jwt public key")
		}
		names, e := cfg.CanonicalUsernames()
		if e != nil {
			return errors.New("invalid username allowlist")
		}
		bindings, e := identity.OpenBindings(filepath.Join(cfg.StateDir, "username-bindings.json"))
		if e != nil {
			return errors.New("cannot open username bindings")
		}
		jwtAuth = &controlapi.JWTAuth{Verifier: verifier, Bindings: bindings, Usernames: names}
	}
	h, e := controlapi.NewWithJWT(ctl, cfg.Credentials, jwtAuth)
	if e != nil {
		return e
	}
	if e = ctl.Start(ctx); e != nil {
		return e
	}
	defer ctl.Close()
	recoverOrDegrade(ctl.RecoverControl, h, os.Stderr)
	listener, e := net.Listen("tcp", cfg.ListenAddr)
	if e != nil {
		return errors.New("cannot bind API listener")
	}
	defer listener.Close()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 * 1024}
	done := make(chan error, 2)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if manager != nil {
		tc := cfg.Terminal
		ln, e := net.Listen("tcp", tc.ListenAddr)
		if e != nil {
			return errors.New("cannot bind terminal listener")
		}
		gateway := manager.Handler()
		if cfg.ExposesAPI() {
			// Same HTTPS origin, fixed in-process routes: /v1, /healthz, /docs,
			// /openapi.yaml and Swagger assets go to the API; /t/ stays terminal.
			gateway = vmbot.ComposeGateway(manager, h)
		}
		go func() { done <- vmbot.ServeGatewayHandler(ctx, ln, tc.TLSCertFile, tc.TLSKeyFile, manager, gateway) }()
	}
	go func() {
		if cfg.TLSCertFile != "" {
			done <- srv.ServeTLS(listener, cfg.TLSCertFile, cfg.TLSKeyFile)
		} else {
			done <- srv.Serve(listener)
		}
	}()
	select {
	case <-ctx.Done():
	case e = <-done:
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 10*time.Second)
	defer stop()
	_ = srv.Shutdown(shutdown)
	if e != nil && !errors.Is(e, http.ErrServerClosed) {
		return errors.New("HTTP server stopped")
	}
	return nil
}
