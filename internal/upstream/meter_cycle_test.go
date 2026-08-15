package upstream

import "testing"

// meter field semantics verified live 2026-08-14: for CapacityType=4
// (Free Plan Subscription) CapacityRemain is STATIC (always 100) while
// CycleCapacityRemain holds the real remaining quota. A plan exhausted at
// the cycle level (CycleCapacityRemain=0) must NOT fall back to
// CapacityRemain (100) — that's the "cb keys failing" 14018 bug.
func TestMeterRemainCyclePreferred(t *testing.T) {
	cases := []struct {
		name    string
		acc     cbMeterAccount
		wantRem float64
		wantUse float64
		wantSz  float64
	}{
		{
			// ck_ftxxj — 14018 exhausted: Free Plan CapacityRemain=100
			// (static) but CycleCapacityRemain=0 → must resolve to 0.
			name: "free-plan-cycle-exhausted",
			acc: cbMeterAccount{
				PackageName:  "Free Plan Subscription",
				CapacitySize: 100, CapacityUsed: 0, CapacityRemain: 100,
				CapacitySizePrecise: "100", CapacityUsedPrecise: "0", CapacityRemainPrecise: "100",
				CycleCapacitySize: 100, CycleCapacityUsed: 100, CycleCapacityRemain: 0,
				CycleCapacitySizePrecise: "100", CycleCapacityUsedPrecise: "100", CycleCapacityRemainPrecise: "0",
				Status: 0,
			},
			wantRem: 0, wantUse: 100, wantSz: 100,
		},
		{
			// ck_fty5n8 — LIVE: cycle remain 98.91
			name: "free-plan-cycle-remaining",
			acc: cbMeterAccount{
				PackageName:  "Free Plan Subscription",
				CapacitySize: 100, CapacityUsed: 0, CapacityRemain: 100,
				CapacitySizePrecise: "100", CapacityUsedPrecise: "0", CapacityRemainPrecise: "100",
				CycleCapacitySize: 100, CycleCapacityUsed: 1, CycleCapacityRemain: 98,
				CycleCapacitySizePrecise: "100", CycleCapacityUsedPrecise: "1.08999998", CycleCapacityRemainPrecise: "98.91000002",
				Status: 0,
			},
			wantRem: 98.91000002, wantUse: 1.08999998, wantSz: 100,
		},
		{
			// ck_ftxx152 — LIVE bonus pack, both field sets agree.
			name: "bonus-pack-agree",
			acc: cbMeterAccount{
				PackageName:  "Bonus Pack",
				CapacitySize: 250, CapacityUsed: 26, CapacityRemain: 223,
				CapacitySizePrecise: "250", CapacityUsedPrecise: "26.96", CapacityRemainPrecise: "223.04",
				CycleCapacitySize: 250, CycleCapacityUsed: 26, CycleCapacityRemain: 223,
				CycleCapacitySizePrecise: "250", CycleCapacityUsedPrecise: "26.96", CycleCapacityRemainPrecise: "223.04",
				Status: 0,
			},
			wantRem: 223.04, wantUse: 26.96, wantSz: 250,
		},
		{
			// Cycle fields absent entirely (old API shape) → legacy fallback.
			name: "cycle-absent-fallback",
			acc: cbMeterAccount{
				PackageName:  "Legacy Plan",
				CapacitySize: 50, CapacityUsed: 10, CapacityRemain: 40,
				CapacitySizePrecise: "50", CapacityUsedPrecise: "10", CapacityRemainPrecise: "40",
				Status: 0,
			},
			wantRem: 40, wantUse: 10, wantSz: 50,
		},
		{
			// Cycle remain present but zero with empty precise → 0, no fallback.
			name: "cycle-zero-int-no-fallback",
			acc: cbMeterAccount{
				PackageName:  "Free Plan Subscription",
				CapacitySize: 100, CapacityUsed: 0, CapacityRemain: 100,
				CapacitySizePrecise: "100", CapacityUsedPrecise: "0", CapacityRemainPrecise: "100",
				CycleCapacitySize: 100, CycleCapacityUsed: 100, CycleCapacityRemain: 0,
				CycleCapacitySizePrecise: "100", CycleCapacityUsedPrecise: "100", CycleCapacityRemainPrecise: "",
				Status: 0,
			},
			wantRem: 0, wantUse: 100, wantSz: 100,
		},
	}

	for _, c := range cases {
		gotRem := meterRemain(c.acc)
		gotUse := meterUsed(c.acc)
		gotSz := meterSize(c.acc)
		if gotRem != c.wantRem {
			t.Errorf("%s: meterRemain = %v, want %v", c.name, gotRem, c.wantRem)
		}
		if gotUse != c.wantUse {
			t.Errorf("%s: meterUsed = %v, want %v", c.name, gotUse, c.wantUse)
		}
		if gotSz != c.wantSz {
			t.Errorf("%s: meterSize = %v, want %v", c.name, gotSz, c.wantSz)
		}
	}
}

// applyMeterAccounts must aggregate BOTH plans and only disable when TOTAL
// remain <= 0 — an exhausted Bonus Pack with a live Free Plan must stay live.
func TestApplyMeterAccountsAggregatesPlans(t *testing.T) {
	// ck_ftxxj shape: Bonus exhausted (0 remain) + Free Plan cycle-exhausted
	// (0 remain) → total 0 → disabled.
	k := &CBKey{}
	k.applyMeterAccounts([]cbMeterAccount{
		{PackageName: "Bonus Pack", CapacitySize: 250, CapacityUsed: 250, CapacityRemain: 0,
			CapacitySizePrecise: "250", CapacityUsedPrecise: "250", CapacityRemainPrecise: "0",
			CycleCapacitySize: 250, CycleCapacityUsed: 250, CycleCapacityRemain: 0,
			CycleCapacitySizePrecise: "250", CycleCapacityUsedPrecise: "250", CycleCapacityRemainPrecise: "0",
			Status: 3},
		{PackageName: "Free Plan Subscription", CapacitySize: 100, CapacityUsed: 0, CapacityRemain: 100,
			CapacitySizePrecise: "100", CapacityUsedPrecise: "0", CapacityRemainPrecise: "100",
			CycleCapacitySize: 100, CycleCapacityUsed: 100, CycleCapacityRemain: 0,
			CycleCapacitySizePrecise: "100", CycleCapacityUsedPrecise: "100", CycleCapacityRemainPrecise: "0",
			Status: 0},
	})
	k.mu.RLock()
	disabled, remain := k.disabled, k.creditsRemain
	k.mu.RUnlock()
	if !disabled {
		t.Errorf("both plans exhausted: expected disabled, got remain=%v", remain)
	}

	// ck_fty5n8 shape: Bonus exhausted + Free Plan 98 remain → total 98 → live.
	k2 := &CBKey{}
	k2.applyMeterAccounts([]cbMeterAccount{
		{PackageName: "Bonus Pack", CapacitySize: 250, CapacityUsed: 250, CapacityRemain: 0,
			CapacitySizePrecise: "250", CapacityUsedPrecise: "250", CapacityRemainPrecise: "0",
			CycleCapacitySize: 250, CycleCapacityUsed: 250, CycleCapacityRemain: 0,
			CycleCapacitySizePrecise: "250", CycleCapacityUsedPrecise: "250", CycleCapacityRemainPrecise: "0",
			Status: 3},
		{PackageName: "Free Plan Subscription", CapacitySize: 100, CapacityUsed: 0, CapacityRemain: 100,
			CapacitySizePrecise: "100", CapacityUsedPrecise: "0", CapacityRemainPrecise: "100",
			CycleCapacitySize: 100, CycleCapacityUsed: 1, CycleCapacityRemain: 98,
			CycleCapacitySizePrecise: "100", CycleCapacityUsedPrecise: "1.08999998", CycleCapacityRemainPrecise: "98.91000002",
			Status: 0},
	})
	k2.mu.RLock()
	disabled2, remain2 := k2.disabled, k2.creditsRemain
	k2.mu.RUnlock()
	if disabled2 {
		t.Errorf("free plan has 98 remain: expected NOT disabled")
	}
	if remain2 != 98.91000002 {
		t.Errorf("remain = %v, want 98.91000002", remain2)
	}
}
