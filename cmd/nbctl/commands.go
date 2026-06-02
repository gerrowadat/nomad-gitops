package main

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	"github.com/gerrowadat/nomad-botherer/internal/grpcapi"
)

func newDiffsCmd(cfg *rootConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "diffs",
		Short: "Show current job diffs",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, close, err := cfg.dial()
			if err != nil {
				return err
			}
			defer close()

			ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
			defer cancel()

			resp, err := client.GetDiffs(ctx, &grpcapi.GetDiffsRequest{})
			if err != nil {
				return fmt.Errorf("GetDiffs: %w", err)
			}

			return printOutput(cmd.OutOrStdout(), cfg.outFmt, resp, func(w io.Writer) {
				if len(resp.Diffs) == 0 {
					fmt.Fprintln(w, "no diffs detected")
				} else {
					fmt.Fprintf(w, "%d diff(s) detected\n", len(resp.Diffs))
				}
				if resp.LastCheckTime != "" {
					fmt.Fprintf(w, "last check:  %s\n", resp.LastCheckTime)
				}
				if resp.LastCommit != "" {
					fmt.Fprintf(w, "last commit: %s\n", resp.LastCommit)
				}
				for _, d := range resp.Diffs {
					fmt.Fprintln(w)
					if d.HclFile != "" {
						fmt.Fprintf(w, "[%s] %s (%s)\n", d.DiffType, d.JobId, d.HclFile)
					} else {
						fmt.Fprintf(w, "[%s] %s\n", d.DiffType, d.JobId)
					}
					if d.Detail != "" {
						fmt.Fprintf(w, "  %s\n", d.Detail)
					}
				}
			})
		},
	}
}

func newSelectedJobsCmd(cfg *rootConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "selected-jobs",
		Short: "List jobs currently selected for monitoring, and why each matched",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, close, err := cfg.dial()
			if err != nil {
				return err
			}
			defer close()

			ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
			defer cancel()

			resp, err := client.GetSelectedJobs(ctx, &grpcapi.GetSelectedJobsRequest{})
			if err != nil {
				return fmt.Errorf("GetSelectedJobs: %w", err)
			}

			return printOutput(cmd.OutOrStdout(), cfg.outFmt, resp, func(w io.Writer) {
				if len(resp.Jobs) == 0 {
					fmt.Fprintln(w, "no jobs currently selected")
				} else {
					fmt.Fprintf(w, "%d job(s) selected\n", len(resp.Jobs))
				}
				if resp.LastCheckTime != "" {
					fmt.Fprintf(w, "last check:  %s\n", resp.LastCheckTime)
				}
				if resp.LastCommit != "" {
					fmt.Fprintf(w, "last commit: %s\n", resp.LastCommit)
				}
				for _, j := range resp.Jobs {
					fmt.Fprintf(w, "  %-40s  %s\n", j.JobId, j.SelectionReason)
				}
			})
		},
	}
}

func newStatusCmd(cfg *rootConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show git watcher status",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, close, err := cfg.dial()
			if err != nil {
				return err
			}
			defer close()

			ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
			defer cancel()

			resp, err := client.GetStatus(ctx, &grpcapi.GetStatusRequest{})
			if err != nil {
				return fmt.Errorf("GetStatus: %w", err)
			}

			return printOutput(cmd.OutOrStdout(), cfg.outFmt, resp, func(w io.Writer) {
				commit := resp.LastCommit
				if commit == "" {
					commit = "(none)"
				}
				fmt.Fprintf(w, "last commit:  %s\n", commit)
				updated := resp.LastUpdateTime
				if updated == "" {
					updated = "(none)"
				}
				fmt.Fprintf(w, "last updated: %s\n", updated)
			})
		},
	}
}

func newRefreshCmd(cfg *rootConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "refresh",
		Short: "Trigger an immediate git pull and diff check",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, close, err := cfg.dial()
			if err != nil {
				return err
			}
			defer close()

			ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
			defer cancel()

			resp, err := client.TriggerRefresh(ctx, &grpcapi.TriggerRefreshRequest{})
			if err != nil {
				return fmt.Errorf("TriggerRefresh: %w", err)
			}

			return printOutput(cmd.OutOrStdout(), cfg.outFmt, resp, func(w io.Writer) {
				fmt.Fprintln(w, resp.Message)
			})
		},
	}
}

func newVersionCmd(cfg *rootConfig) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Show the server's build version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			client, close, err := cfg.dial()
			if err != nil {
				return err
			}
			defer close()

			ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
			defer cancel()

			resp, err := client.GetVersion(ctx, &grpcapi.GetVersionRequest{})
			if err != nil {
				return fmt.Errorf("GetVersion: %w", err)
			}

			return printOutput(cmd.OutOrStdout(), cfg.outFmt, resp, func(w io.Writer) {
				fmt.Fprintf(w, "version:    %s\n", resp.Version)
				fmt.Fprintf(w, "commit:     %s\n", resp.Commit)
				fmt.Fprintf(w, "build date: %s\n", resp.BuildDate)
			})
		},
	}
}

// printOutput writes the response to w. In "json" mode it serialises the
// proto message using protojson; otherwise it calls textFn.
func printOutput(w io.Writer, format string, msg proto.Message, textFn func(io.Writer)) error {
	if format == "json" {
		b, err := protojson.MarshalOptions{Indent: "  ", UseProtoNames: true}.Marshal(msg)
		if err != nil {
			return fmt.Errorf("marshalling json: %w", err)
		}
		fmt.Fprintln(w, string(b))
		return nil
	}
	textFn(w)
	return nil
}
