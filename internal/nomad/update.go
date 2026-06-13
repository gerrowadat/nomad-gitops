package nomad

import (
	"fmt"
	"sync"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

// UpdatePolicy controls how much detected drift may be applied to a job
// automatically. It is declared per job in HCL meta
// (<prefix>_update_policy) and falls back to --default-update-policy.
type UpdatePolicy string

const (
	// UpdatePolicyFull applies any detected drift.
	UpdatePolicyFull UpdatePolicy = "full"
	// UpdatePolicyImageOnly applies drift only when the entire plan diff is
	// confined to Docker image references.
	UpdatePolicyImageOnly UpdatePolicy = "image-only"
	// UpdatePolicyNone detects and surfaces drift but never applies it.
	UpdatePolicyNone UpdatePolicy = "none"
)

// ValidUpdatePolicy reports whether s is a recognised policy value.
func ValidUpdatePolicy(s string) bool {
	switch UpdatePolicy(s) {
	case UpdatePolicyFull, UpdatePolicyImageOnly, UpdatePolicyNone:
		return true
	}
	return false
}

// JobUpdateOperation is the kind of change a JobUpdate applies.
type JobUpdateOperation string

const (
	JobUpdateOperationRegister   JobUpdateOperation = "REGISTER"
	JobUpdateOperationDeregister JobUpdateOperation = "DEREGISTER" // not yet produced; reserved
)

// JobUpdateStatus is the lifecycle state of a JobUpdate.
type JobUpdateStatus string

const (
	JobUpdateStatusPending    JobUpdateStatus = "PENDING"
	JobUpdateStatusInProgress JobUpdateStatus = "IN_PROGRESS"
	JobUpdateStatusSucceeded  JobUpdateStatus = "SUCCEEDED"
	JobUpdateStatusFailed     JobUpdateStatus = "FAILED"
	JobUpdateStatusSuperseded JobUpdateStatus = "SUPERSEDED"
)

// terminal reports whether a status is final.
func (s JobUpdateStatus) terminal() bool {
	switch s {
	case JobUpdateStatusSucceeded, JobUpdateStatusFailed, JobUpdateStatusSuperseded:
		return true
	}
	return false
}

// JobUpdate represents a single intended change to a Nomad job, derived from
// a detected diff between Git and the cluster. A JobDiff is an observation;
// a JobUpdate is an intended transition.
type JobUpdate struct {
	// UpdateID is <job_id>/<git_commit_short> — deliberately derived from
	// stable inputs so the same intent re-detected after a restart or a
	// failure is recognisably the same update.
	UpdateID string `json:"update_id"`

	JobID string `json:"job_id"`

	// HCLFile is the repo path that is the source of truth for this job.
	HCLFile string `json:"hcl_file,omitempty"`

	// GitCommit is the commit hash that triggered this update.
	GitCommit string `json:"git_commit"`

	Operation JobUpdateOperation `json:"operation"`
	Status    JobUpdateStatus    `json:"status"`

	// Policy is the effective update policy that allowed this update.
	Policy UpdatePolicy `json:"policy"`

	// NomadJobModifyIndex is the job's ModifyIndex at detection time, used
	// as the CAS token on Jobs.Register (EnforceIndex). Zero means the job
	// did not exist in Nomad at detection time.
	NomadJobModifyIndex uint64 `json:"nomad_job_modify_index"`

	// NomadRaftIndex is the cluster Raft index at detection time, recorded
	// for auditability.
	NomadRaftIndex uint64 `json:"nomad_raft_index"`

	DetectedAt string `json:"detected_at"`          // RFC3339
	AppliedAt  string `json:"applied_at,omitempty"` // RFC3339; empty until applied
	Error      string `json:"error,omitempty"`

	// job is the parsed HCL job to register. In-memory only; the queue is
	// rebuilt from a diff cycle after restart so this never needs to be
	// serialised.
	job *nomadapi.Job
	// preserveCounts is set when the job has autoscaled task groups, so the
	// register call does not overwrite autoscaler-owned counts.
	preserveCounts bool
}

// updateID builds the stable identifier for a job/commit pair.
func updateID(jobID, commit string) string {
	short := commit
	if len(short) > 7 {
		short = short[:7]
	}
	return fmt.Sprintf("%s/%s", jobID, short)
}

// maxTerminalUpdates caps how many terminal (SUCCEEDED/FAILED/SUPERSEDED)
// records the queue retains for API visibility. Oldest are pruned first.
const maxTerminalUpdates = 200

// UpdateQueue is the in-memory queue between detection and application.
// Restart loses it by design: the next diff cycle recreates any update whose
// drift still exists, and CAS plus re-planning make a re-apply harmless. See
// docs/proposals/gitops-job-updates.md ("Restart safety and recovery").
type UpdateQueue struct {
	mu      sync.Mutex
	updates []*JobUpdate // newest last
}

// NewUpdateQueue returns an empty queue.
func NewUpdateQueue() *UpdateQueue {
	return &UpdateQueue{}
}

// Enqueue records an intended update. Rules:
//   - A non-terminal update with the same UpdateID (same job, same commit) is
//     refreshed in place rather than duplicated.
//   - A terminal update with the same UpdateID is reset to PENDING — the same
//     intent is being retried after a failure or a cluster-side change.
//   - A PENDING update for the same job with a different UpdateID (a newer
//     commit arrived before the old one applied) is marked SUPERSEDED; the
//     most recent intended state wins.
//
// An IN_PROGRESS update for the same job is left alone: the apply already
// started, and the next diff cycle re-detects anything it did not settle.
//
// Returns the number of updates marked SUPERSEDED by this enqueue.
func (q *UpdateQueue) Enqueue(u JobUpdate) (superseded int) {
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, existing := range q.updates {
		if existing.UpdateID == u.UpdateID && !existing.Status.terminal() {
			existing.NomadJobModifyIndex = u.NomadJobModifyIndex
			existing.NomadRaftIndex = u.NomadRaftIndex
			existing.job = u.job
			existing.preserveCounts = u.preserveCounts
			return 0
		}
	}

	for _, existing := range q.updates {
		if existing.JobID == u.JobID && existing.Status == JobUpdateStatusPending {
			existing.Status = JobUpdateStatusSuperseded
			superseded++
		}
	}

	// Retry of the same intent: drop the old terminal record so the queue
	// holds one row per UpdateID.
	for i, existing := range q.updates {
		if existing.UpdateID == u.UpdateID {
			q.updates = append(q.updates[:i], q.updates[i+1:]...)
			break
		}
	}

	u.Status = JobUpdateStatusPending
	q.updates = append(q.updates, &u)
	q.prune()
	return superseded
}

// NextPending returns the oldest PENDING update, marking it IN_PROGRESS, or
// nil when nothing is waiting.
func (q *UpdateQueue) NextPending() *JobUpdate {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, u := range q.updates {
		if u.Status == JobUpdateStatusPending {
			u.Status = JobUpdateStatusInProgress
			return u
		}
	}
	return nil
}

// Complete records the outcome of an apply attempt.
func (q *UpdateQueue) Complete(updateID string, status JobUpdateStatus, appliedIndex uint64, errMsg string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, u := range q.updates {
		if u.UpdateID == updateID && u.Status == JobUpdateStatusInProgress {
			u.Status = status
			u.Error = errMsg
			if status == JobUpdateStatusSucceeded {
				u.AppliedAt = time.Now().UTC().Format(time.RFC3339)
				if appliedIndex != 0 {
					u.NomadJobModifyIndex = appliedIndex
				}
			}
			q.prune()
			return
		}
	}
}

// Snapshot returns a copy of all queue entries, newest last. The internal
// job pointer is not exposed.
func (q *UpdateQueue) Snapshot() []JobUpdate {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := make([]JobUpdate, 0, len(q.updates))
	for _, u := range q.updates {
		c := *u
		c.job = nil
		out = append(out, c)
	}
	return out
}

// PendingCount returns the number of PENDING updates.
func (q *UpdateQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, u := range q.updates {
		if u.Status == JobUpdateStatusPending {
			n++
		}
	}
	return n
}

// prune drops the oldest terminal records beyond maxTerminalUpdates.
// Caller must hold q.mu.
func (q *UpdateQueue) prune() {
	terminal := 0
	for _, u := range q.updates {
		if u.Status.terminal() {
			terminal++
		}
	}
	if terminal <= maxTerminalUpdates {
		return
	}
	keep := q.updates[:0]
	for _, u := range q.updates {
		if terminal > maxTerminalUpdates && u.Status.terminal() {
			terminal--
			continue
		}
		keep = append(keep, u)
	}
	q.updates = keep
}
