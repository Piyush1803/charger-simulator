// Package config holds the runtime configuration for a single virtual charger.
//
// Slice 4 expansion: ChargerType ("AC" or "DC"), NumConnectors (1 or 2), and
// the per-charger battery parameters used to simulate SoC on DC chargers.
package config

import "time"

// ChargerType is the physical-layer class of the charger.
type ChargerType string

const (
	ChargerTypeAC ChargerType = "AC"
	ChargerTypeDC ChargerType = "DC"
)

// ChargerConfig is the immutable per-charger configuration.
type ChargerConfig struct {
	// ---- Identity ----
	CPID            string
	Vendor          string
	Model           string
	FirmwareVersion string

	// ---- Transport ----
	CMSURL        string // full ws://host:port/ocpp/<cp-id>
	BasicAuthUser string
	BasicAuthPass string

	// ---- Timing ----
	HeartbeatInterval time.Duration
	MeterInterval     time.Duration

	// ---- Reconnect (unexpected CMS drop) ----
	// After an unexpected link loss the charger redials with exponential
	// backoff: the first wait is ReconnectInitialBackoff, doubling each failed
	// attempt up to ReconnectMaxBackoff. Zero values fall back to package
	// defaults (1s initial, 30s cap).
	ReconnectInitialBackoff time.Duration
	ReconnectMaxBackoff     time.Duration

	// ---- Topology ----
	// Type is "AC" or "DC". All connectors on a single charger share the
	// same type (mixed-mode chargers like AC+DC combo are out of scope for
	// slice 4 — they'd require per-connector type).
	Type ChargerType
	// NumConnectors is 1 or 2.
	NumConnectors int

	// ---- Universal electrical ----
	MaxPowerW       float64 // delivered power ceiling per connector
	NominalVoltageV float64 // AC line-to-line OR DC pack voltage

	// ---- AC-only fields (ignored when Type == DC) ----
	Phases      int // 1 or 3
	PowerFactor float64

	// ---- DC-only fields (ignored when Type == AC) ----
	// BatteryCapacityKWh is the capacity of the EV the simulator pretends is
	// plugged in. SoC = InitialSoC + delivered_kWh / BatteryCapacityKWh * 100.
	BatteryCapacityKWh float64
	// InitialSoC is the SoC% the simulated EV starts at when a session begins.
	// Clamped to [0, 100].
	InitialSoC float64
}

// IsDC reports whether this charger should be simulated as a DC unit.
func (c ChargerConfig) IsDC() bool { return c.Type == ChargerTypeDC }
