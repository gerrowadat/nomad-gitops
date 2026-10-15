package nomad

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"strings"

	nomadapi "github.com/hashicorp/nomad/api"
)

// Rollback and the flap-loop guard. Both lean on Nomad's own state — version
// history and deployment outcomes — so nomad-gitops holds no durable record
// of "what failed". See docs/design/automatic-rollback.md.
//
// Scope: only deployment-producing jobs (service jobs with an update stanza and
// health checks) ever participate. A job that produces no deployment has no
// failed-deployment signal, so the flap-guard never matches and active rollback
// never fires — both fall through naturally without special-casing.

// volatileJobFields are server-injected or per-registration fields that must be
// stripped before fingerprinting a job spec, so the same intent registered at
// different times fingerprints identically.
var volatileJobFields = []string{
	"Version", "Stable", "SubmitTime", "ModifyIndex", "JobModifyIndex",
	"CreateIndex", "Status", "StatusDescription", "VersionTag", "Namespace",
}

// specFingerprint returns a stable hash of a job's spec, ignoring exactly what
// the diff classifier ignores (nomad-gitops's own managed-prefix meta keys
// and autoscaler-owned Count/Scaling) plus Nomad-injected version bookkeeping.
// Comparing an HCL-parsed job against a stored Nomad version is best-effort:
// server-side defaulting can make a legitimately-identical spec differ, in
// which case the guard misses and the bad spec is retried once more and caught
// again. That degradation is one-way and safe; a false block (two distinct
// specs colliding) needs a sha256 collision.
func specFingerprint(job *nomadapi.Job, metaPrefix string) (string, error) {
	raw, err := json.Marshal(job)
	if err != nil {
		return "", err
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", err
	}
	for _, k := range volatileJobFields {
		delete(m, k)
	}
	if meta, ok := m["Meta"].(map[string]interface{}); ok {
		stripPrefixedKeys(meta, metaPrefix)
		if len(meta) == 0 {
			delete(m, "Meta")
		}
	}
	// Drop autoscaler-owned Count/Scaling from autoscaled groups so an
	// autoscaler nudge does not change the fingerprint.
	autoscaled := autoscaledGroups(job)
	if len(autoscaled) > 0 {
		if tgs, ok := m["TaskGroups"].([]interface{}); ok {
			for _, raw := range tgs {
				g, ok := raw.(map[string]interface{})
				if !ok {
					continue
				}
				name, _ := g["Name"].(string)
				if autoscaled[name] {
					delete(g, "Count")
					delete(g, "Scaling")
				}
			}
		}
	}
	norm, err := json.Marshal(m) // map keys are marshalled in sorted order
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(norm)
	return hex.EncodeToString(sum[:]), nil
}

// stripPrefixedKeys removes nomad-gitops's own managed meta keys (both the
// underscore and dotted forms) from a meta map.
func stripPrefixedKeys(meta map[string]interface{}, prefix string) {
	if prefix == "" {
		return
	}
	for k := range meta {
		if strings.HasPrefix(k, prefix+"_") || strings.HasPrefix(k, prefix+".") {
			delete(meta, k)
		}
	}
}

// effectiveFlapGuard resolves the flap-guard mode for a job: the HCL meta key
// <prefix>_flap_guard wins (Git is intent), otherwise the --flap-guard default.
// An invalid meta value falls back to the default (already logged at ERROR by
// validateManagedMeta).
func (d *Differ) effectiveFlapGuard(meta map[string]string) string {
	if d.managedMetaPrefix != "" {
		if v, ok := meta[d.managedMetaPrefix+"_flap_guard"]; ok && validFlapGuardValue(v) {
			return v
		}
	}
	return d.flapGuard
}

// effectiveRollback resolves whether active rollback is enabled for a job: the
// HCL meta key <prefix>_rollback wins, otherwise the --allow-rollback default.
func (d *Differ) effectiveRollback(meta map[string]string) bool {
	if d.managedMetaPrefix != "" {
		if v, ok := meta[d.managedMetaPrefix+"_rollback"]; ok && validManagedValue(v) {
			return v == "true"
		}
	}
	return d.allowRollback
}

// jobHasAutoRevert reports whether a job's update stanza opts into Nomad's
// native auto_revert, at the job level or on any task group. When true,
// nomad-gitops stands down: Nomad's own rollback always wins.
func jobHasAutoRevert(job *nomadapi.Job) bool {
	if job == nil {
		return false
	}
	if job.Update != nil && job.Update.AutoRevert != nil && *job.Update.AutoRevert {
		return true
	}
	for _, tg := range job.TaskGroups {
		if tg == nil || tg.Update == nil {
			continue
		}
		if tg.Update.AutoRevert != nil && *tg.Update.AutoRevert {
			return true
		}
	}
	return false
}

// failedTagPrefix is the version-tag name prefix used by flap-guard tag mode.
func (d *Differ) failedTagPrefix() string {
	return d.managedMetaPrefix + "-failed-"
}

// failedTagName builds the durable version-tag name for a failed spec.
func (d *Differ) failedTagName(fingerprint string) string {
	return d.failedTagPrefix() + fingerprint
}

// parseFailedFingerprint recovers a spec fingerprint from a failed-version tag
// name, or ("", false) if the tag is not one of ours.
func (d *Differ) parseFailedFingerprint(tagName string) (string, bool) {
	p := d.failedTagPrefix()
	if d.managedMetaPrefix == "" || !strings.HasPrefix(tagName, p) {
		return "", false
	}
	return strings.TrimPrefix(tagName, p), true
}

// flapGuardBlocks reports whether re-applying the candidate's HCL spec would
// re-enter a deployment that already failed. mode is the effective guard mode
// (history or tag; "off" is handled by the caller). On any Nomad API error it
// fails open (returns false): a missed block costs at most one more failed
// attempt, whereas a spurious block could freeze a legitimate apply.
func (d *Differ) flapGuardBlocks(c *updateCandidate, mode string, q *nomadapi.QueryOptions) bool {
	want, err := specFingerprint(c.job, d.managedMetaPrefix)
	if err != nil {
		slog.Warn("Flap-guard: could not fingerprint HCL job; not blocking", "job", c.jobID, "err", err)
		return false
	}
	failed, err := d.failedVersionFingerprints(c.jobID, mode, q)
	if err != nil {
		slog.Warn("Flap-guard: could not read Nomad version history; not blocking", "job", c.jobID, "err", err)
		return false
	}
	_, blocked := failed[want]
	return blocked
}

// failedVersionFingerprints returns the set of spec fingerprints for job
// versions whose deployment failed. In history mode it derives them from the
// current failed deployments and the retained version specs (ephemeral, lost
// when Nomad GCs the version). In tag mode it additionally reads fingerprints
// recovered from durable version tags, and tags any newly-observed failed
// version so the block survives GC.
func (d *Differ) failedVersionFingerprints(jobID, mode string, q *nomadapi.QueryOptions) (map[string]struct{}, error) {
	deps, _, err := d.jobs.Deployments(jobID, false, q)
	if err != nil {
		d.nomadAPIErrors.WithLabelValues("deployments").Inc()
		return nil, err
	}
	failedVersions := make(map[uint64]struct{})
	for _, dep := range deps {
		if dep != nil && dep.Status == nomadapi.DeploymentStatusFailed {
			failedVersions[dep.JobVersion] = struct{}{}
		}
	}
	// History mode with nothing currently failed: no Versions call needed.
	if len(failedVersions) == 0 && mode != "tag" {
		return nil, nil
	}

	versions, _, _, err := d.jobs.Versions(jobID, false, q)
	if err != nil {
		d.nomadAPIErrors.WithLabelValues("versions").Inc()
		return nil, err
	}
	byVersion := make(map[uint64]*nomadapi.Job, len(versions))
	for _, v := range versions {
		if v != nil && v.Version != nil {
			byVersion[*v.Version] = v
		}
	}

	out := make(map[string]struct{})
	// Durable tags survive version GC; recover their fingerprints first.
	if mode == "tag" {
		for _, v := range versions {
			if v == nil || v.VersionTag == nil {
				continue
			}
			if fp, ok := d.parseFailedFingerprint(v.VersionTag.Name); ok {
				out[fp] = struct{}{}
			}
		}
	}
	for ver := range failedVersions {
		v := byVersion[ver]
		if v == nil {
			continue
		}
		fp, err := specFingerprint(v, d.managedMetaPrefix)
		if err != nil {
			continue
		}
		out[fp] = struct{}{}
		if mode == "tag" {
			d.tagFailedVersion(jobID, ver, v, fp, q)
		}
	}
	return out, nil
}

// tagFailedVersion durably tags a failed version so the flap-guard survives
// version GC. A version carries at most one tag, so a version already tagged
// (by us on a prior cycle, or by anything else) is left alone.
func (d *Differ) tagFailedVersion(jobID string, version uint64, v *nomadapi.Job, fingerprint string, q *nomadapi.QueryOptions) {
	if v.VersionTag != nil {
		return
	}
	wq := &nomadapi.WriteOptions{Namespace: d.namespace}
	if _, err := d.jobs.TagVersion(jobID, version, d.failedTagName(fingerprint),
		"nomad-gitops: deployment failed; held by the flap-loop guard", wq); err != nil {
		d.nomadAPIErrors.WithLabelValues("tag").Inc()
		slog.Warn("Flap-guard: could not tag failed version", "job", jobID, "version", version, "err", err)
		return
	}
	d.failedVersionsTagged.WithLabelValues(jobID).Inc()
	slog.Info("Flap-guard: tagged failed version so the block survives GC", "job", jobID, "version", version)
}

// lastStableVersion returns the highest job version strictly below failed that
// is marked Stable — the version to roll back to.
func lastStableVersion(versions []*nomadapi.Job, failed uint64) (uint64, bool) {
	var best uint64
	found := false
	for _, v := range versions {
		if v == nil || v.Version == nil || v.Stable == nil || !*v.Stable {
			continue
		}
		if *v.Version >= failed {
			continue
		}
		if !found || *v.Version > best {
			best = *v.Version
			found = true
		}
	}
	return best, found
}

// checkRollbacks runs the active-rollback poll over the managed jobs. For each
// job that has rollback enabled and whose latest deployment has failed, it
// enqueues a REVERT to the last stable version — unless the job's update stanza
// sets auto_revert, in which case Nomad's own rollback wins and nomad-gitops
// stands down (logged once). Jobs without a deployment are skipped naturally.
// metaByJob carries each managed job's meta (HCL where present, else live).
func (d *Differ) checkRollbacks(metaByJob map[string]map[string]string, q *nomadapi.QueryOptions, raftIndex uint64) {
	for jobID, meta := range metaByJob {
		if !d.effectiveRollback(meta) {
			continue
		}
		// A freeze withholds an active rollback too. Nomad's own auto_revert is
		// outside our control and still fires — the one thing a freeze cannot
		// hold — which is why the stand-down below is separate from this.
		if blocked, why := d.freezeBlocks(jobID); blocked {
			slog.Info("Freeze: holding active rollback", "job", jobID, "reason", why)
			d.appliesHeldByFreeze.WithLabelValues(string(JobUpdateOperationRevert)).Inc()
			d.rollbacks.WithLabelValues(jobID, "held_by_freeze").Inc()
			continue
		}
		dep, _, err := d.jobs.LatestDeployment(jobID, q)
		if err != nil {
			d.nomadAPIErrors.WithLabelValues("deployment").Inc()
			slog.Warn("Rollback: could not read latest deployment", "job", jobID, "err", err)
			continue
		}
		if dep == nil || dep.Status != nomadapi.DeploymentStatusFailed {
			continue
		}

		// The live job decides auto_revert (what Nomad will actually do) and
		// gives us the current version for the CAS guard.
		liveJob, _, err := d.jobs.Info(jobID, q)
		if err != nil {
			d.nomadAPIErrors.WithLabelValues("info").Inc()
			slog.Warn("Rollback: could not read live job", "job", jobID, "err", err)
			continue
		}
		if jobHasAutoRevert(liveJob) {
			// auto_revert always wins. Log the clash once per job so an operator
			// who set both knows nomad-gitops is deliberately standing down.
			if _, seen := d.rollbackLogged.LoadOrStore(jobID, struct{}{}); !seen {
				slog.Warn("Rollback: job has a failed deployment but its update stanza sets auto_revert; standing down and letting Nomad revert",
					"job", jobID)
			}
			d.rollbacks.WithLabelValues(jobID, "deferred_auto_revert").Inc()
			continue
		}

		versions, _, _, err := d.jobs.Versions(jobID, false, q)
		if err != nil {
			d.nomadAPIErrors.WithLabelValues("versions").Inc()
			slog.Warn("Rollback: could not read version history", "job", jobID, "err", err)
			continue
		}
		failedVersion := dep.JobVersion
		target, ok := lastStableVersion(versions, failedVersion)
		if !ok {
			slog.Warn("Rollback: deployment failed but no earlier stable version to revert to; leaving as is",
				"job", jobID, "failed_version", failedVersion)
			d.rollbacks.WithLabelValues(jobID, "no_stable_version").Inc()
			continue
		}

		u := JobUpdate{
			UpdateID:          revertUpdateID(jobID, failedVersion),
			JobID:             jobID,
			Operation:         JobUpdateOperationRevert,
			Status:            JobUpdateStatusPending,
			NomadRaftIndex:    raftIndex,
			DetectedAt:        nowRFC3339(),
			RevertToVersion:   target,
			RevertFromVersion: failedVersion,
		}
		superseded := d.updateQueue.Enqueue(u)
		if superseded > 0 {
			d.jobUpdatesTotal.WithLabelValues(string(JobUpdateOperationRevert), string(JobUpdateStatusSuperseded)).Add(float64(superseded))
		}
		d.pendingUpdates.Set(float64(d.updateQueue.PendingCount()))
		d.rollbacks.WithLabelValues(jobID, "queued").Inc()
		slog.Info("Rollback: enqueued revert of failed deployment to last stable version",
			"job", jobID, "failed_version", failedVersion, "target_version", target, "update_id", u.UpdateID)
		d.notifyApplier()
	}
}
