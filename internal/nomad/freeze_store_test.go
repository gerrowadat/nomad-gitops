package nomad_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	nomadapi "github.com/hashicorp/nomad/api"

	"github.com/gerrowadat/nomad-gitops/internal/nomad"
)

// These tests exercise the real Nomad-Variables-backed freeze store against a
// stub of Nomad's /v1/var API, so the wire shape the store depends on — the
// prefix list, the per-record read, CreateTime, the cas query parameter — is
// covered rather than assumed. Everything else uses the in-memory fake.

const freezePrefix = "nomad/jobs/gitops/freezes"

// varServer is a minimal stand-in for Nomad's Variables API.
type varServer struct {
	mu   sync.Mutex
	vars map[string]*nomadapi.Variable
	// readErrPaths return 500 on a per-record read, to exercise the
	// unreadable-record path.
	readErrPaths map[string]bool
	// casConflict makes the next write return Nomad's CAS conflict.
	casConflict bool

	puts    []string
	deletes []string
	// listPrefix records the prefix the client actually asked for.
	listPrefix string
}

func newVarServer() *varServer {
	return &varServer{vars: map[string]*nomadapi.Variable{}, readErrPaths: map[string]bool{}}
}

// set installs a variable as if an operator had written it.
func (v *varServer) set(path string, items map[string]string, createdAt time.Time, modifyIndex uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.vars[path] = &nomadapi.Variable{
		Namespace:   "default",
		Path:        path,
		Items:       items,
		CreateTime:  createdAt.UnixNano(),
		ModifyTime:  createdAt.UnixNano(),
		ModifyIndex: modifyIndex,
	}
}

func (v *varServer) handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/v1/vars", func(w http.ResponseWriter, r *http.Request) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.listPrefix = r.URL.Query().Get("prefix")
		out := []*nomadapi.VariableMetadata{}
		for path, vr := range v.vars {
			if !strings.HasPrefix(path, v.listPrefix) {
				continue
			}
			out = append(out, &nomadapi.VariableMetadata{
				Namespace:   vr.Namespace,
				Path:        vr.Path,
				CreateTime:  vr.CreateTime,
				ModifyTime:  vr.ModifyTime,
				ModifyIndex: vr.ModifyIndex,
			})
		}
		writeVarJSON(w, out)
	})

	mux.HandleFunc("/v1/var/", func(w http.ResponseWriter, r *http.Request) {
		path := strings.TrimPrefix(r.URL.Path, "/v1/var/")
		v.mu.Lock()
		defer v.mu.Unlock()

		switch r.Method {
		case http.MethodGet:
			if v.readErrPaths[path] {
				http.Error(w, "permission denied", http.StatusForbidden)
				return
			}
			vr, ok := v.vars[path]
			if !ok {
				http.Error(w, "variable not found", http.StatusNotFound)
				return
			}
			writeVarJSON(w, vr)
		case http.MethodPut:
			if v.casConflict {
				v.casConflict = false
				// Nomad answers a failed check-and-set with 409 plus the
				// conflicting variable, which the client turns into
				// ErrCASConflict.
				w.WriteHeader(http.StatusConflict)
				writeVarJSON(w, &nomadapi.Variable{Path: path, ModifyIndex: 999})
				return
			}
			var in nomadapi.Variable
			if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			v.puts = append(v.puts, path+"?"+r.URL.RawQuery)
			stored := &nomadapi.Variable{
				Namespace:   "default",
				Path:        path,
				Items:       in.Items,
				CreateTime:  time.Now().UnixNano(),
				ModifyTime:  time.Now().UnixNano(),
				ModifyIndex: in.ModifyIndex + 1,
			}
			v.vars[path] = stored
			writeVarJSON(w, stored)
		case http.MethodDelete:
			v.deletes = append(v.deletes, path)
			delete(v.vars, path)
			w.WriteHeader(http.StatusOK)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	return mux
}

func writeVarJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// newVarStore starts a stub Nomad and returns a store pointed at it, plus the
// operations the store reported as failing.
func newVarStore(t *testing.T, vs *varServer) (nomad.FreezeStore, *[]string) {
	t.Helper()
	srv := httptest.NewServer(vs.handler())
	t.Cleanup(srv.Close)

	cfg := nomadapi.DefaultConfig()
	cfg.Address = srv.URL
	client, err := nomadapi.NewClient(cfg)
	if err != nil {
		t.Fatalf("nomad client: %v", err)
	}
	var errs []string
	store := nomad.NewNomadVariableFreezeStore(client, freezePrefix, "default", func(op string) {
		errs = append(errs, op)
	})
	return store, &errs
}

func TestVariableFreezeStore_ListReadsItemsAndCreateTime(t *testing.T) {
	vs := newVarServer()
	created := time.Now().Add(-90 * time.Minute).Truncate(time.Second)
	expiry := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)
	vs.set(freezePrefix+"/api-server", map[string]string{
		"reason":     "INC-4412: fixing the migration by hand",
		"expires_at": expiry.Format(time.RFC3339),
		"frozen_by":  "doc",
	}, created, 12)

	store, errs := newVarStore(t, vs)
	recs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	rec := recs[0]
	if rec.JobID != "api-server" {
		t.Errorf("job_id = %q, want api-server", rec.JobID)
	}
	if rec.Reason != "INC-4412: fixing the migration by hand" {
		t.Errorf("reason = %q", rec.Reason)
	}
	if rec.FrozenBy != "doc" {
		t.Errorf("frozen_by = %q, want doc", rec.FrozenBy)
	}
	// FrozenAt comes from the Variable's own CreateTime; nothing writes a
	// created_at item.
	if !rec.Created().Equal(created.UTC()) {
		t.Errorf("frozen_at = %v, want the Variable's CreateTime %v", rec.Created(), created.UTC())
	}
	if !rec.Expiry().Equal(expiry) {
		t.Errorf("expiry = %v, want %v", rec.Expiry(), expiry)
	}
	if rec.State != nomad.FreezeStateActive {
		t.Errorf("state = %q, want active", rec.State)
	}
	if nomad.FreezeRecordModifyIndex(rec) != 12 {
		t.Errorf("modify index = %d, want 12 (the CAS token for the next write)", nomad.FreezeRecordModifyIndex(rec))
	}
	if len(*errs) != 0 {
		t.Errorf("no errors expected, got %v", *errs)
	}
	if vs.listPrefix != freezePrefix+"/" {
		t.Errorf("listed prefix = %q, want the freeze prefix with a trailing slash", vs.listPrefix)
	}
}

func TestVariableFreezeStore_EmptyStoreIsNotAnError(t *testing.T) {
	store, _ := newVarStore(t, newVarServer())
	recs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List on an empty store: %v", err)
	}
	if len(recs) != 0 {
		t.Errorf("want no records, got %d", len(recs))
	}
}

func TestVariableFreezeStore_UnreadableRecordBecomesMalformed(t *testing.T) {
	// An unreadable record must not vanish: dropping it would free the job it
	// names, which is the opposite of what a safety control should do when it
	// cannot be read.
	vs := newVarServer()
	vs.set(freezePrefix+"/api-server", map[string]string{"reason": "incident"}, time.Now(), 1)
	vs.readErrPaths[freezePrefix+"/api-server"] = true

	store, errs := newVarStore(t, vs)
	recs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want the unreadable record kept, got %d records", len(recs))
	}
	if recs[0].State != nomad.FreezeStateMalformed {
		t.Errorf("state = %q, want malformed", recs[0].State)
	}
	if !recs[0].State.InForce() {
		t.Error("an unreadable record must still hold writes")
	}
	if recs[0].JobID != "api-server" {
		t.Errorf("job_id = %q, want the job the path names", recs[0].JobID)
	}
	if len(*errs) == 0 || (*errs)[0] != "read" {
		t.Errorf("a read error should be counted, got %v", *errs)
	}
}

func TestVariableFreezeStore_ListErrorIsReported(t *testing.T) {
	// An error must be distinguishable from an empty store: empty means
	// nothing is frozen, an error does not.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	cfg := nomadapi.DefaultConfig()
	cfg.Address = srv.URL
	client, _ := nomadapi.NewClient(cfg)
	var errs []string
	store := nomad.NewNomadVariableFreezeStore(client, freezePrefix, "default", func(op string) { errs = append(errs, op) })

	if _, err := store.List(context.Background()); err == nil {
		t.Fatal("a failing list must return an error, not an empty slice")
	}
	if len(errs) == 0 || errs[0] != "list" {
		t.Errorf("a list error should be counted, got %v", errs)
	}
}

func TestVariableFreezeStore_JobItemCarriesAnUnslugabbleID(t *testing.T) {
	// A job ID containing '.' cannot be a Variable path segment, so the exact
	// ID travels in a "job" item and the path holds the slug.
	vs := newVarServer()
	vs.set(freezePrefix+"/api-server-v2", map[string]string{
		"reason": "incident",
		"job":    "api-server.v2",
	}, time.Now(), 1)

	store, _ := newVarStore(t, vs)
	recs, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].JobID != "api-server.v2" {
		t.Errorf("job_id = %q, want the exact ID from the job item", recs[0].JobID)
	}
	if recs[0].State != nomad.FreezeStateIndefinite {
		t.Errorf("state = %q, want indefinite", recs[0].State)
	}
}

func TestVariableFreezeStore_JobItemMismatchIsMalformed(t *testing.T) {
	// A record whose job item disagrees with its own path could claim a job it
	// is not addressed for, so it is not trusted — but it is still held.
	vs := newVarServer()
	vs.set(freezePrefix+"/api-server", map[string]string{
		"reason": "incident",
		"job":    "something-else",
	}, time.Now(), 1)

	store, _ := newVarStore(t, vs)
	recs, _ := store.List(context.Background())
	if len(recs) != 1 {
		t.Fatalf("want 1 record, got %d", len(recs))
	}
	if recs[0].State != nomad.FreezeStateMalformed {
		t.Errorf("state = %q, want malformed", recs[0].State)
	}
	if !strings.Contains(recs[0].Issue, "does not match") {
		t.Errorf("issue should explain the mismatch, got %q", recs[0].Issue)
	}
}

func TestVariableFreezeStore_PutUsesCAS(t *testing.T) {
	vs := newVarServer()
	store, _ := newVarStore(t, vs)

	expiry := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	rec, err := store.Put(context.Background(), nomad.NewFreezeRecord("api-server", "incident", "doc", expiry, time.Time{}), 0)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if rec.JobID != "api-server" || rec.Reason != "incident" {
		t.Errorf("stored record = %+v, want the job and reason round-tripped", rec)
	}
	if len(vs.puts) != 1 {
		t.Fatalf("want 1 write, got %d", len(vs.puts))
	}
	// A create must be cas=0, so a record that already exists is a conflict
	// rather than a silent overwrite.
	if !strings.Contains(vs.puts[0], "cas=0") {
		t.Errorf("create should use cas=0, got %q", vs.puts[0])
	}

	// A replace must CAS on the index we last read.
	if _, err := store.Put(context.Background(), nomad.NewFreezeRecord("api-server", "incident", "doc", expiry, time.Time{}), 7); err != nil {
		t.Fatalf("Put (update): %v", err)
	}
	if !strings.Contains(vs.puts[1], "cas=7") {
		t.Errorf("update should CAS on the last-read index, got %q", vs.puts[1])
	}
}

func TestVariableFreezeStore_PutWritesTheJobItemOnlyWhenNeeded(t *testing.T) {
	vs := newVarServer()
	store, _ := newVarStore(t, vs)

	if _, err := store.Put(context.Background(), nomad.NewFreezeRecord("api-server", "incident", "", "", time.Time{}), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, ok := vs.vars[freezePrefix+"/api-server"].Items["job"]; ok {
		t.Error("a path-safe job ID needs no job item")
	}

	if _, err := store.Put(context.Background(), nomad.NewFreezeRecord("api.server", "incident", "", "", time.Time{}), 0); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stored, ok := vs.vars[freezePrefix+"/api-server"]
	if !ok {
		t.Fatal("expected the slugged path to be written")
	}
	if stored.Items["job"] != "api.server" {
		t.Errorf("job item = %q, want the exact job ID", stored.Items["job"])
	}
}

func TestVariableFreezeStore_PutRejectsAReasonlessRecord(t *testing.T) {
	vs := newVarServer()
	store, _ := newVarStore(t, vs)
	if _, err := store.Put(context.Background(), nomad.NewFreezeRecord("api", "", "", "", time.Time{}), 0); err == nil {
		t.Error("a record with no reason should not be writable")
	}
	if len(vs.puts) != 0 {
		t.Errorf("nothing should have been written, got %v", vs.puts)
	}
}

func TestVariableFreezeStore_PutSurfacesCASConflict(t *testing.T) {
	vs := newVarServer()
	vs.casConflict = true
	store, _ := newVarStore(t, vs)

	_, err := store.Put(context.Background(), nomad.NewFreezeRecord("api", "incident", "", "", time.Time{}), 3)
	if !errors.Is(err, nomad.ErrFreezeConflict) {
		t.Errorf("want ErrFreezeConflict so the caller re-reads and retries, got %v", err)
	}
}

func TestVariableFreezeStore_Delete(t *testing.T) {
	vs := newVarServer()
	vs.set(freezePrefix+"/api-server", map[string]string{"reason": "incident"}, time.Now(), 1)
	store, _ := newVarStore(t, vs)

	if err := store.Delete(context.Background(), "api-server"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(vs.deletes) != 1 || vs.deletes[0] != freezePrefix+"/api-server" {
		t.Errorf("deletes = %v, want the record's path", vs.deletes)
	}
	recs, _ := store.List(context.Background())
	if len(recs) != 0 {
		t.Errorf("want the record gone, got %d", len(recs))
	}
}

func TestVariableFreezeStore_DeleteMissingIsNotAnError(t *testing.T) {
	store, _ := newVarStore(t, newVarServer())
	if err := store.Delete(context.Background(), "never-frozen"); err != nil {
		t.Errorf("deleting a record that is not there should succeed, got %v", err)
	}
}

func TestVariableFreezeStore_DeleteErrorIsReported(t *testing.T) {
	// A delete that Nomad refuses (an ACL problem, say) must surface rather
	// than be mistaken for "the record is gone".
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "permission denied", http.StatusForbidden)
	}))
	t.Cleanup(srv.Close)
	cfg := nomadapi.DefaultConfig()
	cfg.Address = srv.URL
	client, _ := nomadapi.NewClient(cfg)
	var errs []string
	store := nomad.NewNomadVariableFreezeStore(client, freezePrefix, "default", func(op string) { errs = append(errs, op) })

	if err := store.Delete(context.Background(), "api-server"); err == nil {
		t.Fatal("a refused delete must return an error")
	}
	if len(errs) == 0 || errs[0] != "delete" {
		t.Errorf("a delete error should be counted, got %v", errs)
	}
}
