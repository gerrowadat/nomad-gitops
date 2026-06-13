package nomad_test

import (
	"fmt"
	"testing"

	"github.com/gerrowadat/nomad-botherer/internal/nomad"
)

func pendingUpdate(jobID, commit string) nomad.JobUpdate {
	return nomad.JobUpdate{
		UpdateID:  jobID + "/" + commit[:min(7, len(commit))],
		JobID:     jobID,
		GitCommit: commit,
		Operation: nomad.JobUpdateOperationRegister,
		Status:    nomad.JobUpdateStatusPending,
		Policy:    nomad.UpdatePolicyFull,
	}
}

func TestUpdateQueue_EnqueueAndNextPending(t *testing.T) {
	q := nomad.NewUpdateQueue()
	q.Enqueue(pendingUpdate("a", "commit1"))
	q.Enqueue(pendingUpdate("b", "commit1"))

	first := q.NextPending()
	if first == nil || first.JobID != "a" {
		t.Fatalf("NextPending should return oldest first, got %+v", first)
	}
	if first.Status != nomad.JobUpdateStatusInProgress {
		t.Errorf("NextPending should mark IN_PROGRESS, got %s", first.Status)
	}
	second := q.NextPending()
	if second == nil || second.JobID != "b" {
		t.Fatalf("expected b next, got %+v", second)
	}
	if q.NextPending() != nil {
		t.Error("queue should be drained")
	}
}

func TestUpdateQueue_SameUpdateID_NotDuplicated(t *testing.T) {
	q := nomad.NewUpdateQueue()
	u := pendingUpdate("a", "commit1")
	u.NomadJobModifyIndex = 10
	q.Enqueue(u)
	u.NomadJobModifyIndex = 20
	if superseded := q.Enqueue(u); superseded != 0 {
		t.Errorf("re-enqueue of same UpdateID should not supersede, got %d", superseded)
	}

	snap := q.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("expected 1 entry, got %d: %+v", len(snap), snap)
	}
	if snap[0].NomadJobModifyIndex != 20 {
		t.Errorf("re-enqueue should refresh the CAS token, got %d", snap[0].NomadJobModifyIndex)
	}
}

func TestUpdateQueue_NewCommitSupersedesPending(t *testing.T) {
	q := nomad.NewUpdateQueue()
	q.Enqueue(pendingUpdate("a", "commit1"))
	if superseded := q.Enqueue(pendingUpdate("a", "commit2")); superseded != 1 {
		t.Errorf("expected 1 superseded, got %d", superseded)
	}

	snap := q.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(snap))
	}
	var statuses []nomad.JobUpdateStatus
	for _, u := range snap {
		statuses = append(statuses, u.Status)
	}
	if statuses[0] != nomad.JobUpdateStatusSuperseded || statuses[1] != nomad.JobUpdateStatusPending {
		t.Errorf("expected [SUPERSEDED PENDING], got %v", statuses)
	}

	next := q.NextPending()
	if next == nil || next.GitCommit != "commit2" {
		t.Errorf("the newer commit should be the one applied, got %+v", next)
	}
}

func TestUpdateQueue_FailedIsRetriedOnReenqueue(t *testing.T) {
	q := nomad.NewUpdateQueue()
	q.Enqueue(pendingUpdate("a", "commit1"))
	u := q.NextPending()
	q.Complete(u.UpdateID, nomad.JobUpdateStatusFailed, 0, "register: boom")

	// Same drift re-detected on the next cycle: same UpdateID, reset to PENDING.
	q.Enqueue(pendingUpdate("a", "commit1"))
	snap := q.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("retry should reuse one row per UpdateID, got %d entries", len(snap))
	}
	if snap[0].Status != nomad.JobUpdateStatusPending {
		t.Errorf("retried update should be PENDING, got %s", snap[0].Status)
	}
	if snap[0].Error != "" {
		t.Errorf("retried update should have a cleared error, got %q", snap[0].Error)
	}
}

func TestUpdateQueue_Complete_Succeeded(t *testing.T) {
	q := nomad.NewUpdateQueue()
	q.Enqueue(pendingUpdate("a", "commit1"))
	u := q.NextPending()
	q.Complete(u.UpdateID, nomad.JobUpdateStatusSucceeded, 99, "")

	snap := q.Snapshot()
	if snap[0].Status != nomad.JobUpdateStatusSucceeded {
		t.Errorf("status: got %s", snap[0].Status)
	}
	if snap[0].AppliedAt == "" {
		t.Error("AppliedAt should be set on success")
	}
	if snap[0].NomadJobModifyIndex != 99 {
		t.Errorf("successful apply should record the new ModifyIndex, got %d", snap[0].NomadJobModifyIndex)
	}
}

func TestUpdateQueue_InProgressNotSuperseded(t *testing.T) {
	q := nomad.NewUpdateQueue()
	q.Enqueue(pendingUpdate("a", "commit1"))
	inflight := q.NextPending()

	q.Enqueue(pendingUpdate("a", "commit2"))
	snap := q.Snapshot()
	for _, u := range snap {
		if u.UpdateID == inflight.UpdateID && u.Status != nomad.JobUpdateStatusInProgress {
			t.Errorf("in-flight update must not be superseded, got %s", u.Status)
		}
	}
}

func TestUpdateQueue_PendingCount(t *testing.T) {
	q := nomad.NewUpdateQueue()
	if q.PendingCount() != 0 {
		t.Error("empty queue should have 0 pending")
	}
	q.Enqueue(pendingUpdate("a", "commit1"))
	q.Enqueue(pendingUpdate("b", "commit1"))
	if got := q.PendingCount(); got != 2 {
		t.Errorf("PendingCount: want 2, got %d", got)
	}
	q.NextPending()
	if got := q.PendingCount(); got != 1 {
		t.Errorf("PendingCount after NextPending: want 1, got %d", got)
	}
}

func TestUpdateQueue_TerminalRecordsPruned(t *testing.T) {
	q := nomad.NewUpdateQueue()
	// Push well past the terminal cap; each job completes immediately.
	for i := 0; i < 250; i++ {
		q.Enqueue(pendingUpdate(fmt.Sprintf("job-%d", i), "commit1"))
		u := q.NextPending()
		q.Complete(u.UpdateID, nomad.JobUpdateStatusSucceeded, 1, "")
	}
	if got := len(q.Snapshot()); got > 200 {
		t.Errorf("terminal records should be capped at 200, got %d", got)
	}
}
