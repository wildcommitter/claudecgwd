package bridge

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Files sent over a chat surface are downloaded and written to an inbox dir so
// the assistant can read and catalog them. saveInbox handles the on-disk part;
// the bridges enqueue an inbound notice with the path, and the received-files
// skill catalogs it.

var unsafeNameRe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// saveInbox writes data to dir under a timestamped, sanitized filename and
// returns the absolute path. source is the surface ("telegram"/"whatsapp").
func saveInbox(dir, source, origName string, data []byte) (string, error) {
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".local", "share", "assistant", "inbox")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	name := sanitizeName(origName)
	if name == "" {
		name = "file"
	}
	ts := time.Now().Format("20060102-150405")
	path := filepath.Join(dir, fmt.Sprintf("%s-%s-%s", ts, source, name))
	if _, err := os.Stat(path); err == nil {
		// Collision within the same second: disambiguate with nanoseconds.
		path = filepath.Join(dir, fmt.Sprintf("%s-%s-%d-%s", ts, source, time.Now().Nanosecond(), name))
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// sanitizeName makes an arbitrary filename safe for the inbox directory.
func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, " ", "_")
	s = unsafeNameRe.ReplaceAllString(s, "_")
	s = strings.Trim(s, "._")
	if len(s) > 100 { // keep the tail (extension) when truncating
		s = s[len(s)-100:]
	}
	return s
}
