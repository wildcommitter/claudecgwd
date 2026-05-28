// Spawn claude under a PTY and dump raw bytes (and the post-strip text) to a
// file for inspection. No driver logic involved.
//
//	ptydump -session <uuid> -duration 8s -out /tmp/claude-pty.log
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
	"github.com/hinshun/vt10x"
)

func main() {
	session := flag.String("session", "", "session UUID")
	binary := flag.String("binary", "claude", "claude binary path")
	workdir := flag.String("workdir", ".", "workdir")
	dur := flag.Duration("duration", 8*time.Second, "how long to capture")
	out := flag.String("out", "/tmp/claude-pty.log", "raw output path")
	sendAfter := flag.Duration("send-after", 0, "if >0, send the -prompt after this delay")
	prompt := flag.String("prompt", "", "prompt to send")
	flag.Parse()
	if *session == "" {
		fmt.Fprintln(os.Stderr, "missing -session")
		os.Exit(2)
	}

	args := []string{"--session-id", *session, "--dangerously-skip-permissions"}
	absWorkdir, _ := filepath.Abs(*workdir)
	slug := strings.ReplaceAll(absWorkdir, "/", "-")
	sessFile := filepath.Join(os.Getenv("HOME"), ".claude", "projects", slug, *session+".jsonl")
	if _, err := os.Stat(sessFile); err == nil {
		args = []string{"--resume", *session, "--dangerously-skip-permissions"}
	}
	cmd := exec.Command(*binary, args...)
	cmd.Dir = *workdir
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	cols, rows := 200, 500
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: uint16(cols), Rows: uint16(rows)})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer f.Close()

	rawOut, err := os.Create(*out)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	defer rawOut.Close()

	strippedOut, _ := os.Create(*out + ".stripped.txt")
	defer strippedOut.Close()

	term := vt10x.New(vt10x.WithSize(cols, rows))

	done := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := f.Read(buf)
			if n > 0 {
				_, _ = rawOut.Write(buf[:n])
				_, _ = strippedOut.WriteString(ansi.Strip(string(buf[:n])))
				term.Write(buf[:n])
			}
			if err != nil {
				if err != io.EOF {
					fmt.Fprintf(os.Stderr, "read: %v\n", err)
				}
				close(done)
				return
			}
		}
	}()

	if *sendAfter > 0 && *prompt != "" {
		go func() {
			time.Sleep(*sendAfter)
			payload := "\x1b[200~" + *prompt + "\x1b[201~\r"
			_, _ = f.WriteString(payload)
		}()
	}

	select {
	case <-time.After(*dur):
	case <-done:
	}

	// Dump the terminal screen.
	term.Lock()
	cur := term.Cursor()
	screen := &strings.Builder{}
	for y := 0; y < rows; y++ {
		row := &strings.Builder{}
		for x := 0; x < cols; x++ {
			g := term.Cell(x, y)
			if g.Char == 0 {
				row.WriteByte(' ')
			} else {
				row.WriteRune(g.Char)
			}
		}
		line := strings.TrimRight(row.String(), " \t\x00")
		if line == "" {
			continue
		}
		fmt.Fprintf(screen, "%3d: %s\n", y, line)
	}
	term.Unlock()
	screenOut, _ := os.Create(*out + ".screen.txt")
	fmt.Fprintf(screenOut, "cursor: row=%d col=%d\n\n", cur.Y, cur.X)
	screenOut.WriteString(screen.String())
	screenOut.Close()

	_ = cmd.Process.Kill()
	fmt.Printf("captured to %s, %s.stripped.txt, %s.screen.txt (cursor row=%d col=%d)\n", *out, *out, *out, cur.Y, cur.X)
}
