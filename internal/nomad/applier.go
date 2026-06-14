package nomad

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

// Updates returns a snapshot of the update queue for the JSON API.
func (d *Differ) Updates() []JobUpdate {
	return d.updateQueue.Snapshot()
}

// notifyApplier wakes the applier loop. Non-blocking; multiple rapid
// notifications coalesce, mirroring the git watcher's trigger channel.
func (d *Differ) notifyApplier() {
	select {
	case d.applyCh <- struct{}{}:
	default:
	}
}

// invalidateSkip clears the cached Raft index so the next Check cannot take
// the skip-optimisation shortcut. Called after a failed apply: the failure is
// retried by letting the next full diff cycle re-detect the drift and
// re-enqueue the same UpdateID, rather than by a bespoke retry loop.
func (d *Differ) invalidateSkip() {
	d.mu.Lock()
	d.lastNomadIndex = 0
	d.mu.Unlock()
}

// RunApplier drains the update queue until ctx is cancelled. It wakes on
// every enqueue and on a fallback ticker (--apply-interval). Detection and
// application are deliberately decoupled: a slow or failing apply never
// delays the next diff check.
func (d *Differ) RunApplier(ctx context.Context) {
	ticker := time.NewTicker(d.applyInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-d.applyCh:
		}
		d.drainUpdates()
	}
}

// drainUpdates applies every PENDING update in queue order.
func (d *Differ) drainUpdates() {
	for {
		u := d.updateQueue.NextPending()
		if u == nil {
			d.pendingUpdates.Set(0)
			return
		}
		d.applyUpdate(u)
		d.pendingUpdates.Set(float64(d.updateQueue.PendingCount()))
	}
}

// completeUpdate records an apply outcome in the queue and metrics.
func (d *Differ) completeUpdate(u *JobUpdate, status JobUpdateStatus, appliedIndex uint64, errMsg string) {
	d.updateQueue.Complete(u.UpdateID, status, appliedIndex, errMsg)
	d.jobUpdatesTotal.WithLabelValues(string(u.Operation), string(status)).Inc()
}

// applyUpdate executes one update against Nomad.
func (d *Differ) applyUpdate(u *JobUpdate) {
	if u.Operation == JobUpdateOperationDeregister {
		d.applyDeregister(u)
		return
	}
	if u.Operation != JobUpdateOperationRegister {
		d.completeUpdate(u, JobUpdateStatusFailed, 0, fmt.Sprintf("unsupported operation %q", u.Operation))
		return
	}
	if u.job == nil {
		d.completeUpdate(u, JobUpdateStatusFailed, 0, "internal error: update has no parsed job")
		return
	}

	wq := &nomadapi.WriteOptions{Namespace: d.namespace}

	// Plan first — never register without a plan. If the plan no longer
	// shows any Git-owned change, the drift resolved between detection and
	// apply (or was only autoscaler churn); the update completes as a no-op.
	plan, _, err := d.jobs.Plan(u.job, true, wq)
	if err != nil {
		d.nomadAPIErrors.WithLabelValues("plan").Inc()
		slog.Warn("Apply: plan failed", "job", u.JobID, "update_id", u.UpdateID, "err", err)
		d.completeUpdate(u, JobUpdateStatusFailed, 0, fmt.Sprintf("plan: %v", err))
		d.invalidateSkip()
		return
	}
	if classifyDiff(plan.Diff, autoscaledGroups(u.job), d.managedMetaPrefix) == DiffClassNone {
		slog.Info("Apply: plan shows no change, nothing to do", "job", u.JobID, "update_id", u.UpdateID)
		d.completeUpdate(u, JobUpdateStatusSucceeded, 0, "")
		return
	}

	// CAS register: EnforceIndex with the ModifyIndex captured at detection
	// time. Nomad rejects the write if the job changed in between; for new
	// jobs the index is 0, which Nomad reads as "must not already exist".
	// PreserveCounts keeps autoscaler-owned group counts out of the write.
	resp, _, err := d.jobs.RegisterOpts(u.job, &nomadapi.RegisterOptions{
		EnforceIndex:   true,
		ModifyIndex:    u.NomadJobModifyIndex,
		PreserveCounts: u.preserveCounts,
	}, wq)
	if err != nil {
		d.nomadAPIErrors.WithLabelValues("register").Inc()
		slog.Warn("Apply: register failed", "job", u.JobID, "update_id", u.UpdateID,
			"enforce_index", u.NomadJobModifyIndex, "err", err)
		d.completeUpdate(u, JobUpdateStatusFailed, 0, fmt.Sprintf("register: %v", err))
		// Whether a CAS conflict or a transient error, the recovery is the
		// same: force the next diff cycle to run in full so it re-detects
		// current state and enqueues a fresh update with a current token.
		d.invalidateSkip()
		return
	}

	slog.Info("Apply: job registered", "job", u.JobID, "update_id", u.UpdateID,
		"eval_id", resp.EvalID, "new_modify_index", resp.JobModifyIndex)
	d.completeUpdate(u, JobUpdateStatusSucceeded, resp.JobModifyIndex, "")
}

// applyDeregister removes a job that was deleted from the repo. It rechecks
// live state immediately before the call rather than trusting the stored
// intent ("recheck, don't remember"): a deregister only proceeds if the job
// still exists and still carries the managed tag. If it is already gone the
// update succeeds as a no-op; if it exists but is no longer tagged (someone
// re-registered it, or took it out of management) the deregister is abandoned.
func (d *Differ) applyDeregister(u *JobUpdate) {
	q := &nomadapi.QueryOptions{Namespace: d.namespace}
	wq := &nomadapi.WriteOptions{Namespace: d.namespace}

	live, _, err := d.jobs.Info(u.JobID, q)
	if err != nil {
		if isNotFound(err) {
			slog.Info("Deregister: job already gone, nothing to do", "job", u.JobID, "update_id", u.UpdateID)
			d.completeUpdate(u, JobUpdateStatusSucceeded, 0, "")
			return
		}
		d.nomadAPIErrors.WithLabelValues("info").Inc()
		slog.Warn("Deregister: recheck failed", "job", u.JobID, "update_id", u.UpdateID, "err", err)
		d.completeUpdate(u, JobUpdateStatusFailed, 0, fmt.Sprintf("recheck: %v", err))
		d.invalidateSkip()
		return
	}
	if live == nil || !d.metaKeyPresent(live.Meta) {
		// The job no longer carries the managed tag: precondition gone, do not
		// touch it. The next cycle re-evaluates against current state.
		slog.Info("Deregister: live job no longer carries the managed tag; not deregistering",
			"job", u.JobID, "update_id", u.UpdateID)
		d.completeUpdate(u, JobUpdateStatusFailed, 0, "live job no longer carries the managed tag")
		d.invalidateSkip()
		return
	}

	slog.Info("Deregister: removing job that was deleted from the repo",
		"job", u.JobID, "update_id", u.UpdateID, "purge", d.deregisterPurge)
	evalID, _, err := d.jobs.Deregister(u.JobID, d.deregisterPurge, wq)
	if err != nil {
		d.nomadAPIErrors.WithLabelValues("deregister").Inc()
		slog.Warn("Deregister failed", "job", u.JobID, "update_id", u.UpdateID, "err", err)
		d.completeUpdate(u, JobUpdateStatusFailed, 0, fmt.Sprintf("deregister: %v", err))
		d.invalidateSkip()
		return
	}
	slog.Info("Deregister: job deregistered", "job", u.JobID, "update_id", u.UpdateID, "eval_id", evalID, "purge", d.deregisterPurge)
	d.completeUpdate(u, JobUpdateStatusSucceeded, 0, "")
}
