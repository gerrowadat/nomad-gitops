package nomad

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strings"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"
)

// A freeze is an operator-set, temporary hold on nomad-gitops's authority to
// write to one job. It lives in Nomad Variables rather than in this process or
// in Git: the hold has to survive a restart and be settable during an incident
// without a commit, and Nomad's Raft log already holds durable truth for this
// system. See docs/design/temporary-freeze.md for the reasoning, in particular
// why a non-Git input may withhold write authority but never grant it.

// FreezeState classifies a freeze record as read from the store.
type FreezeState string

const (
	// FreezeStateActive is a freeze with an expiry still in the future.
	FreezeStateActive FreezeState = "active"
	// FreezeStateIndefinite is a freeze with no expires_at. Permitted, but it
	// never lifts on its own, so it is reported and alerted on.
	FreezeStateIndefinite FreezeState = "indefinite"
	// FreezeStateExpired is a record past its expires_at. It suppresses
	// nothing — the job reconciles normally again — but it is left in place
	// rather than deleted, because it is the only record of why the job was
	// frozen and somebody meant to come back to it.
	FreezeStateExpired FreezeState = "expired"
	// FreezeStateMalformed is a record nomad-gitops cannot make sense of. It
	// is read conservatively as an indefinite freeze on the job it appears to
	// name: a safety control that cannot be parsed must not silently fail open.
	FreezeStateMalformed FreezeState = "malformed"
)

// freezeStates is every state, in reporting order, so metrics and the API
// expose a stable set of series rather than only the states in use.
var freezeStates = []FreezeState{
	FreezeStateActive,
	FreezeStateIndefinite,
	FreezeStateExpired,
	FreezeStateMalformed,
}

// InForce reports whether a record in this state withholds writes.
func (s FreezeState) InForce() bool {
	switch s {
	case FreezeStateActive, FreezeStateIndefinite, FreezeStateMalformed:
		return true
	}
	return false
}

// Freeze record item keys. All values are strings: Nomad Variable items are a
// flat map[string]string, which is deliberate here — it keeps a record
// writable by hand with `nomad var put k=v` and nothing to quote-escape.
const (
	freezeItemReason    = "reason"
	freezeItemExpiresAt = "expires_at"
	freezeItemFrozenBy  = "frozen_by"
	freezeItemJob       = "job"
)

// maxVariablePathBytes is Nomad's limit on a Variable path. Quoting the
// `nomad var put` documentation: "Paths may be up to 128 bytes long", validated
// against ^[a-zA-Z0-9-_~/]{1,128}$.
const maxVariablePathBytes = 128

// freezePathUnsafeRe matches any character Nomad will not accept in a Variable
// path segment. Nomad job IDs are more permissive than Variable paths (a job ID
// may contain '.', a path may not), so the path segment is a slug of the job ID
// and the exact ID travels in the record's "job" item.
var freezePathUnsafeRe = regexp.MustCompile(`[^A-Za-z0-9_~-]`)

// FreezeSlug maps a job ID to its Variable path segment. Exported because the
// path encoding is part of the FreezeStore contract: an alternative backend,
// or tooling that addresses freeze records directly, has to agree with it.
func FreezeSlug(jobID string) string {
	return freezePathUnsafeRe.ReplaceAllString(jobID, "-")
}

// FreezeRecord is one job's freeze, as stored in a Nomad Variable.
type FreezeRecord struct {
	JobID        string      `json:"job_id"`
	VariablePath string      `json:"variable_path"`
	Reason       string      `json:"reason,omitempty"`
	FrozenBy     string      `json:"frozen_by,omitempty"`
	FrozenAt     string      `json:"frozen_at,omitempty"`  // RFC3339; the Variable's CreateTime
	ExpiresAt    string      `json:"expires_at,omitempty"` // RFC3339; empty means indefinite
	State        FreezeState `json:"state"`
	// Issue explains a malformed record. Empty otherwise.
	Issue string `json:"issue,omitempty"`

	frozenAt    time.Time
	expiresAt   time.Time
	modifyIndex uint64
}

// Classify sets State from the record's contents, relative to now. Reading a
// record and classifying it are separate steps so that the clock belongs to the
// caller: a record does not change in the store when it expires.
func (r *FreezeRecord) Classify(now time.Time) {
	switch {
	case r.Issue != "":
		r.State = FreezeStateMalformed
	case r.expiresAt.IsZero():
		r.State = FreezeStateIndefinite
	case r.expiresAt.After(now):
		r.State = FreezeStateActive
	default:
		r.State = FreezeStateExpired
	}
}

// Describe renders the record as a phrase for a log line, an apply_detail, or
// the console — "frozen until <t> (<reason>)".
func (r FreezeRecord) Describe(now time.Time) string {
	var when string
	switch r.State {
	case FreezeStateMalformed:
		return fmt.Sprintf("frozen by a malformed freeze record at %s, read conservatively as indefinite (%s)", r.VariablePath, r.Issue)
	case FreezeStateIndefinite:
		when = "indefinitely (no expires_at set)"
	case FreezeStateExpired:
		when = fmt.Sprintf("until %s (expired)", r.ExpiresAt)
	default:
		when = fmt.Sprintf("until %s (%s from now)", r.ExpiresAt, r.expiresAt.Sub(now).Round(time.Second))
	}
	if r.Reason != "" {
		return fmt.Sprintf("frozen %s: %s", when, r.Reason)
	}
	return "frozen " + when
}

// NewFreezeRecord builds a record from its string fields, parsing expiresAt and
// setting Issue when the record is not well formed (no reason, or an expiry
// that is not RFC3339). frozenAt may be the zero time. It is exported because
// FreezeStore is: an alternative backend needs to be able to produce records
// the differ reads correctly, including the parsed times Classify works from.
func NewFreezeRecord(jobID, reason, frozenBy, expiresAt string, frozenAt time.Time) FreezeRecord {
	rec := FreezeRecord{
		JobID:    jobID,
		Reason:   strings.TrimSpace(reason),
		FrozenBy: strings.TrimSpace(frozenBy),
	}
	if !frozenAt.IsZero() {
		rec.frozenAt = frozenAt.UTC()
		rec.FrozenAt = rec.frozenAt.Format(time.RFC3339)
	}
	if rec.Reason == "" {
		rec.Issue = fmt.Sprintf("record has no %q item; an unexplained freeze is not honoured as a well-formed one", freezeItemReason)
		return rec
	}
	if exp := strings.TrimSpace(expiresAt); exp != "" {
		t, err := time.Parse(time.RFC3339, exp)
		if err != nil {
			rec.Issue = fmt.Sprintf("%s %q is not an RFC3339 timestamp", freezeItemExpiresAt, exp)
			return rec
		}
		rec.expiresAt = t.UTC()
		rec.ExpiresAt = rec.expiresAt.Format(time.RFC3339)
	}
	return rec
}

// Created is when the freeze record was created (the Variable's CreateTime),
// or the zero time if the store did not report one.
func (r FreezeRecord) Created() time.Time { return r.frozenAt }

// Expiry is when the freeze expires, or the zero time for an indefinite one.
func (r FreezeRecord) Expiry() time.Time { return r.expiresAt }

// ErrFreezeConflict reports that a freeze write lost a CAS race: the record
// changed between being read and being written. The caller re-reads and
// retries rather than overwriting someone else's change.
var ErrFreezeConflict = errors.New("freeze record conflict")

// isCASConflict reports whether err is Nomad's check-and-set rejection.
func isCASConflict(err error) bool {
	var cas nomadapi.ErrCASConflict
	return errors.As(err, &cas)
}

// FreezeStore holds operator-set holds on nomad-gitops's write authority. The
// store is the source of truth; nomad-gitops caches reads but never invents a
// record.
type FreezeStore interface {
	// List returns every freeze record, including expired and malformed ones.
	// A record that cannot be read individually is returned as malformed
	// rather than omitted, so an unreadable record cannot fail open. An error
	// means the listing itself failed and the result must not be read as "the
	// store is empty".
	List(ctx context.Context) ([]FreezeRecord, error)

	// Put writes a record, CAS-protected on modifyIndex (0 to create), and
	// returns the stored record including its new ModifyIndex. It must return
	// an error wrapping ErrFreezeConflict when the CAS check fails, so the
	// caller can re-read and retry rather than overwrite. The returned
	// record's State is reclassified by the caller.
	Put(ctx context.Context, rec FreezeRecord, modifyIndex uint64) (FreezeRecord, error)

	// Delete purges a record. Deleting one that does not exist is not an error.
	Delete(ctx context.Context, jobID string) error
}

// nomadVariableFreezeStore is the FreezeStore backed by Nomad Variables.
type nomadVariableFreezeStore struct {
	vars      *nomadapi.Variables
	prefix    string
	namespace string
	// errFn counts a failing operation ("list", "read", "write", "delete") so
	// the store does not need to know about a metrics registry.
	errFn func(op string)
	now   func() time.Time
}

// newNomadVariableFreezeStore builds a FreezeStore over the given client.
func newNomadVariableFreezeStore(c *nomadapi.Client, prefix, namespace string, errFn func(op string)) *nomadVariableFreezeStore {
	if errFn == nil {
		errFn = func(string) {}
	}
	return &nomadVariableFreezeStore{
		vars:      c.Variables(),
		prefix:    strings.Trim(prefix, "/"),
		namespace: namespace,
		errFn:     errFn,
		now:       time.Now,
	}
}

// pathFor returns the Variable path for a job, and whether the slug differs
// from the job ID (in which case the record carries a "job" item).
func (s *nomadVariableFreezeStore) pathFor(jobID string) (string, bool) {
	slug := FreezeSlug(jobID)
	return s.prefix + "/" + slug, slug != jobID
}

// validateFreezePath reports an error when a job cannot be addressed in the
// store at all. A job whose ID is too long to freeze is something an operator
// needs to know before an incident, not during one.
func validateFreezePath(path string) error {
	if len(path) > maxVariablePathBytes {
		return fmt.Errorf("freeze variable path %q is %d bytes; Nomad limits a Variable path to %d",
			path, len(path), maxVariablePathBytes)
	}
	return nil
}

func (s *nomadVariableFreezeStore) queryOpts(ctx context.Context) *nomadapi.QueryOptions {
	return (&nomadapi.QueryOptions{Namespace: s.namespace}).WithContext(ctx)
}

func (s *nomadVariableFreezeStore) writeOpts(ctx context.Context) *nomadapi.WriteOptions {
	return (&nomadapi.WriteOptions{Namespace: s.namespace}).WithContext(ctx)
}

func (s *nomadVariableFreezeStore) List(ctx context.Context) ([]FreezeRecord, error) {
	metas, _, err := s.vars.PrefixList(s.prefix+"/", s.queryOpts(ctx))
	if err != nil {
		s.errFn("list")
		return nil, fmt.Errorf("listing freeze variables under %q: %w", s.prefix, err)
	}

	out := make([]FreezeRecord, 0, len(metas))
	for _, m := range metas {
		v, _, err := s.vars.Peek(m.Path, s.queryOpts(ctx))
		if err != nil || v == nil {
			// Unreadable: keep it as a malformed record for the job the path
			// names rather than dropping it, so a read failure on one record
			// withholds writes to that job instead of silently freeing it.
			s.errFn("read")
			issue := "record could not be read"
			if err != nil {
				issue = fmt.Sprintf("record could not be read: %v", err)
			}
			slog.Warn("Freeze: could not read freeze record; treating as an indefinite freeze", "path", m.Path, "err", err)
			out = append(out, FreezeRecord{
				JobID:        s.jobIDFromPath(m.Path),
				VariablePath: m.Path,
				Issue:        issue,
				State:        FreezeStateMalformed,
			})
			continue
		}
		rec := s.recordFromVariable(v)
		rec.Classify(s.now())
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].JobID < out[j].JobID })
	return out, nil
}

// jobIDFromPath recovers the job ID a path names, used when the record's own
// items are unavailable. It is the slug, which equals the job ID for every
// path-safe job ID.
func (s *nomadVariableFreezeStore) jobIDFromPath(path string) string {
	return path[strings.LastIndex(path, "/")+1:]
}

// recordFromVariable parses a Variable into a record, setting Issue for
// anything that makes the record untrustworthy. It does not set State; the
// caller classifies against its own clock.
func (s *nomadVariableFreezeStore) recordFromVariable(v *nomadapi.Variable) FreezeRecord {
	segment := s.jobIDFromPath(v.Path)
	jobID := segment
	var frozenAt time.Time
	if v.CreateTime != 0 {
		frozenAt = time.Unix(0, v.CreateTime).UTC()
	}

	// An explicit "job" item carries the exact job ID for a job whose slug
	// differs from it (a job ID containing '.', say). It must agree with the
	// path it sits at, or the record could claim a job it is not addressed
	// for — two records could then both claim one job.
	mismatch := ""
	if j := strings.TrimSpace(v.Items[freezeItemJob]); j != "" {
		if FreezeSlug(j) != segment {
			mismatch = fmt.Sprintf("job item %q does not match the record's path segment %q", j, segment)
		} else {
			jobID = j
		}
	}

	rec := NewFreezeRecord(jobID, v.Items[freezeItemReason], v.Items[freezeItemFrozenBy], v.Items[freezeItemExpiresAt], frozenAt)
	rec.VariablePath = v.Path
	rec.modifyIndex = v.ModifyIndex
	if mismatch != "" {
		rec.Issue = mismatch
	}
	return rec
}

func (s *nomadVariableFreezeStore) Put(ctx context.Context, rec FreezeRecord, modifyIndex uint64) (FreezeRecord, error) {
	path, needsJobItem := s.pathFor(rec.JobID)
	if err := validateFreezePath(path); err != nil {
		return FreezeRecord{}, err
	}
	if strings.TrimSpace(rec.Reason) == "" {
		return FreezeRecord{}, errors.New("a freeze needs a reason")
	}

	v := nomadapi.NewVariable(path)
	v.Namespace = s.namespace
	v.ModifyIndex = modifyIndex
	v.Items[freezeItemReason] = rec.Reason
	if rec.FrozenBy != "" {
		v.Items[freezeItemFrozenBy] = rec.FrozenBy
	}
	if rec.ExpiresAt != "" {
		v.Items[freezeItemExpiresAt] = rec.ExpiresAt
	}
	if needsJobItem {
		v.Items[freezeItemJob] = rec.JobID
	}

	// CAS on every write: cas=0 to create (fails if a record already exists),
	// the last-read ModifyIndex to replace one. A racing write — another
	// replica, or a human `nomad var put` — loses cleanly rather than being
	// silently clobbered.
	var out *nomadapi.Variable
	var err error
	if modifyIndex == 0 {
		out, _, err = s.vars.CheckedCreate(v, s.writeOpts(ctx))
	} else {
		out, _, err = s.vars.CheckedUpdate(v, s.writeOpts(ctx))
	}
	if err != nil {
		if isCASConflict(err) {
			return FreezeRecord{}, fmt.Errorf("%w: freeze record %q changed since it was read", ErrFreezeConflict, path)
		}
		s.errFn("write")
		return FreezeRecord{}, fmt.Errorf("writing freeze record %q: %w", path, err)
	}
	stored := s.recordFromVariable(out)
	stored.Classify(s.now())
	return stored, nil
}

func (s *nomadVariableFreezeStore) Delete(ctx context.Context, jobID string) error {
	path, _ := s.pathFor(jobID)
	if _, err := s.vars.Delete(path, s.writeOpts(ctx)); err != nil {
		if errors.Is(err, nomadapi.ErrVariablePathNotFound) || isNotFound(err) {
			return nil
		}
		s.errFn("delete")
		return fmt.Errorf("deleting freeze record %q: %w", path, err)
	}
	return nil
}

// ── Differ-side freeze view ───────────────────────────────────────────────────

// freezeView is the Differ's cached read of the freeze store. The store is
// authoritative; this is only how recently we looked.
type freezeView struct {
	records  []FreezeRecord
	byJob    map[string]FreezeRecord // in-force records only
	lastRead time.Time
	readOK   bool // a successful read has happened at least once
}

// FreezeEnabled reports whether freeze support is configured.
func (d *Differ) FreezeEnabled() bool {
	return d.freezeStore != nil
}

// FreezeDefaultTTL is the expiry applied to a freeze created through the API
// that does not specify one. 0 means indefinite.
func (d *Differ) FreezeDefaultTTL() time.Duration {
	return d.freezeDefaultTTL
}

// SetFreezeStore replaces the freeze store. Used by tests to inject a fake.
func (d *Differ) SetFreezeStore(s FreezeStore) {
	d.freezeMu.Lock()
	d.freezeStore = s
	d.freezeMu.Unlock()
}

// Freezes returns the current freeze records and the time of the last
// successful read, for the JSON API and the console.
func (d *Differ) Freezes() ([]FreezeRecord, time.Time, bool) {
	d.freezeMu.RLock()
	defer d.freezeMu.RUnlock()
	out := make([]FreezeRecord, len(d.freezes.records))
	copy(out, d.freezes.records)
	return out, d.freezes.lastRead, d.freezes.readOK
}

// refreshFreezes re-reads the freeze store and updates the cached view and the
// freeze metrics. A failed read leaves the previous view in force — stale but
// safe beats fresh and empty — and the staleness window in freezeHold decides
// when a stale view stops being good enough to write against.
//
// This runs before the Raft-index skip in Check, not after: a Variable write
// does not move the jobs table's index, so a freeze set during a quiet period
// would otherwise not be reported until something else changed.
func (d *Differ) refreshFreezes(ctx context.Context) {
	d.freezeMu.RLock()
	store := d.freezeStore
	d.freezeMu.RUnlock()
	if store == nil {
		return
	}

	recs, err := store.List(ctx)
	now := time.Now()
	if err != nil {
		d.freezeMu.RLock()
		everRead := d.freezes.readOK
		d.freezeMu.RUnlock()
		if everRead {
			slog.Warn("Freeze: could not read the freeze store; keeping the previous view. Applies are held once the view passes --freeze-max-staleness",
				"err", err)
		} else {
			// Never once readable: freezes are not being honoured at all.
			// Logged once so a missing ACL policy is obvious without
			// drowning the log every cycle.
			d.freezeUnreadableLogged.Do(func() {
				slog.Error("Freeze: the freeze store has never been readable, so freezes are NOT being honoured. Grant variables read+list on the freeze prefix, or set --freeze-variable-prefix=\"\" to disable freeze support",
					"err", err)
			})
		}
		d.updateFreezeMetrics()
		return
	}

	byJob := make(map[string]FreezeRecord, len(recs))
	for i := range recs {
		recs[i].Classify(now)
		if recs[i].State.InForce() {
			byJob[recs[i].JobID] = recs[i]
		}
	}

	d.freezeMu.Lock()
	prevHeld := d.freezeHeldSince
	held := make(map[string]time.Time, len(byJob))
	for jobID := range byJob {
		// Accrue time spent frozen so a freeze holds a grace period rather
		// than spending it: see graceOffset.
		if since, ok := prevHeld[jobID]; ok {
			d.freezeAccrued[jobID] += now.Sub(since)
		}
		held[jobID] = now
	}
	d.freezeHeldSince = held
	d.freezes = freezeView{records: recs, byJob: byJob, lastRead: now, readOK: true}
	d.freezeMu.Unlock()

	d.updateFreezeMetrics()
}

// freezeFor returns the in-force freeze record for a job, if any.
func (d *Differ) freezeFor(jobID string) (FreezeRecord, bool) {
	d.freezeMu.RLock()
	defer d.freezeMu.RUnlock()
	rec, ok := d.freezes.byJob[jobID]
	return rec, ok
}

// freezeHold reports whether every write must be withheld because the freeze
// view cannot be trusted, with a reason. A freeze that gets ignored because of
// an ACL error is a broken safety control, so once the store has been read
// successfully a later read failure holds applies rather than assuming nothing
// is frozen. Detection and reporting continue regardless. --freeze-fail-open
// takes the other trade.
//
// The deliberate exception is a store that has *never* been read successfully.
// That is treated as "freezes are not in use here" and holds nothing: freeze
// support is on by default, so a deployment whose ACL policy predates it (no
// variables:read) would otherwise have every apply stopped by a feature it is
// not using. Nobody can be relying on a freeze that nomad-gitops has never
// once been able to see, and the condition is loud — the store-error counter
// rises, /api/v1/freezes reports store_healthy false, the console says freezes
// are not being honoured, and NomadGitopsFreezeStoreErrors fires.
func (d *Differ) freezeHold() (bool, string) {
	d.freezeMu.RLock()
	defer d.freezeMu.RUnlock()
	if d.freezeStore == nil || d.freezeFailOpen || !d.freezes.readOK {
		return false, ""
	}
	if d.freezeMaxStaleness > 0 {
		if age := time.Since(d.freezes.lastRead); age > d.freezeMaxStaleness {
			return true, fmt.Sprintf("the freeze view is %s stale (older than --freeze-max-staleness=%s), so nomad-gitops cannot tell which jobs are frozen (fail-closed; see --freeze-fail-open)",
				age.Round(time.Second), d.freezeMaxStaleness)
		}
	}
	return false, ""
}

// freezeBlocks reports whether a write to jobID must be withheld and why. It
// answers for both a freeze on that job and an untrustworthy freeze view.
func (d *Differ) freezeBlocks(jobID string) (bool, string) {
	if rec, ok := d.freezeFor(jobID); ok {
		return true, "not applied: " + rec.Describe(time.Now())
	}
	if hold, why := d.freezeHold(); hold {
		return true, "not applied: " + why
	}
	return false, ""
}

// graceOffset is how long a job has spent frozen, which is subtracted from its
// orphan grace period: freezing an orphaned job buys time without spending it,
// so a job 4 minutes into a 5-minute grace when frozen is still 4 minutes in
// when thawed.
func (d *Differ) graceOffset(jobID string) time.Duration {
	d.freezeMu.RLock()
	defer d.freezeMu.RUnlock()
	off := d.freezeAccrued[jobID]
	if since, ok := d.freezeHeldSince[jobID]; ok {
		off += time.Since(since)
	}
	return off
}

// clearFreezeAccrual drops a job's accrued frozen time, called when the job
// stops being orphaned so a later orphaning starts its grace period clean.
func (d *Differ) clearFreezeAccrual(jobID string) {
	d.freezeMu.Lock()
	delete(d.freezeAccrued, jobID)
	d.freezeMu.Unlock()
}

// updateFreezeMetrics republishes the freeze gauges from the cached view. The
// per-job series are reset first so a thawed job's series disappears rather
// than sticking at a stale value.
func (d *Differ) updateFreezeMetrics() {
	if d.jobFrozen == nil {
		return // metrics not registered (freeze support disabled)
	}
	recs, lastRead, readOK := d.Freezes()

	d.jobFrozen.Reset()
	d.jobFreezeCreated.Reset()
	d.jobFreezeExpires.Reset()

	counts := make(map[FreezeState]int, len(freezeStates))
	frozen := 0
	for _, r := range recs {
		counts[r.State]++
		if !r.State.InForce() {
			continue
		}
		frozen++
		d.jobFrozen.WithLabelValues(r.JobID).Set(1)
		if !r.frozenAt.IsZero() {
			d.jobFreezeCreated.WithLabelValues(r.JobID).Set(float64(r.frozenAt.Unix()))
		}
		// Deliberately absent for an indefinite freeze: that absence is what
		// makes indefinite freezes queryable with `unless on(nomad_job)`.
		if !r.expiresAt.IsZero() {
			d.jobFreezeExpires.WithLabelValues(r.JobID).Set(float64(r.expiresAt.Unix()))
		}
	}
	for _, st := range freezeStates {
		d.freezeRecords.WithLabelValues(string(st)).Set(float64(counts[st]))
	}
	d.frozenJobs.Set(float64(frozen))
	if readOK && !lastRead.IsZero() {
		d.freezeStoreLastRead.Set(float64(lastRead.Unix()))
	}
}

// ── Freeze mutation (the API write path) ──────────────────────────────────────

// FreezeJob places or replaces a freeze on a job. expiresAt is the RFC3339
// expiry, or empty for an indefinite freeze. It is CAS-protected against the
// record's last-read ModifyIndex, so a racing write loses cleanly.
func (d *Differ) FreezeJob(ctx context.Context, jobID, reason, frozenBy, expiresAt string) (FreezeRecord, error) {
	d.freezeMu.RLock()
	store := d.freezeStore
	d.freezeMu.RUnlock()
	if store == nil {
		return FreezeRecord{}, errors.New("freeze support is disabled (--freeze-variable-prefix is empty)")
	}
	if strings.TrimSpace(jobID) == "" {
		return FreezeRecord{}, errors.New("a freeze needs a job ID")
	}
	if strings.TrimSpace(reason) == "" {
		return FreezeRecord{}, errors.New("a freeze needs a reason")
	}
	if expiresAt != "" {
		if _, err := time.Parse(time.RFC3339, expiresAt); err != nil {
			return FreezeRecord{}, fmt.Errorf("expires_at %q is not an RFC3339 timestamp: %w", expiresAt, err)
		}
	}

	// CAS against what we last read, so replacing a freeze cannot clobber one
	// somebody else wrote in the meantime.
	var modifyIndex uint64
	if existing, ok := d.freezeRecordAny(jobID); ok {
		modifyIndex = existing.modifyIndex
	}

	want := FreezeRecord{
		JobID:     jobID,
		Reason:    reason,
		FrozenBy:  frozenBy,
		ExpiresAt: expiresAt,
	}
	rec, err := store.Put(ctx, want, modifyIndex)
	if errors.Is(err, ErrFreezeConflict) {
		// Somebody wrote this record between our last read and now. Re-read
		// and retry once against what is actually there; a second conflict is
		// returned to the caller rather than retried in a loop.
		d.refreshFreezes(ctx)
		var retryIndex uint64
		if existing, ok := d.freezeRecordAny(jobID); ok {
			retryIndex = existing.modifyIndex
		}
		rec, err = store.Put(ctx, want, retryIndex)
	}
	if err != nil {
		return FreezeRecord{}, err
	}
	// Classify here rather than trusting the backend to have done it, so the
	// record handed back is correctly stated whichever store is behind this.
	rec.Classify(time.Now())
	slog.Info("Freeze: job frozen", "job", jobID, "expires_at", expiresAt, "frozen_by", frozenBy, "reason", reason)
	d.refreshFreezes(ctx)
	return rec, nil
}

// ThawJob removes a job's freeze. Removing one that is not there is not an
// error: the caller asked for the job not to be frozen, and it is not.
func (d *Differ) ThawJob(ctx context.Context, jobID string) error {
	d.freezeMu.RLock()
	store := d.freezeStore
	d.freezeMu.RUnlock()
	if store == nil {
		return errors.New("freeze support is disabled (--freeze-variable-prefix is empty)")
	}
	if err := store.Delete(ctx, jobID); err != nil {
		return err
	}
	slog.Info("Freeze: job thawed", "job", jobID)
	d.clearFreezeAccrual(jobID)
	d.refreshFreezes(ctx)
	return nil
}

// freezeRecordAny returns a job's record whatever its state, including expired
// ones (which are not in the in-force map but still occupy the Variable path).
func (d *Differ) freezeRecordAny(jobID string) (FreezeRecord, bool) {
	d.freezeMu.RLock()
	defer d.freezeMu.RUnlock()
	for _, r := range d.freezes.records {
		if r.JobID == jobID {
			return r, true
		}
	}
	return FreezeRecord{}, false
}

// purgeExpiredFreezes deletes records that have been expired for at least
// --freeze-expired-retention. Off by default: a tool that deletes the audit
// trail of its own withheld writes should be opted into.
func (d *Differ) purgeExpiredFreezes(ctx context.Context) {
	if d.freezeExpiredRetention <= 0 {
		return
	}
	d.freezeMu.RLock()
	store := d.freezeStore
	d.freezeMu.RUnlock()
	if store == nil {
		return
	}

	recs, _, _ := d.Freezes()
	now := time.Now()
	purged := 0
	for _, r := range recs {
		if r.State != FreezeStateExpired || r.expiresAt.IsZero() {
			continue
		}
		if now.Sub(r.expiresAt) < d.freezeExpiredRetention {
			continue
		}
		if err := store.Delete(ctx, r.JobID); err != nil {
			slog.Warn("Freeze: could not purge expired freeze record", "job", r.JobID, "err", err)
			continue
		}
		slog.Info("Freeze: purged expired freeze record", "job", r.JobID,
			"expired_at", r.ExpiresAt, "retention", d.freezeExpiredRetention)
		d.clearFreezeAccrual(r.JobID)
		purged++
	}
	if purged > 0 {
		d.refreshFreezes(ctx)
	}
}
