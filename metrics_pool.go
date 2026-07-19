// metrics_pool.go — pool-gauge updater that touches private manager fields.
// Metric definitions live in internal/metrics; this file just snapshots pool
// sizes into those gauges from the same package that owns the managers.
package main

import (
	"foxrouters/internal/metrics"
)

// updatePoolGauges snapshots pool sizes into active/disabled gauges.
// Called periodically from the pool managers' background workers so the
// gauges stay eventually-consistent without adding hot-path overhead.
func updatePoolGauges(grokAM *GrokAccountManager, cbKM *CBKeyManager, authMgr *AuthManager) {
	if grokAM != nil {
		grokAM.mu.RLock()
		var gActive, gDisabled int
		for _, a := range grokAM.accounts {
			if a.IsDisabled() {
				gDisabled++
			} else {
				gActive++
			}
		}
		grokAM.mu.RUnlock()
		metrics.SetPoolGauges("grok", gActive, gDisabled)
	}
	if cbKM != nil {
		cbKM.mu.RLock()
		var cActive, cDisabled int
		for _, k := range cbKM.keys {
			_, _, disabled := k.Stats()
			if disabled {
				cDisabled++
			} else {
				cActive++
			}
		}
		cbKM.mu.RUnlock()
		metrics.SetPoolGauges("codebuddy", cActive, cDisabled)
	}
	if authMgr != nil {
		var aActive, aDisabled int
		for _, info := range authMgr.GetAll() {
			if info.Disabled {
				aDisabled++
			} else {
				aActive++
			}
		}
		metrics.SetPoolGauges("auth", aActive, aDisabled)
	}
}

// setCircuitState is a thin wrapper that maps the local CircuitState enum
// to the int expected by internal/metrics.SetCircuitState.
func setCircuitState(upstream string, state CircuitState) {
	var v int
	switch state {
	case CircuitClosed:
		v = 0
	case CircuitOpen:
		v = 1
	case CircuitHalfOpen:
		v = 2
	}
	metrics.SetCircuitState(upstream, v)
}
