package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*fileStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.json")
	fs, err := NewFileStore(path, nil)
	if err != nil {
		t.Fatalf("NewFileStore: %v", err)
	}
	return fs, path
}

func sampleSession() OpenSession {
	return OpenSession{
		CPID:         "CP-1",
		ConnectorID:  1,
		TxID:         1001,
		IdTag:        "TESTRFID0001",
		MeterStartWh: 500,
		StartedAt:    time.Unix(1700000000, 0).UTC(),
		LastMeterWh:  500,
		LastMeterAt:  time.Unix(1700000000, 0).UTC(),
	}
}

func TestPutLoadDelete(t *testing.T) {
	fs, _ := newTestStore(t)
	s := sampleSession()
	if err := fs.Put(s); err != nil {
		t.Fatalf("Put: %v", err)
	}

	open, err := fs.LoadOpen("CP-1")
	if err != nil {
		t.Fatalf("LoadOpen: %v", err)
	}
	if len(open) != 1 || open[0].TxID != 1001 {
		t.Fatalf("LoadOpen = %+v, want one record tx 1001", open)
	}

	if err := fs.Delete("CP-1", 1); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if open, _ := fs.LoadOpen("CP-1"); len(open) != 0 {
		t.Fatalf("after Delete LoadOpen = %+v, want empty", open)
	}
}

func TestUpdateMeter(t *testing.T) {
	fs, _ := newTestStore(t)
	if err := fs.Put(sampleSession()); err != nil {
		t.Fatalf("Put: %v", err)
	}
	at := time.Unix(1700000123, 0).UTC()
	if err := fs.UpdateMeter("CP-1", 1, 8200, at); err != nil {
		t.Fatalf("UpdateMeter: %v", err)
	}
	open, _ := fs.LoadOpen("CP-1")
	if open[0].LastMeterWh != 8200 || !open[0].LastMeterAt.Equal(at) {
		t.Fatalf("UpdateMeter did not persist: %+v", open[0])
	}
	// MeterStart must be untouched — recovery reports energy = LastMeter - MeterStart.
	if open[0].MeterStartWh != 500 {
		t.Fatalf("UpdateMeter clobbered MeterStartWh: %+v", open[0])
	}
}

func TestUpdateMeterNoSessionIsNoop(t *testing.T) {
	fs, _ := newTestStore(t)
	if err := fs.UpdateMeter("CP-1", 1, 999, time.Now()); err != nil {
		t.Fatalf("UpdateMeter on empty store should be a no-op, got %v", err)
	}
	if open, _ := fs.LoadOpen("CP-1"); len(open) != 0 {
		t.Fatalf("UpdateMeter fabricated a record: %+v", open)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	fs, path := newTestStore(t)
	if err := fs.Put(sampleSession()); err != nil {
		t.Fatalf("Put: %v", err)
	}

	// Simulate a process restart: a brand-new store over the same file must see
	// the open session (this is exactly the mode-3 recovery path).
	fs2, err := NewFileStore(path, nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	open, _ := fs2.LoadOpen("CP-1")
	if len(open) != 1 || open[0].TxID != 1001 {
		t.Fatalf("reopened store lost the session: %+v", open)
	}
}

func TestDualGunIndependent(t *testing.T) {
	fs, _ := newTestStore(t)
	a := sampleSession()
	b := sampleSession()
	b.ConnectorID, b.TxID = 2, 1002
	if err := fs.Put(a); err != nil {
		t.Fatal(err)
	}
	if err := fs.Put(b); err != nil {
		t.Fatal(err)
	}
	if open, _ := fs.LoadOpen("CP-1"); len(open) != 2 {
		t.Fatalf("want 2 independent sessions, got %d", len(open))
	}
	// Deleting one leaves the other.
	if err := fs.Delete("CP-1", 1); err != nil {
		t.Fatal(err)
	}
	open, _ := fs.LoadOpen("CP-1")
	if len(open) != 1 || open[0].ConnectorID != 2 {
		t.Fatalf("Delete affected the wrong connector: %+v", open)
	}
}

func TestLoadOpenFiltersByCPID(t *testing.T) {
	fs, _ := newTestStore(t)
	a := sampleSession()
	b := sampleSession()
	b.CPID, b.TxID = "CP-2", 2001
	_ = fs.Put(a)
	_ = fs.Put(b)
	if open, _ := fs.LoadOpen("CP-1"); len(open) != 1 || open[0].CPID != "CP-1" {
		t.Fatalf("LoadOpen leaked another CPID: %+v", open)
	}
	if open, _ := fs.LoadOpen("CP-2"); len(open) != 1 || open[0].CPID != "CP-2" {
		t.Fatalf("LoadOpen(CP-2) wrong: %+v", open)
	}
}

func TestMissingFileStartsEmpty(t *testing.T) {
	path := filepath.Join(t.TempDir(), "does-not-exist.json")
	fs, err := NewFileStore(path, nil)
	if err != nil {
		t.Fatalf("NewFileStore on missing file should succeed, got %v", err)
	}
	if open, _ := fs.LoadOpen("CP-1"); len(open) != 0 {
		t.Fatalf("missing-file store not empty: %+v", open)
	}
}

func TestCorruptFileStartsEmptyAndHeals(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sessions.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	fs, err := NewFileStore(path, nil)
	if err != nil {
		t.Fatalf("corrupt file should not fail NewFileStore, got %v", err)
	}
	if open, _ := fs.LoadOpen("CP-1"); len(open) != 0 {
		t.Fatalf("corrupt store should be empty, got %+v", open)
	}
	// A subsequent write must heal the file to valid JSON.
	if err := fs.Put(sampleSession()); err != nil {
		t.Fatalf("Put after corrupt load: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var recs []OpenSession
	if err := json.Unmarshal(b, &recs); err != nil {
		t.Fatalf("store did not heal to valid JSON: %v (%s)", err, b)
	}
	if len(recs) != 1 {
		t.Fatalf("healed store contents wrong: %+v", recs)
	}
}

func TestPutReplacesSameKey(t *testing.T) {
	fs, _ := newTestStore(t)
	s := sampleSession()
	_ = fs.Put(s)
	s.TxID = 1099 // same (CPID, ConnectorID), new tx
	_ = fs.Put(s)
	open, _ := fs.LoadOpen("CP-1")
	if len(open) != 1 || open[0].TxID != 1099 {
		t.Fatalf("Put should replace by (CPID,ConnectorID): %+v", open)
	}
}
