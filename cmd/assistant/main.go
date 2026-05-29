package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
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

	// Tracks project directories across /project switches so a name can be
	// wildcard-resolved later (paired with the project-tracker skill). Uses the
	// default store path (~/.local/share/assistant/projects.tsv).
	projects := bridge.NewProjectRegistry("")

	// /search shells out to scripts/rag. Resolve it from the configured (stable)
	// workdir so it keeps working after a /project switch moves the session.
	ragCmd := filepath.Join(cfg.Claude.Workdir, "scripts", "rag")

	router := bridge.NewRouter(
		driver,
		driver, // also the SessionController for /new, /project, /status
		projects,
		ragCmd,
		inbound,
		logger.With("component", "router"),
		time.Duration(cfg.Router.WatchdogTimeoutS)*time.Second,
	)

	var wg sync.WaitGroup

	stt := bridge.NewTranscriber(cfg.STT)

	var tg *bridge.Telegram
	if cfg.Telegram.Enabled() {
		tg = bridge.NewTelegram(cfg.Telegram, logger.With("component", "telegram"), inbound, cfg.Files.InboxDir, stt)
		wg.Add(1)
		go func() {
			defer wg.Done()
			superviseBridge(ctx, "telegram", logger, tg.Run)
		}()
	}

	var wa *bridge.WhatsApp
	if cfg.WhatsApp.Enabled {
		// Deliver the WhatsApp pairing QR through Telegram as a PNG.
		var sink bridge.QRSink
		if tg != nil {
			sink = tg.SendQRToOwner
		}
		wa = bridge.NewWhatsApp(cfg.WhatsApp, logger.With("component", "whatsapp"), inbound, sink, cfg.Files.InboxDir, stt)
		wg.Add(1)
		go func() {
			defer wg.Done()
			superviseBridge(ctx, "whatsapp", logger, wa.Run)
		}()
	}

	// Proactive-notify path: anything written to the notify FIFO is fanned out
	// to every configured surface (so a watcher finishing can ping the user
	// even with no inbound turn to reply to).
	var pushers []bridge.Pusher
	if tg != nil {
		pushers = append(pushers, tg.SendTextToOwner)
	}
	if wa != nil {
		pushers = append(pushers, wa.PushToOwner)
	}
	if len(pushers) > 0 {
		notifier := bridge.NewNotifier("", logger.With("component", "notify"), pushers...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = notifier.Run(ctx)
		}()

		// Scheduled reminders ride the same push surfaces: scripts/remind
		// appends to the store and the scheduler fires each one when it's due.
		scheduler := bridge.NewScheduler(cfg.Reminders.StorePath, logger.With("component", "scheduler"), pushers...)
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = scheduler.Run(ctx)
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

// superviseBridge runs a bridge's Run loop and restarts it with exponential
// backoff if it returns unexpectedly — e.g. a network outage at startup makes
// the initial connect fail. Previously any such error cancelled the whole
// process; now only a real shutdown (ctx cancelled) stops the supervisor, so a
// transient outage no longer takes the assistant down. A bridge that ran
// healthily for a while before failing resets the backoff so it reconnects
// promptly.
func superviseBridge(ctx context.Context, name string, log *slog.Logger, run func(context.Context) error) {
	const (
		base       = time.Second
		maxBackoff = 30 * time.Second
		healthyFor = time.Minute
	)
	attempt := 0
	for {
		start := time.Now()
		err := run(ctx)
		if ctx.Err() != nil {
			return // shutting down — expected
		}
		if time.Since(start) >= healthyFor {
			attempt = 0 // ran healthily, treat the next failure as fresh
		}
		shift := attempt
		if shift > 5 { // clamp so the shift can't overflow on a long outage
			shift = 5
		}
		backoff := base << shift
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
		attempt++
		if err != nil {
			log.Error(name+" bridge exited; restarting", "err", err, "restart_in", backoff)
		} else {
			log.Warn(name+" bridge returned without error; restarting", "restart_in", backoff)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
	}
}
