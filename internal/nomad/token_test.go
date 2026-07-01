package nomad

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gerrowadat/nomad-botherer/internal/config"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

func TestResolveNomadToken_StaticOnly(t *testing.T) {
	t.Setenv("NOMAD_SECRETS_DIR", "") // ensure no auto-detect
	tok, watch, err := resolveNomadToken(&config.Config{NomadToken: "static-abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "static-abc" || watch != "" {
		t.Errorf("static token: want (static-abc, \"\"), got (%q, %q)", tok, watch)
	}
}

func TestResolveNomadToken_None(t *testing.T) {
	t.Setenv("NOMAD_SECRETS_DIR", "")
	tok, watch, err := resolveNomadToken(&config.Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" || watch != "" {
		t.Errorf("no token: want (\"\", \"\"), got (%q, %q)", tok, watch)
	}
}

func TestResolveNomadToken_FileExplicit_WinsOverStatic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeFile(t, path, "  file-xyz\n")

	tok, watch, err := resolveNomadToken(&config.Config{
		NomadToken:     "static-abc",
		NomadTokenFile: path,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "file-xyz" {
		t.Errorf("token should be read and trimmed from the file, got %q", tok)
	}
	if watch != path {
		t.Errorf("watch path: want %q, got %q", path, watch)
	}
}

func TestResolveNomadToken_FileExplicit_MissingIsError(t *testing.T) {
	_, _, err := resolveNomadToken(&config.Config{NomadTokenFile: "/nonexistent/nomad_token"})
	if err == nil {
		t.Error("an explicitly configured token file that cannot be read should error at startup")
	}
}

// A raw workload-identity token file is no longer auto-detected as a token: the
// raw WI JWT does not work with Job.Plan (issue #74), so with no token
// configured resolveNomadToken returns anonymous — login mode is handled
// separately by the caller.
func TestResolveNomadToken_NoAutoDetectOfWIFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wiTokenFilename), "ey.some.jwt")
	t.Setenv("NOMAD_SECRETS_DIR", dir)

	tok, watch, err := resolveNomadToken(&config.Config{}) // no token configured
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" || watch != "" {
		t.Errorf("WI token file must not be auto-detected as a token, got (%q, %q)", tok, watch)
	}
}

func TestResolveNomadToken_StaticIgnoresWIFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wiTokenFilename), "wi-token-123")
	t.Setenv("NOMAD_SECRETS_DIR", dir)

	tok, watch, err := resolveNomadToken(&config.Config{NomadToken: "static-abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "static-abc" || watch != "" {
		t.Errorf("static token should be used as-is, got (%q, %q)", tok, watch)
	}
}

func TestLooksLikeJWT(t *testing.T) {
	jwt := "eyJhbGciOiJSUzI1NiJ9.eyJzdWIiOiJ4In0.sig"
	if !looksLikeJWT(jwt) {
		t.Errorf("expected %q to look like a JWT", jwt)
	}
	if looksLikeJWT("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee") {
		t.Error("a UUID SecretID must not look like a JWT")
	}
	if looksLikeJWT("") {
		t.Error("empty string must not look like a JWT")
	}
}

func TestLoginJWTPath(t *testing.T) {
	// Explicit flag wins.
	if got := loginJWTPath(&config.Config{NomadLoginJWTFile: "/x/named.jwt"}); got != "/x/named.jwt" {
		t.Errorf("explicit jwt file: got %q", got)
	}
	// Default to ${NOMAD_SECRETS_DIR}/nomad_token.
	t.Setenv("NOMAD_SECRETS_DIR", "/secrets")
	if got := loginJWTPath(&config.Config{}); got != filepath.Join("/secrets", wiTokenFilename) {
		t.Errorf("default jwt file: got %q", got)
	}
	// Nothing available.
	t.Setenv("NOMAD_SECRETS_DIR", "")
	if got := loginJWTPath(&config.Config{}); got != "" {
		t.Errorf("no jwt file available: got %q", got)
	}
}

func TestNextLoginDelay(t *testing.T) {
	// Half the remaining lifetime for a comfortable TTL.
	exp := time.Now().Add(30 * time.Minute)
	d := nextLoginDelay(&exp)
	if d < 14*time.Minute || d > 15*time.Minute {
		t.Errorf("half-life delay for 30m TTL: want ~15m, got %v", d)
	}
	// A short TTL must still refresh *before* expiry (never floored past it).
	short := time.Now().Add(20 * time.Second)
	got := nextLoginDelay(&short)
	if got <= 0 || got >= 20*time.Second-loginSafetyMargin+time.Second {
		t.Errorf("short TTL must schedule before expiry (< ~%v), got %v", 20*time.Second-loginSafetyMargin, got)
	}
	// At/after expiry: re-login now.
	past := time.Now().Add(-time.Second)
	if got := nextLoginDelay(&past); got != 0 {
		t.Errorf("expired token should re-login now (0), got %v", got)
	}
	// No expiry falls back.
	if got := nextLoginDelay(nil); got != defaultLoginRefresh {
		t.Errorf("nil expiry should fall back to %v, got %v", defaultLoginRefresh, got)
	}
}

func TestRefreshTokenFile_AppliesRotatedToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeFile(t, path, "tok-1")

	var mu sync.Mutex
	var applied []string
	setToken := func(s string) {
		mu.Lock()
		defer mu.Unlock()
		applied = append(applied, s)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		refreshTokenFile(ctx, path, 5*time.Millisecond, "tok-1", setToken, func(error) {})
		close(done)
	}()

	// Rotate the token; the refresher should pick it up.
	writeFile(t, path, "tok-2")

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(applied)
		last := ""
		if n > 0 {
			last = applied[n-1]
		}
		mu.Unlock()
		if last == "tok-2" {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("rotated token was not applied; applied=%v", applied)
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done

	// The unchanged baseline should never have been re-applied.
	mu.Lock()
	defer mu.Unlock()
	for _, a := range applied {
		if a == "tok-1" {
			t.Errorf("baseline token should not be re-applied, got %v", applied)
		}
	}
}

func TestRefreshTokenFile_ReportsReadErrors(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeFile(t, path, "tok-1")

	var mu sync.Mutex
	errCount := 0
	onErr := func(error) {
		mu.Lock()
		errCount++
		mu.Unlock()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		refreshTokenFile(ctx, path, 5*time.Millisecond, "tok-1", func(string) {}, onErr)
		close(done)
	}()

	// Remove the file so reads fail.
	if err := os.Remove(path); err != nil {
		t.Fatalf("removing token file: %v", err)
	}

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := errCount
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("read error was not reported")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestRefreshTokenFile_StopsOnContextCancel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	writeFile(t, path, "tok-1")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		refreshTokenFile(ctx, path, 5*time.Millisecond, "tok-1", func(string) {}, func(error) {})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("refresher did not stop after context cancel")
	}
}

func TestRunLoginRefresher_AppliesOnSuccess(t *testing.T) {
	exp := time.Now().Add(30 * time.Minute)
	var mu sync.Mutex
	var applied []string
	login := func() (string, *time.Time, error) { return "sid-1", &exp, nil }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runLoginRefresher(ctx, 5*time.Millisecond, login,
			func(s string) { mu.Lock(); applied = append(applied, s); mu.Unlock() },
			func(error) { t.Error("unexpected error callback") })
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := len(applied)
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("login token was not applied")
		case <-time.After(5 * time.Millisecond):
		}
	}
	mu.Lock()
	if applied[0] != "sid-1" {
		t.Errorf("applied token: want sid-1, got %q", applied[0])
	}
	mu.Unlock()
	cancel()
	<-done
}

func TestRunLoginRefresher_ReportsError(t *testing.T) {
	var mu sync.Mutex
	errCount := 0
	login := func() (string, *time.Time, error) { return "", nil, errors.New("login rejected") }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		runLoginRefresher(ctx, 5*time.Millisecond, login,
			func(string) { t.Error("apply must not be called on error") },
			func(error) { mu.Lock(); errCount++; mu.Unlock() })
		close(done)
	}()

	deadline := time.After(2 * time.Second)
	for {
		mu.Lock()
		n := errCount
		mu.Unlock()
		if n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("login error was not reported")
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

func TestRunLoginRefresher_StopsOnContextCancel(t *testing.T) {
	login := func() (string, *time.Time, error) { return "sid", nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		// A long first delay so nothing fires before cancel.
		runLoginRefresher(ctx, time.Hour, login, func(string) {}, func(error) {})
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("login refresher did not stop after context cancel")
	}
}
