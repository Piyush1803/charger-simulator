package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/tobor/charger-simulator/internal/config"
)

// The last-applied charger config is persisted next to the session store so the
// worker can auto-relaunch the charger after a process restart — which lets it
// send a StopTransaction for any session the restart interrupted. Persisted on
// /api/configure, cleared on /api/disconnect. NOT cleared on graceful shutdown,
// so an unattended crash/redeploy comes back on its own.

func (s *Server) configPath() string {
	if s.dataDir == "" {
		return ""
	}
	return filepath.Join(s.dataDir, "charger.json")
}

// persistConfig saves cfg so RestorePersistedCharger can relaunch it later.
// Best-effort: failures are logged, never fatal.
func (s *Server) persistConfig(cfg config.ChargerConfig) {
	path := s.configPath()
	if path == "" {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.log.Error("persist charger config: mkdir failed", "err", err)
		return
	}
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		s.log.Error("persist charger config: marshal failed", "err", err)
		return
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		s.log.Error("persist charger config: write failed", "path", path, "err", err)
	}
}

// clearPersistedConfig removes the saved config so the charger is NOT
// auto-relaunched on the next start (operator explicitly disconnected it).
func (s *Server) clearPersistedConfig() {
	path := s.configPath()
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		s.log.Error("clear persisted charger config failed", "err", err)
	}
}

func (s *Server) loadPersistedConfig() (config.ChargerConfig, bool) {
	path := s.configPath()
	if path == "" {
		return config.ChargerConfig{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			s.log.Error("read persisted charger config failed", "path", path, "err", err)
		}
		return config.ChargerConfig{}, false
	}
	var cfg config.ChargerConfig
	if err := json.Unmarshal(b, &cfg); err != nil {
		s.log.Error("persisted charger config corrupt; ignoring", "path", path, "err", err)
		return config.ChargerConfig{}, false
	}
	return cfg, true
}

// RestorePersistedCharger relaunches the charger saved before the last
// shutdown/crash, if any. Called once at worker startup, before serving. The
// fresh charger boots and, via recoverInterruptedSessions, sends a
// StopTransaction for any session the restart interrupted.
func (s *Server) RestorePersistedCharger() {
	cfg, ok := s.loadPersistedConfig()
	if !ok {
		return
	}
	s.log.Info("restoring persisted charger", "cp_id", cfg.CPID, "cms_url", cfg.CMSURL)
	s.startCharger(cfg)
}
