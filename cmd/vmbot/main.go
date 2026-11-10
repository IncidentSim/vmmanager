// vmbot is the HTTP-only Telegram frontend. It never starts DBOS, a controller,
// a facade connection, or a terminal process. See docs/bot-api-client.md.
package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/petruekhin/vibe-firecracker/controlclient"
	"github.com/petruekhin/vibe-firecracker/internal/jwtauth"
	"github.com/petruekhin/vibe-firecracker/internal/tgbot"
)

func main() {
	logger := log.New(os.Stderr, "vmbot: ", log.LstdFlags)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if e := run(ctx, os.Args[1:], logger); e != nil {
		logger.Printf("fatal: %v", e)
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, logger *log.Logger) error {
	fs := flag.NewFlagSet("vmbot", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "path to strict frontend JSON config")
	source := fs.String("import-legacy-offset", "", "one-time read-only import from stopped legacy state.json; exits without polling")
	stopped := fs.Bool("legacy-stopped", false, "operator confirms legacy service stopped and disabled")
	check := fs.Bool("check-config", false, "validate config and private credentials without network access")
	if fs.Parse(args) != nil || fs.NArg() != 0 {
		return errors.New("invalid arguments; use -config, -check-config, or -import-legacy-offset with -legacy-stopped")
	}
	if *cfgPath == "" {
		return errors.New("-config is required")
	}
	if *stopped && *source == "" || *source != "" && (!*stopped || *check) {
		return errors.New("offset import requires -legacy-stopped and cannot be combined with -check-config")
	}
	cfg, e := tgbot.LoadConfig(*cfgPath)
	if e != nil {
		return e
	}
	if *source != "" {
		delivery, e := tgbot.OpenDelivery(cfg.DeliveryDir)
		if e != nil {
			return e
		}
		defer delivery.Close()
		if e = delivery.ImportLegacy(*source, *stopped); e != nil {
			return e
		}
		logger.Printf("legacy delivery offset imported; no Telegram requests sent")
		return nil
	}
	token, credential, e := cfg.Secrets()
	if e != nil {
		return e
	}
	// JWT mode: no service credential exists; NewBot signs a per-user token for
	// every API call (client stays nil and is built per user).
	var client *controlclient.Client
	if cfg.JWT != nil {
		if _, e = jwtauth.LoadSigner(cfg.JWT.PrivateKeyFile, cfg.JWT.Issuer, cfg.JWT.Audience); e != nil {
			return errors.New("invalid JWT signing key")
		}
	} else if client, e = controlclient.New(cfg.APIURL, credential, nil); e != nil {
		return e
	}
	if *check {
		logger.Printf("frontend configuration valid; no network requests sent")
		return nil
	}
	delivery, e := tgbot.OpenDelivery(cfg.DeliveryDir)
	if e != nil {
		return e
	}
	defer delivery.Close()
	tg := tgbot.NewTelegramClient("https://api.telegram.org", token, nil)
	bot, e := tgbot.NewBot(cfg, tg, client, delivery)
	if e != nil {
		return e
	}
	logger.Printf("HTTP-only frontend started; VM lifecycle remains in control API")
	return bot.Run(ctx)
}
