package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/joho/godotenv"

	"github.com/gerrowadat/nomad-botherer/internal/config"
	"github.com/gerrowadat/nomad-botherer/internal/gitwatch"
	"github.com/gerrowadat/nomad-botherer/internal/nomad"
	"github.com/gerrowadat/nomad-botherer/internal/server"
)

// Injected at build time via -ldflags.
var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	// Load .env for local development. Non-fatal if the file is absent.
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		slog.Warn("Error loading .env file", "err", err)
	}

	cfg, err := config.Load()
	if err != nil {
		slog.Error("Loading config", "err", err)
		os.Exit(1)
	}

	setupLogging(cfg.LogLevel)
	slog.Info("Starting nomad-botherer", "version", version, "commit", commit, "buildDate", buildDate)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	differ, err := nomad.NewDiffer(cfg)
	if err != nil {
		slog.Error("Creating Nomad differ", "err", err)
		os.Exit(1)
	}

	// onChange is called by the watcher whenever the branch HEAD advances.
	// We close over watcher, which is set below before Run is called.
	var watcher *gitwatch.Watcher
	onChange := func(newCommit string) {
		hclFiles, err := watcher.ReadHCLFiles()
		if err != nil {
			slog.Error("Reading HCL files from repo", "err", err)
			return
		}
		if err := differ.Check(hclFiles, newCommit); err != nil {
			slog.Error("Running diff check", "err", err)
		}
	}

	watcher = gitwatch.New(cfg, onChange)
	// The differ reads prior git state (via the watcher) to tell whether drift
	// pre-dates a job's opt-in.
	differ.SetHistorySource(watcher)

	if err := watcher.Clone(ctx); err != nil {
		slog.Error("Cloning repository", "err", err)
		os.Exit(1)
	}

	// Run an initial diff check immediately after clone.
	onChange(watcher.LastCommit())

	// Periodic diff checks independent of git changes (catches Nomad-side drift).
	go func() {
		ticker := time.NewTicker(cfg.DiffInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				commit, _ := watcher.Status()
				hclFiles, err := watcher.ReadHCLFiles()
				if err != nil {
					slog.Error("Reading HCL files for periodic check", "err", err)
					continue
				}
				if err := differ.Check(hclFiles, commit); err != nil {
					slog.Error("Periodic diff check failed", "err", err)
				}
			}
		}
	}()

	// Git staleness checker: triggers a fetch when the repo has not been
	// successfully fetched within MaxGitStaleness. Disabled when zero.
	if cfg.MaxGitStaleness > 0 {
		go func() {
			checkInterval := cfg.MaxGitStaleness / 2
			if checkInterval < 10*time.Second {
				checkInterval = 10 * time.Second
			}
			ticker := time.NewTicker(checkInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, lastGitUpdate := watcher.Status()
					if !lastGitUpdate.IsZero() && time.Since(lastGitUpdate) > cfg.MaxGitStaleness {
						slog.Info("Git repo is stale, triggering refresh", "age", time.Since(lastGitUpdate), "max", cfg.MaxGitStaleness)
						watcher.TriggerStale()
					}
				}
			}
		}()
	}

	// Nomad staleness checker: forces a diff check when Nomad state has not
	// been checked within MaxNomadStaleness. Disabled when zero.
	if cfg.MaxNomadStaleness > 0 {
		go func() {
			checkInterval := cfg.MaxNomadStaleness / 2
			if checkInterval < 10*time.Second {
				checkInterval = 10 * time.Second
			}
			ticker := time.NewTicker(checkInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					_, lastNomadCheck, _ := differ.Diffs()
					if !lastNomadCheck.IsZero() && time.Since(lastNomadCheck) > cfg.MaxNomadStaleness {
						slog.Info("Nomad state is stale, forcing diff check", "age", time.Since(lastNomadCheck), "max", cfg.MaxNomadStaleness)
						commit, _ := watcher.Status()
						hclFiles, err := watcher.ReadHCLFiles()
						if err != nil {
							slog.Error("Reading HCL files for staleness check", "err", err)
							continue
						}
						if err := differ.ForceCheck(hclFiles, commit); err != nil {
							slog.Error("Staleness diff check failed", "err", err)
						}
					}
				}
			}
		}()
	}

	// Watcher polls git and triggers onChange on new commits.
	go watcher.Run(ctx)

	// Applier drains the GitOps update queue. With the default
	// --default-update-policy=none nothing is ever enqueued, so this loop
	// idles unless jobs opt in via meta or the default policy is raised.
	go differ.RunApplier(ctx)

	srv := server.New(cfg, differ, watcher, server.BuildInfo{
		Version:   version,
		Commit:    commit,
		BuildDate: buildDate,
	})
	if err := srv.Run(ctx); err != nil {
		slog.Error("HTTP server error", "err", err)
		os.Exit(1)
	}
}

func setupLogging(level string) {
	var l slog.Level
	switch level {
	case "debug":
		l = slog.LevelDebug
	case "warn":
		l = slog.LevelWarn
	case "error":
		l = slog.LevelError
	default:
		l = slog.LevelInfo
	}
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: l})))
}
