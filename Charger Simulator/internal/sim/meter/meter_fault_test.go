package meter

import (
	"testing"
	"time"

	"github.com/tobor/charger-simulator/internal/ocpp/j16"
)

// healthyAC builds a representative single-phase AC MeterValue: energy register,
// power, voltage (L1-N), current (L1).
func healthyAC() j16.MeterValue {
	return Snapshot{
		At:       time.Unix(1700000000, 0).UTC(),
		Type:     ModeAC,
		EnergyWh: 1234,
		PowerW:   7000,
		VoltageV: 230,
		CurrentA: 30.4,
		Phases:   1,
		PFactor:  1.0,
	}.ToOCPP()
}

// present reports which measurands appear in mv, with their reported value.
func present(mv j16.MeterValue) map[j16.Measurand]string {
	out := make(map[j16.Measurand]string, len(mv.SampledValue))
	for _, sv := range mv.SampledValue {
		out[sv.Measurand] = sv.Value
	}
	return out
}

func TestApplyFault(t *testing.T) {
	const (
		energy  = j16.MeasurandEnergyActiveImportRegister
		power   = j16.MeasurandPowerActiveImport
		voltage = j16.MeasurandVoltage
		current = j16.MeasurandCurrentImport
	)

	t.Run("none reports everything and sends", func(t *testing.T) {
		mv, send := ApplyFault(healthyAC(), FaultNone)
		if !send {
			t.Fatal("FaultNone must send")
		}
		m := present(mv)
		for _, want := range []j16.Measurand{energy, power, voltage, current} {
			if _, ok := m[want]; !ok {
				t.Errorf("FaultNone dropped %s", want)
			}
		}
	})

	t.Run("dead_silent suppresses the whole message", func(t *testing.T) {
		_, send := ApplyFault(healthyAC(), FaultDeadSilent)
		if send {
			t.Fatal("FaultDeadSilent must not send")
		}
	})

	t.Run("dead_empty sends an empty sampledValue", func(t *testing.T) {
		mv, send := ApplyFault(healthyAC(), FaultDeadEmpty)
		if !send {
			t.Fatal("FaultDeadEmpty must send")
		}
		if len(mv.SampledValue) != 0 {
			t.Errorf("want 0 samples, got %d", len(mv.SampledValue))
		}
	})

	t.Run("dead_zero zeroes V/I/P but keeps the energy register truthful", func(t *testing.T) {
		mv, send := ApplyFault(healthyAC(), FaultDeadZero)
		if !send {
			t.Fatal("FaultDeadZero must send")
		}
		m := present(mv)
		for _, z := range []j16.Measurand{power, voltage, current} {
			if m[z] != "0" {
				t.Errorf("%s = %q, want \"0\"", z, m[z])
			}
		}
		if m[energy] != "1234" {
			t.Errorf("energy register = %q, want %q (must stay truthful)", m[energy], "1234")
		}
	})

	t.Run("no_current keeps voltage, drops current", func(t *testing.T) {
		mv, send := ApplyFault(healthyAC(), FaultNoCurrent)
		if !send {
			t.Fatal("FaultNoCurrent must send")
		}
		m := present(mv)
		if _, ok := m[current]; ok {
			t.Error("no_current must drop Current.Import")
		}
		if _, ok := m[voltage]; !ok {
			t.Error("no_current must keep Voltage")
		}
		if _, ok := m[energy]; !ok {
			t.Error("no_current must keep the energy register")
		}
	})

	t.Run("no_vip drops V/I/P, keeps energy register", func(t *testing.T) {
		mv, send := ApplyFault(healthyAC(), FaultNoVIP)
		if !send {
			t.Fatal("FaultNoVIP must send")
		}
		m := present(mv)
		for _, gone := range []j16.Measurand{power, voltage, current} {
			if _, ok := m[gone]; ok {
				t.Errorf("no_vip must drop %s", gone)
			}
		}
		if _, ok := m[energy]; !ok {
			t.Error("no_vip must keep the energy register")
		}
	})

	t.Run("does not mutate the caller's snapshot", func(t *testing.T) {
		orig := healthyAC()
		before := len(orig.SampledValue)
		_, _ = ApplyFault(orig, FaultNoVIP)
		if len(orig.SampledValue) != before {
			t.Errorf("ApplyFault mutated input: had %d samples, now %d", before, len(orig.SampledValue))
		}
	})
}

func TestValidFault(t *testing.T) {
	valid := []string{"", "none", "dead_silent", "dead_empty", "dead_zero", "no_current", "no_vip"}
	for _, s := range valid {
		if !ValidFault(s) {
			t.Errorf("ValidFault(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"bogus", "No_Current", "dead"} {
		if ValidFault(s) {
			t.Errorf("ValidFault(%q) = true, want false", s)
		}
	}
}
