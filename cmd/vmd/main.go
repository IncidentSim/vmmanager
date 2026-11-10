// vmd serves the VM lifecycle facade and/or the host backend over generated gRPC
// with mandatory mutual TLS. It owns process signals; the vmlib library never
// installs signal handlers. It never starts a host service itself: VMMs are
// started by the operator-installed helper configured in backend.local.helper.
//
//	backend-only: backend.kind local + serve_backend
//	facade-only:  backend.kind local|remote + serve_facade
//	combined:     backend.kind local + serve_backend + serve_facade
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"google.golang.org/grpc"

	"example.com/firecracker-postgres-builder/vmlib"
	"example.com/firecracker-postgres-builder/vmlib/local"
	"example.com/firecracker-postgres-builder/vmlib/remote"
)

// deps lets tests replace the two host seams; production leaves it zero.
type deps struct {
	Launcher local.Launcher
	VMM      local.VMM
}

func main() {
	path := flag.String("config", "", "JSON configuration file (see docs/architecture.md)")
	flag.Parse()
	if *path == "" || flag.NArg() != 0 {
		fmt.Fprintln(os.Stderr, "usage: vmd -config FILE")
		os.Exit(2)
	}
	data, err := os.ReadFile(*path)
	if err == nil {
		var cfg *config
		if cfg, err = parseConfig(data); err == nil {
			ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			err = run_(ctx, cfg, deps{}, func(a map[string]string) {
				for k, v := range a {
					fmt.Printf("vmd: %s listening on %s\n", k, v)
				}
			})
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ERROR:", err)
		os.Exit(1)
	}
}

func buildLocal(l *localConfig, d deps) (*local.Backend, error) {
	launcher, vmm := d.Launcher, d.VMM
	if launcher == nil {
		h, err := local.NewHelperLauncher(l.Helper)
		if err != nil {
			return nil, err
		}
		launcher = h
	}
	if vmm == nil {
		vmm = local.SDKVMM{RunDir: l.RunDir}
	}
	var profiles []vmlib.Profile
	for _, p := range l.Profiles {
		profiles = append(profiles, vmlib.Profile{Name: p.Name, VCPU: p.VCPU, MemMiB: p.MemMiB, DiskMiB: p.DiskMiB})
	}
	var bastion *vmlib.Bastion
	if l.Bastion != nil {
		bastion = &vmlib.Bastion{Host: l.Bastion.Host, Port: l.Bastion.Port, User: l.Bastion.User}
	}
	return local.New(local.Config{WorkRoot: l.WorkRoot, ImagesDir: l.ImagesDir, RunDir: l.RunDir, MaxVMs: l.MaxVMs,
		Profiles: profiles, SSHUser: l.SSHUser, Bastion: bastion, Launcher: launcher, VMM: vmm, MinFreeMiB: l.MinFreeMiB})
}

// run_ serves until ctx is cancelled. Cancelling ctx stops listeners only; it
// never stops a VM (guests keep running and are reconciled on the next start).
func run_(ctx context.Context, cfg *config, d deps, ready func(map[string]string)) error {
	var backend vmlib.Backend
	var closeBackend func() error
	switch cfg.Backend.Kind {
	case "local":
		b, err := buildLocal(cfg.Backend.Local, d)
		if err != nil {
			return err
		}
		backend = b
	case "remote":
		r := cfg.Backend.Remote
		tlsCfg, err := remote.ClientTLSFromFiles(r.CA, r.Cert, r.Key, r.ServerName)
		if err != nil {
			return err
		}
		c, err := remote.DialBackend(r.Addr, tlsCfg)
		if err != nil {
			return err
		}
		backend, closeBackend = c, c.Close
	}
	if closeBackend != nil {
		defer closeBackend()
	}

	type server struct {
		name string
		srv  *grpc.Server
		lis  net.Listener
	}
	var servers []server
	start := func(name string, l *listenConfig, register func(*grpc.Server)) error {
		tlsCfg, err := remote.ServerTLSFromFiles(l.CA, l.Cert, l.Key)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		lis, err := net.Listen("tcp", l.Listen)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		s := remote.NewGRPCServer(tlsCfg)
		register(s)
		servers = append(servers, server{name, s, lis})
		return nil
	}
	if cfg.ServeBackend != nil {
		if err := start("backend", cfg.ServeBackend, func(s *grpc.Server) { remote.RegisterBackend(s, backend) }); err != nil {
			return err
		}
	}
	if cfg.ServeFacade != nil {
		svc, err := vmlib.NewService(backend)
		if err != nil {
			return err
		}
		if err := start("facade", cfg.ServeFacade, func(s *grpc.Server) { remote.RegisterFacade(s, svc) }); err != nil {
			for _, s := range servers {
				s.lis.Close()
			}
			return err
		}
	}

	errc := make(chan error, len(servers))
	addrs := map[string]string{}
	for _, s := range servers {
		addrs[s.name] = s.lis.Addr().String()
		go func(s server) { errc <- s.srv.Serve(s.lis) }(s)
	}
	if ready != nil {
		ready(addrs)
	}
	var result error
	select {
	case <-ctx.Done():
	case err := <-errc:
		result = err
	}
	for _, s := range servers {
		stopped := make(chan struct{})
		go func() { s.srv.GracefulStop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(10 * time.Second):
			s.srv.Stop() // closes connections only; no VM is touched
		}
	}
	if errors.Is(result, grpc.ErrServerStopped) {
		result = nil
	}
	return result
}
