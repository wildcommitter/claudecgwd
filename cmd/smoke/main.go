// Smoke runner for the Claude PTY driver — no bots required.
//
//	smoke -session <uuid> -prompt "say hi in 3 words"
//
// Spawns claude under the driver, sends one prompt, prints the extracted
// response, exits. Useful for verifying the driver works against the
// installed claude binary before wiring up Telegram/IRC.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wildcommitter/claudecgwd/internal/claude"
	"github.com/wildcommitter/claudecgwd/internal/config"
)

func main() {
	var (
		session = flag.String("session", "", "claude session UUID (required)")
		binary  = flag.String("binary", "claude", "path to claude binary")
		workdir = flag.String("workdir", ".", "working directory for claude")
		prompt  = flag.String("prompt", "Reply with exactly: hi from smoke", "prompt to send")
		debug   = flag.Bool("debug", false, "verbose logging")
	)
	flag.Parse()
	if *session == "" {
		fmt.Fprintln(os.Stderr, "missing -session <uuid>")
		os.Exit(2)
	}

	lvl := slog.LevelInfo
	if *debug {
		lvl = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	cfg := config.ClaudeConfig{
		Binary:         *binary,
		Workdir:        *workdir,
		SessionID:      *session,
		PermissionMode: "bypassPermissions",
		PtyCols:        200,
		PtyRows:        500,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	driver := claude.New(cfg, logger)
	if err := driver.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "driver start: %v\n", err)
		os.Exit(1)
	}
	defer driver.Close()

	sendCtx, sendCancel := context.WithTimeout(ctx, 3*time.Minute)
	defer sendCancel()
	reply, err := driver.Send(sendCtx, *prompt)
	if err != nil {
		fmt.Fprintf(os.Stderr, "send: %v\n", err)
		os.Exit(1)
	}

	fmt.Println("===== REPLY =====")
	fmt.Println(reply)
	fmt.Println("===== END =====")
}
