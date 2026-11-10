// vmbot is a Telegram bot that owns up to max_vms VMs on a vmd facade (mTLS) and
// exposes a revocable capability-link web terminal (ttyd -> ssh) per VM. See
// docs/vmbot.md, docs/adr/0002-telegram-single-vm-bot.md and
// docs/adr/0003-vmbot-multi-vm.md.
//
//	vmbot -config /path/to/vmbot.json
//
// Process exit never deletes the VM: a restart reconciles and keeps it.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/petruekhin/vibe-firecracker/internal/vmbot"
	"github.com/petruekhin/vibe-firecracker/vmlib/remote"
)

const telegramAPI = "https://api.telegram.org"

func main() {
	cfgPath := flag.String("config", "", "path to the JSON configuration (no secrets)")
	flag.Parse()
	logger := log.New(os.Stderr, "vmbot: ", log.LstdFlags)
	if err := run(*cfgPath, logger); err != nil {
		logger.Printf("fatal: %v", err)
		os.Exit(1)
	}
}

func run(cfgPath string, logger *log.Logger) error {
	if cfgPath == "" {
		return errors.New("-config is required")
	}
	cfg, err := vmbot.LoadConfig(cfgPath)
	if err != nil {
		return err
	}
	token, err := cfg.ReadToken()
	if err != nil {
		return err
	}
	store, err := vmbot.OpenStore(cfg.StateDir)
	if err != nil {
		return fmt.Errorf("state: %w", err)
	}
	defer store.Close()
	if m := store.Migration(); m != nil {
		logger.Printf("state migrated v1 -> v2; v1 backup kept at %s (restoring it is only safe before any v2 change: "+
			"later it loses the update offset and any VM created since; see docs/adr/0003-vmbot-multi-vm.md)", m.BackupPath)
	}

	tlsCfg, err := remote.ClientTLSFromFiles(cfg.Facade.CAFile, cfg.Facade.CertFile, cfg.Facade.KeyFile, cfg.Facade.ServerName)
	if err != nil {
		return fmt.Errorf("facade tls: %w", err)
	}
	facade, err := remote.DialFacade(cfg.Facade.Addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("facade dial: %w", err)
	}
	defer facade.Close()

	t := cfg.Terminal
	mgr, err := vmbot.NewManager(vmbot.ManagerConfig{
		TTYDPath: t.TTYDPath, SSHPath: t.SSHPath, SSHKeyFile: t.SSHKeyFile, StateDir: cfg.StateDir,
		PublicOrigin: t.PublicOrigin, MaxClients: t.MaxClients, MaxSessions: cfg.MaxVMs, UseBastion: t.SSHUseBastion,
	})
	if err != nil {
		return err
	}
	// Recover can start terminal projections before Listen succeeds. Always
	// drain them on an early startup error too, not only via ServeGateway.
	defer mgr.Close()
	ctl := vmbot.NewController(facade, store, mgr, vmbot.ControllerConfig{
		ImageID: cfg.VM.ImageID, Profile: cfg.VM.Profile, PublicOrigin: t.PublicOrigin, MaxVMs: cfg.MaxVMs, Logger: logger,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// The state-dir lifetime flock (OpenStore) is already held, so no second
	// instance can share the orchestration database. Start resumes pending
	// workflows; Recover re-submits intents and rebuilds terminal projections.
	if err := ctl.Start(ctx); err != nil {
		return fmt.Errorf("orchestration: %w", err)
	}
	defer func() {
		if err := ctl.Close(); err != nil {
			logger.Printf("orchestration shutdown: %v", err)
		}
	}()
	if err := ctl.Recover(ctx); err != nil {
		logger.Printf("startup reconcile incomplete (VM kept; will retry on next operation): %v", err)
	}

	ln, err := net.Listen("tcp", t.ListenAddr)
	if err != nil {
		return fmt.Errorf("terminal listen: %w", err)
	}
	gwDone := make(chan error, 1)
	go func() { gwDone <- vmbot.ServeGateway(ctx, ln, t.TLSCertFile, t.TLSKeyFile, mgr) }()

	api := vmbot.NewTelegramClient(telegramAPI, token, &http.Client{Timeout: 45 * time.Second})
	bot := vmbot.NewBot(cfg.AllowedUserID, api, ctl, store, logger)
	botDone := make(chan struct{})
	go func() { bot.Run(ctx); close(botDone) }()

	logger.Printf("started (terminal gateway on %s)", t.ListenAddr)
	var gwErr error
	gwReceived := false
	select {
	case <-ctx.Done():
	case gwErr = <-gwDone:
		gwReceived = true
		cancel()
	}
	<-botDone
	if !gwReceived {
		gwErr = <-gwDone
	}
	logger.Printf("stopped; the VM, if any, was left in place")
	if gwErr != nil {
		return fmt.Errorf("terminal gateway: %w", gwErr)
	}
	return nil
}
