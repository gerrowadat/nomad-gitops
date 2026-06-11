package main

import (
	"context"
	"log/slog"
	"testing"
)

func TestSetupLogging_Levels(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	cases := []struct {
		in   string
		want slog.Level
	}{
		{"debug", slog.LevelDebug},
		{"info", slog.LevelInfo},
		{"warn", slog.LevelWarn},
		{"error", slog.LevelError},
		{"bogus", slog.LevelInfo}, // unknown levels fall back to info
		{"", slog.LevelInfo},
	}

	ctx := context.Background()
	for _, tc := range cases {
		setupLogging(tc.in)
		logger := slog.Default()
		if !logger.Enabled(ctx, tc.want) {
			t.Errorf("level %q: logger should be enabled at %v", tc.in, tc.want)
		}
		if logger.Enabled(ctx, tc.want-1) {
			t.Errorf("level %q: logger should be disabled below %v", tc.in, tc.want)
		}
	}
}
