package nomad

import (
	"context"
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

func TestResolveNomadToken_AutoDetectWorkloadIdentity(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wiTokenFilename), "wi-token-123\n")
	t.Setenv("NOMAD_SECRETS_DIR", dir)

	tok, watch, err := resolveNomadToken(&config.Config{}) // no token configured
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "wi-token-123" {
		t.Errorf("auto-detected workload-identity token: want wi-token-123, got %q", tok)
	}
	if watch != filepath.Join(dir, wiTokenFilename) {
		t.Errorf("watch path: want the WI token file, got %q", watch)
	}
}

func TestResolveNomadToken_StaticBeatsAutoDetect(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, wiTokenFilename), "wi-token-123")
	t.Setenv("NOMAD_SECRETS_DIR", dir)

	// A static token is set: it should be used as-is, not the auto-detected file,
	// so manual testing with --nomad-token works even inside a task environment.
	tok, watch, err := resolveNomadToken(&config.Config{NomadToken: "static-abc"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "static-abc" || watch != "" {
		t.Errorf("static token should win over auto-detect, got (%q, %q)", tok, watch)
	}
}

func TestResolveNomadToken_NoAutoDetectWhenFileAbsent(t *testing.T) {
	dir := t.TempDir() // NOMAD_SECRETS_DIR set but no nomad_token file present
	t.Setenv("NOMAD_SECRETS_DIR", dir)
	tok, watch, err := resolveNomadToken(&config.Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if tok != "" || watch != "" {
		t.Errorf("no auto-detect without the token file present, got (%q, %q)", tok, watch)
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
