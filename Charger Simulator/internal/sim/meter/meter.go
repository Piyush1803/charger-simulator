// Package meter is the calculation engine that produces OCPP MeterValues for a
// virtual charger. It is pure (deterministic) given (config, time, session state).
//
// Slice 4 adds DC-mode simulation: the engine tracks an EV battery's State of
// Charge (SoC) from the energy delivered, applies a simple CC→taper curve
// above 80% SoC, and emits SoC + DC V/I MeterValues instead of per-phase AC.
package meter

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/tobor/charger-simulator/internal/ocpp/j16"
)

// Mode picks between AC and DC behaviour.
type Mode string

const (
	ModeAC Mode = "AC"
	ModeDC Mode = "DC"
)

// Engine holds per-connector config and the running session state.
//
// All state mutation happens inside Sample(); call sites must be on the actor
// goroutine — there is no internal locking.
type Engine struct {
	// ---- universal configuration (set once) ----
	Type        Mode    // ModeAC or ModeDC; defaults to AC
	MaxPowerW   float64 // delivered-power ceiling
	NominalV    float64 // AC line-line OR DC nominal pack voltage

	// ---- AC-only configuration ----
	Phases      int     // 1 or 3
	PowerFactor float64 // 0.95–1.0 typical
	PhaseSagOhm float64 // equivalent series resistance for V sag under load

	// ---- DC-only configuration ----
	// BatteryCapacityKWh is the EV's battery size. SoC ramps from InitialSoC
	// toward 100% as energy is delivered. 0 disables SoC simulation.
	BatteryCapacityKWh float64
	// InitialSoC is the SoC% the simulator pretends the EV started at when
	// the session began. Each Start() resets currentSoC back to this value.
	InitialSoC float64

	// ---- session state ----
	startedAt     time.Time
	startEnergyWh float64
	currentWh     float64
	currentSoC    float64
	lastSampleAt  time.Time
	lastSnap      Snapshot
}

// LastSnap returns the snapshot from the most recent Sample call. Zero-value
// snapshot if Sample has not been called yet. Useful when the actor wants to
// publish a state event without advancing the integrator.
func (e *Engine) LastSnap() Snapshot { return e.lastSnap }

// Snapshot is the instantaneous reading produced by Sample.
type Snapshot struct {
	At       time.Time
	Type     Mode
	EnergyWh float64
	PowerW   float64
	VoltageV float64
	CurrentA float64
	SoC      float64 // 0..100; only meaningful when Type == ModeDC
	Phases   int     // AC only
	PFactor  float64 // AC only
}

// Start (re)initialises the session. startEnergyWh is the cumulative register
// value at the start of the session (carried across sessions for monotonicity).
// For DC chargers, currentSoC also resets to InitialSoC.
func (e *Engine) Start(at time.Time, startEnergyWh float64) {
	e.startedAt = at
	e.startEnergyWh = startEnergyWh
	e.currentWh = startEnergyWh
	e.lastSampleAt = at
	if e.Type == ModeDC {
		soc := e.InitialSoC
		if soc < 0 {
			soc = 0
		}
		if soc > 100 {
			soc = 100
		}
		e.currentSoC = soc
	} else {
		e.currentSoC = 0
	}
}

// CurrentWh returns the latest energy register without advancing time.
// Useful for setting meterStart on StartTransaction before a session begins.
func (e *Engine) CurrentWh() float64 { return e.currentWh }

// CurrentSoC returns the latest SoC reading without advancing time. Returns
// 0 for AC engines.
func (e *Engine) CurrentSoC() float64 { return e.currentSoC }

// SetMaxPower changes the instantaneous power ceiling. Takes effect from the
// next Sample call.
func (e *Engine) SetMaxPower(w float64) {
	if w < 0 {
		w = 0
	}
	e.MaxPowerW = w
}

// Sample advances the integrator to time `at` and returns the current state.
// Energy is monotonically non-decreasing.
func (e *Engine) Sample(at time.Time) Snapshot {
	power := e.power(at)

	if !e.lastSampleAt.IsZero() {
		dt := at.Sub(e.lastSampleAt).Seconds()
		if dt > 0 {
			e.currentWh += power * dt / 3600.0 // W * s → Wh
			if e.Type == ModeDC && e.BatteryCapacityKWh > 0 {
				delivered := (e.currentWh - e.startEnergyWh) / 1000.0 // kWh delivered this session
				e.currentSoC = clampSoC(e.InitialSoC + delivered/e.BatteryCapacityKWh*100.0)
			}
		}
	}
	e.lastSampleAt = at

	var snap Snapshot
	if e.Type == ModeDC {
		snap = e.dcSnapshot(at, power)
	} else {
		snap = e.acSnapshot(at, power)
	}
	e.lastSnap = snap
	return snap
}

func (e *Engine) acSnapshot(at time.Time, power float64) Snapshot {
	phases := e.Phases
	if phases == 0 {
		phases = 1
	}
	pf := e.PowerFactor
	if pf == 0 {
		pf = 1.0
	}
	phaseFactor := 1.0
	if phases == 3 {
		phaseFactor = math.Sqrt(3)
	}
	current := 0.0
	if e.NominalV > 0 && pf > 0 {
		current = power / (e.NominalV * pf * phaseFactor)
	}
	voltage := e.NominalV - e.PhaseSagOhm*current
	return Snapshot{
		At:       at,
		Type:     ModeAC,
		EnergyWh: e.currentWh,
		PowerW:   power,
		VoltageV: voltage,
		CurrentA: current,
		Phases:   phases,
		PFactor:  pf,
	}
}

func (e *Engine) dcSnapshot(at time.Time, power float64) Snapshot {
	// DC pack voltage drifts upward slightly with SoC (very rough Li-ion model).
	// At 0% SoC ~80% of nominal; at 100% SoC ~105% of nominal.
	voltageFrac := 0.80 + 0.25*(e.currentSoC/100.0)
	voltage := e.NominalV * voltageFrac
	current := 0.0
	if voltage > 0 {
		current = power / voltage
	}
	return Snapshot{
		At:       at,
		Type:     ModeDC,
		EnergyWh: e.currentWh,
		PowerW:   power,
		VoltageV: voltage,
		CurrentA: current,
		SoC:      e.currentSoC,
	}
}

// power returns the requested charging power at time t, applying DC tapering
// above 80% SoC.
//
// Slice 4: constant power until 80% SoC, linear taper to 0 at 100%. For AC, no
// taper — just return MaxPowerW.
func (e *Engine) power(_ time.Time) float64 {
	if e.Type != ModeDC {
		return e.MaxPowerW
	}
	soc := e.currentSoC
	if soc < 80 {
		return e.MaxPowerW
	}
	if soc >= 100 {
		return 0
	}
	// linear taper between 80% (full) and 100% (zero)
	factor := (100 - soc) / 20.0
	return e.MaxPowerW * factor
}

func clampSoC(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// ToOCPP turns a snapshot into an OCPP MeterValue with one timestamped sample
// group. For AC: per-phase V/I + power + energy. For DC: a single V, I, energy,
// power, plus SoC.
func (s Snapshot) ToOCPP() j16.MeterValue {
	if s.Type == ModeDC {
		return s.toOCPPDC()
	}
	return s.toOCPPAC()
}

func (s Snapshot) toOCPPAC() j16.MeterValue {
	samples := []j16.SampledValue{
		{
			Value:     strconv.Itoa(int(s.EnergyWh)),
			Context:   j16.ReadingContextSamplePeriodic,
			Measurand: j16.MeasurandEnergyActiveImportRegister,
			Unit:      j16.UnitWh,
			Location:  j16.LocationOutlet,
		},
		{
			Value:     fmt.Sprintf("%.1f", s.PowerW),
			Context:   j16.ReadingContextSamplePeriodic,
			Measurand: j16.MeasurandPowerActiveImport,
			Unit:      j16.UnitW,
			Location:  j16.LocationOutlet,
		},
	}
	if s.Phases == 1 {
		samples = append(samples,
			j16.SampledValue{
				Value: fmt.Sprintf("%.1f", s.VoltageV), Context: j16.ReadingContextSamplePeriodic,
				Measurand: j16.MeasurandVoltage, Phase: j16.PhaseL1N, Unit: j16.UnitV, Location: j16.LocationOutlet,
			},
			j16.SampledValue{
				Value: fmt.Sprintf("%.2f", s.CurrentA), Context: j16.ReadingContextSamplePeriodic,
				Measurand: j16.MeasurandCurrentImport, Phase: j16.PhaseL1, Unit: j16.UnitA, Location: j16.LocationOutlet,
			},
		)
	} else {
		for _, p := range []j16.Phase{j16.PhaseL1, j16.PhaseL2, j16.PhaseL3} {
			samples = append(samples,
				j16.SampledValue{
					Value: fmt.Sprintf("%.1f", s.VoltageV), Context: j16.ReadingContextSamplePeriodic,
					Measurand: j16.MeasurandVoltage, Phase: p, Unit: j16.UnitV, Location: j16.LocationOutlet,
				},
				j16.SampledValue{
					Value: fmt.Sprintf("%.2f", s.CurrentA), Context: j16.ReadingContextSamplePeriodic,
					Measurand: j16.MeasurandCurrentImport, Phase: p, Unit: j16.UnitA, Location: j16.LocationOutlet,
				},
			)
		}
	}
	return j16.MeterValue{Timestamp: s.At, SampledValue: samples}
}

// ---------------------------------------------------------------------------
// Energy-meter fault injection
// ---------------------------------------------------------------------------

// Fault selects an energy-meter fault to inject into the MeterValues a
// connector reports. It is a pure *reporting* filter applied after the snapshot
// is built: the integrator keeps running underneath, so the energy register
// reflects true cumulative energy once the fault clears.
//
// These model a flaky internal MID/Modbus energy meter — the channel that
// feeds the charger's MeterValues — failing in the ways a CMS has to tolerate.
type Fault string

const (
	FaultNone       Fault = "none"        // healthy meter — report everything
	FaultDeadSilent Fault = "dead_silent" // send no MeterValues at all (telemetry blackout)
	FaultDeadEmpty  Fault = "dead_empty"  // send MeterValues with an empty sampledValue (schema-invalid)
	FaultDeadZero   Fault = "dead_zero"   // report every measurand but V/I/P read 0
	FaultNoCurrent  Fault = "no_current"  // keep voltage (+energy/power/SoC), drop Current.Import
	FaultNoVIP      Fault = "no_vip"      // drop Voltage, Current.Import, Power.Active.Import; keep energy + SoC
)

// ValidFault reports whether s names a known fault mode. The empty string is
// treated as FaultNone and is therefore valid.
func ValidFault(s string) bool {
	switch Fault(s) {
	case "", FaultNone, FaultDeadSilent, FaultDeadEmpty, FaultDeadZero, FaultNoCurrent, FaultNoVIP:
		return true
	}
	return false
}

// ApplyFault rewrites mv according to f and reports whether the MeterValues
// message should be sent at all (send=false means the actor should drop the
// whole CALL — a total telemetry blackout). mv is returned unmodified for
// FaultNone / unknown faults. The returned MeterValue never shares its
// SampledValue backing array with the input when it is altered, so the caller's
// snapshot is left intact.
func ApplyFault(mv j16.MeterValue, f Fault) (out j16.MeterValue, send bool) {
	switch f {
	case FaultDeadSilent:
		return mv, false
	case FaultDeadEmpty:
		mv.SampledValue = []j16.SampledValue{}
		return mv, true
	case FaultDeadZero:
		mv.SampledValue = zeroMeasurands(mv.SampledValue,
			j16.MeasurandVoltage, j16.MeasurandCurrentImport, j16.MeasurandPowerActiveImport)
		return mv, true
	case FaultNoCurrent:
		mv.SampledValue = dropMeasurands(mv.SampledValue, j16.MeasurandCurrentImport)
		return mv, true
	case FaultNoVIP:
		mv.SampledValue = dropMeasurands(mv.SampledValue,
			j16.MeasurandVoltage, j16.MeasurandCurrentImport, j16.MeasurandPowerActiveImport)
		return mv, true
	default: // FaultNone or any unknown value — report unchanged.
		return mv, true
	}
}

// dropMeasurands returns a fresh slice with every sample whose Measurand is in
// `drop` removed. Other samples are preserved in order.
func dropMeasurands(in []j16.SampledValue, drop ...j16.Measurand) []j16.SampledValue {
	out := make([]j16.SampledValue, 0, len(in))
	for _, sv := range in {
		if containsMeasurand(drop, sv.Measurand) {
			continue
		}
		out = append(out, sv)
	}
	return out
}

// zeroMeasurands returns a fresh slice in which every sample whose Measurand is
// in `zero` has its Value forced to "0". The cumulative energy register and SoC
// are left untouched — a dead instantaneous channel doesn't reset the register
// nor the EV's reported state of charge.
func zeroMeasurands(in []j16.SampledValue, zero ...j16.Measurand) []j16.SampledValue {
	out := make([]j16.SampledValue, len(in))
	copy(out, in)
	for i := range out {
		if containsMeasurand(zero, out[i].Measurand) {
			out[i].Value = "0"
		}
	}
	return out
}

func containsMeasurand(set []j16.Measurand, m j16.Measurand) bool {
	for _, x := range set {
		if x == m {
			return true
		}
	}
	return false
}

func (s Snapshot) toOCPPDC() j16.MeterValue {
	// DC: no Phase field on the V/I samples — the EVSE delivers a single rail.
	samples := []j16.SampledValue{
		{
			Value:     strconv.Itoa(int(s.EnergyWh)),
			Context:   j16.ReadingContextSamplePeriodic,
			Measurand: j16.MeasurandEnergyActiveImportRegister,
			Unit:      j16.UnitWh,
			Location:  j16.LocationOutlet,
		},
		{
			Value:     fmt.Sprintf("%.1f", s.PowerW),
			Context:   j16.ReadingContextSamplePeriodic,
			Measurand: j16.MeasurandPowerActiveImport,
			Unit:      j16.UnitW,
			Location:  j16.LocationOutlet,
		},
		{
			Value:     fmt.Sprintf("%.1f", s.VoltageV),
			Context:   j16.ReadingContextSamplePeriodic,
			Measurand: j16.MeasurandVoltage,
			Unit:      j16.UnitV,
			Location:  j16.LocationOutlet,
		},
		{
			Value:     fmt.Sprintf("%.2f", s.CurrentA),
			Context:   j16.ReadingContextSamplePeriodic,
			Measurand: j16.MeasurandCurrentImport,
			Unit:      j16.UnitA,
			Location:  j16.LocationOutlet,
		},
		{
			Value:     fmt.Sprintf("%.1f", s.SoC),
			Context:   j16.ReadingContextSamplePeriodic,
			Measurand: j16.MeasurandSoC,
			Unit:      j16.UnitPercent,
			Location:  j16.LocationEV,
		},
	}
	return j16.MeterValue{Timestamp: s.At, SampledValue: samples}
}
