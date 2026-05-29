package bridge

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Indexer keeps the RAG index fresh without anyone running `rag index` by hand.
// It shells out to the same incremental indexer (scripts/rag index) on two
// signals: a periodic tick (to pick up new conversation turns) and an explicit
// Trigger() (fired the moment a file lands in the inbox, so a just-sent
// attachment is searchable immediately). The Python side does the real work and
// is incremental — indexed inbox files are skipped and transcripts are read
// forward from a cursor — so a run that finds nothing new is cheap.
type Indexer struct {
	ragCmd   string
	interval time.Duration
	debounce time.Duration
	log      *slog.Logger
	trigger  chan struct{}

	// runFn performs one index pass; overridable in tests.
	runFn func(context.Context) (string, error)
}

func NewIndexer(ragCmd string, interval time.Duration, log *slog.Logger) *Indexer {
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	ix := &Indexer{
		ragCmd:   ragCmd,
		interval: interval,
		debounce: 2 * time.Second,
		log:      log,
		trigger:  make(chan struct{}, 1),
	}
	ix.runFn = ix.defaultRun
	return ix
}

// RAGVenvReady reports whether the local embeddings venv exists, so the bridge
// only starts auto-indexing when RAG is actually set up (no noisy failures
// otherwise). Honors $RAG_VENV, else the default location.
func RAGVenvReady() bool {
	venv := os.Getenv("RAG_VENV")
	if venv == "" {
		home, _ := os.UserHomeDir()
		venv = filepath.Join(home, ".local", "share", "assistant", "rag-venv")
	}
	_, err := os.Stat(filepath.Join(venv, "bin", "python"))
	return err == nil
}

// Trigger requests an index pass. Non-blocking and coalesced: a burst of file
// arrivals collapses into a single pending run.
func (ix *Indexer) Trigger() {
	select {
	case ix.trigger <- struct{}{}:
	default:
	}
}

func (ix *Indexer) Run(ctx context.Context) error {
	ix.log.Info("auto-indexer active", "interval", ix.interval)
	ix.runOnce(ctx, "startup") // catch up on anything missed while we were down

	t := time.NewTicker(ix.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			ix.runOnce(ctx, "tick")
		case <-ix.trigger:
			// Brief debounce so a burst of files batches into one pass.
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(ix.debounce):
			}
			drain(ix.trigger)
			ix.runOnce(ctx, "attachment")
		}
	}
}

func (ix *Indexer) runOnce(ctx context.Context, reason string) {
	out, err := ix.runFn(ctx)
	if err != nil {
		ix.log.Warn("auto-index failed", "reason", reason, "err", err, "out", out)
		return
	}
	// Stay quiet on the common no-op pass; only announce real work.
	if strings.Contains(out, "indexed 0 ") {
		ix.log.Debug("auto-index (no change)", "reason", reason)
		return
	}
	ix.log.Info("auto-index", "reason", reason, "result", out)
}

func (ix *Indexer) defaultRun(ctx context.Context) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	out, err := exec.CommandContext(cctx, ix.ragCmd, "index").CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// drain empties a buffered signal channel without blocking.
func drain(ch chan struct{}) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}
