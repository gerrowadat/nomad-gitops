package nomad

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gerrowadat/nomad-botherer/internal/config"
)

// Authentication to the Nomad API. Three sources, in precedence order:
//
//  1. A token file (--nomad-token-file) — preferred. Re-read periodically so a
//     rotating token stays current. This is how Nomad workload identity works:
//     when nomad-botherer runs as a Nomad task with `identity { file = true }`,
//     Nomad writes the task's identity token to ${NOMAD_SECRETS_DIR}/nomad_token
//     and rotates it before expiry. The file is auto-detected at that path when
//     no token is configured at all, so a correctly-set-up job needs no token
//     flags.
//  2. A static token (--nomad-token / NOMAD_TOKEN) — for manual running and
//     testing. Does not refresh; fine for short-lived or non-expiring tokens.
//  3. None — anonymous access, which works only when the cluster has ACLs
//     disabled.
//
// A static token does not refresh, so under Nomad prefer the file source: a
// static NOMAD_TOKEN (including one injected by `identity { env = true }`) will
// eventually expire and is not re-read.

// wiTokenFilename is the file Nomad writes the default workload-identity token
// to under the task secrets dir when `identity { file = true }` is set.
const wiTokenFilename = "nomad_token"

// resolveNomadToken decides the initial token to authenticate with and, when
// the token is sourced from a file, the path to keep re-reading. watchPath is
// empty for the static and anonymous cases. An explicitly-configured token file
// that cannot be read is a fatal misconfiguration and returns an error; the
// auto-detected path is only used when it already exists.
func resolveNomadToken(cfg *config.Config) (token, watchPath string, err error) {
	switch {
	case cfg.NomadTokenFile != "":
		watchPath = cfg.NomadTokenFile
		if cfg.NomadToken != "" {
			slog.Warn("Both a static Nomad token and a token file are configured; using the token file (it refreshes) and ignoring the static token",
				"token_file", watchPath)
		}
	case cfg.NomadToken != "":
		return cfg.NomadToken, "", nil
	default:
		// No token configured: auto-detect the workload-identity token file so a
		// job that sets `identity { file = true }` needs no token configuration.
		watchPath = defaultWorkloadTokenPath()
	}

	if watchPath == "" {
		return "", "", nil // anonymous
	}
	token, err = readTokenFile(watchPath)
	if err != nil {
		return "", "", err
	}
	return token, watchPath, nil
}

// defaultWorkloadTokenPath returns ${NOMAD_SECRETS_DIR}/nomad_token when that
// file might be present, else "". NOMAD_SECRETS_DIR is set for every Nomad
// task, but the token file is only written when the job opts in with
// `identity { file = true }`, so a definitively-absent file means "workload
// identity not configured" and we fall through to the other sources.
//
// Only the not-exist case is suppressed: any other stat error (permissions, IO)
// means the file is probably there but unreadable, which on an ACL-enabled
// cluster should surface as a startup error rather than silently degrade to
// anonymous access. Returning the path lets readTokenFile report it.
func defaultWorkloadTokenPath() string {
	dir := os.Getenv("NOMAD_SECRETS_DIR")
	if dir == "" {
		return ""
	}
	p := filepath.Join(dir, wiTokenFilename)
	if _, err := os.Stat(p); errors.Is(err, fs.ErrNotExist) {
		return ""
	}
	return p
}

// readTokenFile reads and trims a token from a file.
func readTokenFile(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("reading Nomad token file %q: %w", path, err)
	}
	return strings.TrimSpace(string(b)), nil
}

// refreshTokenFile re-reads watchPath every interval and calls setToken whenever
// the (non-empty) token changes from current. Read errors are reported via onErr
// and the previous token is kept. It blocks until ctx is cancelled. setToken and
// onErr are injected so the loop is testable without a live Nomad client.
func refreshTokenFile(ctx context.Context, watchPath string, interval time.Duration, current string, setToken func(string), onErr func(error)) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tok, err := readTokenFile(watchPath)
			if err != nil {
				onErr(err)
				continue
			}
			// An empty file is treated as a transient write-in-progress, not an
			// intentional clearing of the token.
			if tok != "" && tok != current {
				current = tok
				setToken(tok)
			}
		}
	}
}
