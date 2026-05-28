package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/wildcommitter/claudecgwd/internal/bridge"
	"github.com/wildcommitter/claudecgwd/internal/claude"
	"github.com/wildcommitter/claudecgwd/internal/config"
)

func main() {
	var configPath string
	flag.StringVar(&configPath, "config", "config.yaml", "path to config.yaml")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if err := run(configPath, logger); err != nil {
		logger.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(configPath string, logger *slog.Logger) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	driver := claude.New(cfg.Claude, logger.With("component", "driver"))
	if err := driver.Start(ctx); err != nil {
		return fmt.Errorf("start driver: %w", err)
	}
	defer driver.Close()

	inbound := make(chan bridge.Inbound, cfg.Router.InboundBuffer)

	router := bridge.NewRouter(
		driver,
		inbound,
		logger.With("component", "router"),
		time.Duration(cfg.Router.WatchdogTimeoutS)*time.Second,
	)

	var wg sync.WaitGroup

	if cfg.Telegram.Enabled() {
		tg := bridge.NewTelegram(cfg.Telegram, logger.With("component", "telegram"), inbound)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := tg.Run(ctx); err != nil {
				logger.Error("telegram exited", "err", err)
				cancel()
			}
		}()
	}

	if cfg.IRC.Enabled() {
		ir := bridge.NewIRC(cfg.IRC, logger.With("component", "irc"), inbound)
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := ir.Run(ctx); err != nil {
				logger.Error("irc exited", "err", err)
				cancel()
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = router.Run(ctx)
	}()

	// Block until ctx is cancelled or the claude child exits.
	select {
	case <-ctx.Done():
		logger.Info("shutting down")
	case <-driver.Done():
		logger.Error("claude child exited; shutting down")
		cancel()
	}
	wg.Wait()
	return nil
}
