// Package session persists open OCPP transactions to disk so a charger can
// close them out after any interruption of the CMS link.
//
// The record for each open session carries everything needed to build a
// StopTransaction WITHOUT any live in-memory session state, so recovery works
// even across a full process restart (when the actor's txID is long gone):
//
//   - written when StartTransaction succeeds,
//   - refreshed on every online MeterValues tick (LastMeterWh / LastMeterAt),
//   - deleted only when a StopTransaction is confirmed by the CMS.
//
// Because the actor only samples the meter while online, the last persisted
// reading naturally freezes at the pre-interruption value — no separate freeze
// logic is needed. On the next boot the charger loads any open records for its
// CPID and replays a StopTransaction(PowerLoss) for each.
package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// OpenSession is one persisted, not-yet-stopped transaction.
type OpenSession struct {
	CPID         string    `json:"cp_id"`
	ConnectorID  int       `json:"connector_id"`
	TxID         int       `json:"tx_id"`
	IdTag        string    `json:"id_tag"`
	MeterStartWh int       `json:"meter_start_wh"`
	StartedAt    time.Time `json:"started_at"`
	// LastMeterWh / LastMeterAt are refreshed on each online meter tick and thus
	// hold the last reading seen before an outage. The recovery StopTransaction
	// reports these as meterStop + timestamp.
	LastMeterWh int       `json:"last_meter_wh"`
	LastMeterAt time.Time `json:"last_meter_at"`
}

// Store persists open sessions, keyed by (CPID, ConnectorID).
type Store interface {
	// Put inserts or replaces the open session for its (CPID, ConnectorID).
	Put(s OpenSession) error
	// UpdateMeter refreshes the last-seen meter reading for an open session.
	// No-op if no session is currently open for that key.
	UpdateMeter(cpid string, connectorID, lastMeterWh int, at time.Time) error
	// Delete removes the open session for (CPID, ConnectorID). No-op if absent.
	Delete(cpid string, connectorID int) error
	// LoadOpen returns every open session for the given CPID.
	LoadOpen(cpid string) ([]OpenSession, error)
}

func key(cpid string, connectorID int) string {
	return fmt.Sprintf("%s\x1f%d", cpid, connectorID)
}

// fileStore is a JSON-file Store. Records are held in memory and the whole file
// is rewritten atomically on every mutation. A single JSON file serves every
// charger the process hosts (records are keyed by CPID), which is fine at the
// current one-charger scale; a large fleet should move to per-CPID files or a
// real database (the Store interface makes that swap transparent).
type fileStore struct {
	mu       sync.Mutex
	path     string
	log      *slog.Logger
	sessions map[string]OpenSession
}

// NewFileStore opens (or creates) the JSON store at path. A missing file starts
// empty; a corrupt file is logged and treated as empty (the next write heals it),
// so a bad store can never stop the worker from booting.
func NewFileStore(path string, log *slog.Logger) (*fileStore, error) {
	if log == nil {
		log = slog.Default()
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("session store dir %q: %w", dir, err)
		}
	}
	fs := &fileStore{path: path, log: log, sessions: make(map[string]OpenSession)}

	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		// fresh install — empty store
	case err != nil:
		log.Error("session store unreadable; starting empty", "path", path, "err", err)
	default:
		var recs []OpenSession
		if err := json.Unmarshal(b, &recs); err != nil {
			log.Error("session store corrupt; starting empty", "path", path, "err", err)
		} else {
			for _, r := range recs {
				fs.sessions[key(r.CPID, r.ConnectorID)] = r
			}
			log.Info("session store loaded", "path", path, "open_sessions", len(recs))
		}
	}
	return fs, nil
}

func (fs *fileStore) Put(s OpenSession) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.sessions[key(s.CPID, s.ConnectorID)] = s
	return fs.flushLocked()
}

func (fs *fileStore) UpdateMeter(cpid string, connectorID, lastMeterWh int, at time.Time) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	k := key(cpid, connectorID)
	r, ok := fs.sessions[k]
	if !ok {
		return nil
	}
	r.LastMeterWh = lastMeterWh
	r.LastMeterAt = at
	fs.sessions[k] = r
	return fs.flushLocked()
}

func (fs *fileStore) Delete(cpid string, connectorID int) error {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	k := key(cpid, connectorID)
	if _, ok := fs.sessions[k]; !ok {
		return nil
	}
	delete(fs.sessions, k)
	return fs.flushLocked()
}

func (fs *fileStore) LoadOpen(cpid string) ([]OpenSession, error) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	out := make([]OpenSession, 0, 2)
	for _, r := range fs.sessions {
		if r.CPID == cpid {
			out = append(out, r)
		}
	}
	return out, nil
}

func (fs *fileStore) flushLocked() error {
	recs := make([]OpenSession, 0, len(fs.sessions))
	for _, r := range fs.sessions {
		recs = append(recs, r)
	}
	b, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(fs.path, b)
}

// atomicWrite writes b to path crash-safely: a temp file IN THE SAME DIRECTORY
// (so the rename is an atomic pointer-flip, not a cross-volume copy) is synced
// to disk and then renamed over the destination. On modern Windows/Go, os.Rename
// maps to MoveFileEx(MOVEFILE_REPLACE_EXISTING) and atomically replaces an
// existing file, so no pre-delete is required.
func atomicWrite(path string, b []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".sessions-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename

	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
