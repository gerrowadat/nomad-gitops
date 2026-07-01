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

// Authentication to the Nomad API. Modes, in precedence order:
//
//  1. Workload-identity login (--nomad-login-auth-method) — the working way to
//     use Nomad workload identity. The identity JWT is exchanged for a real ACL
//     token (SecretID) via POST /v1/acl/login, and re-exchanged before it
//     expires. A raw WI JWT authenticates read RPCs but is *rejected* by
//     Nomad's Job.Plan RPC ("UUID must be 36 characters"), which nomad-botherer
//     needs for every drift check — so the JWT cannot be used directly as a
//     token (issue #74). Login exchange is the fix.
//  2. A token file (--nomad-token-file) — a real ACL SecretID in a file, re-read
//     periodically. For a sidecar-written token, or a rotating static token.
//  3. A static token (--nomad-token / NOMAD_TOKEN) — manual running and testing.
//  4. None — anonymous, which works only when the cluster has ACLs disabled.

// wiTokenFilename is the file Nomad writes the default workload-identity token
// (a JWT) to under the task secrets dir when `identity { file = true }` is set.
const wiTokenFilename = "nomad_token"

const (
	// loginSafetyMargin is how far before expiry a re-login must complete, so the
	// token is always refreshed before it expires even for a short TTL.
	loginSafetyMargin = 5 * time.Second
	// loginRetryBackoff is the delay before retrying after a failed login.
	loginRetryBackoff = 15 * time.Second
	// defaultLoginRefresh is used when an exchanged token carries no expiry
	// (unusual — WI login tokens are TTL-bounded).
	defaultLoginRefresh = 5 * time.Minute
)

// resolveNomadToken decides the initial token to authenticate with (for the
// file and static modes) and, when the token is sourced from a file, the path
// to keep re-reading. It does not handle login mode — the caller checks
// NomadLoginAuthMethod first. watchPath is empty for the static and anonymous
// cases. An explicitly-configured token file that cannot be read is a fatal
// misconfiguration and returns an error.
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
		return "", "", nil // anonymous
	}

	token, err = readTokenFile(watchPath)
	if err != nil {
		return "", "", err
	}
	return token, watchPath, nil
}

// loginJWTPath resolves the workload-identity JWT file to exchange in login
// mode: the explicit --nomad-login-jwt-file, else ${NOMAD_SECRETS_DIR}/nomad_token
// (the default identity), else "".
func loginJWTPath(cfg *config.Config) string {
	if cfg.NomadLoginJWTFile != "" {
		return cfg.NomadLoginJWTFile
	}
	if dir := os.Getenv("NOMAD_SECRETS_DIR"); dir != "" {
		return filepath.Join(dir, wiTokenFilename)
	}
	return ""
}

// defaultWorkloadTokenPath returns ${NOMAD_SECRETS_DIR}/nomad_token when that
// file exists, else "". Used only to detect a workload-identity deployment that
// has not configured login, so a clear hint can be logged.
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

// looksLikeJWT reports whether s is (probably) a JWT rather than an ACL
// SecretID, so a misconfiguration (feeding a raw WI JWT as a token) can be
// flagged. A SecretID is a 36-char UUID; a JWT is a longer dotted base64url
// string, conventionally starting "ey".
func looksLikeJWT(s string) bool {
	return strings.HasPrefix(s, "ey") && strings.Count(s, ".") == 2
}

// readTokenFile reads and trims a token (SecretID or JWT) from a file.
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

// nextLoginDelay returns how long until the next re-login: half the remaining
// lifetime (so a fresh token is obtained well before this one expires), but
// never later than loginSafetyMargin before expiry — even for a short TTL the
// refresh completes before the token is invalid. A non-positive result means
// re-login now (the token is at or past expiry). Falls back to
// defaultLoginRefresh when the token has no expiry.
func nextLoginDelay(expiry *time.Time) time.Duration {
	if expiry == nil {
		return defaultLoginRefresh
	}
	remaining := time.Until(*expiry)
	d := remaining / 2
	if latest := remaining - loginSafetyMargin; d > latest {
		d = latest
	}
	if d < 0 {
		d = 0
	}
	return d
}

// runLoginRefresher re-exchanges the workload-identity JWT for a fresh ACL token
// before the current one expires. firstDelay is when to attempt the next login
// (computed by the caller from the startup login's expiry, or a short backoff if
// startup login failed). login performs the exchange, returning the new SecretID
// and its expiry. apply installs the token; onErr reports a failed exchange. It
// blocks until ctx is cancelled. login/apply/onErr are injected so the loop is
// testable without a live Nomad client.
func runLoginRefresher(ctx context.Context, firstDelay time.Duration, login func() (secretID string, expiry *time.Time, err error), apply func(string), onErr func(error)) {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			secretID, expiry, err := login()
			if err != nil {
				onErr(err)
				timer.Reset(loginRetryBackoff)
				continue
			}
			apply(secretID)
			timer.Reset(nextLoginDelay(expiry))
		}
	}
}
