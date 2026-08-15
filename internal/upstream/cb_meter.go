package upstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (k *CBKey) AddCredits(c float64) {
	k.mu.Lock()
	k.creditsUsed += c
	k.totalReqs++
	// Prefer meter remain when available: if remain was set and used climbs
	// above limit, disable. creditLimit==0 → fallback constant.
	limit := k.creditLimit
	if limit <= 0 {
		limit = CB_CREDIT_LIMIT
	}
	// If meter remain is known, also update local remain estimate.
	if k.meterSyncedAt.After(time.Time{}) && k.creditLimit > 0 {
		k.creditsRemain = k.creditLimit - k.creditsUsed
		if k.creditsRemain < 0 {
			k.creditsRemain = 0
		}
	}
	if k.creditsUsed >= limit {
		k.disabled = true
		k.disabledAt = time.Time{} // permanent until reset
		// I1: local-credit exhaustion is meter-driven — auto-lift when a
		// later meter sync reports credits.
		k.disabledReason = cbMeterDisableReason
		slog.Warn("key disabled (credits used)",
			"module", "cb",
			"key", k.displayIDLocked(),
			"credits_used", k.creditsUsed,
			"credit_limit", limit)
	}
	k.mu.Unlock()
	if k.db != nil {
		saveCBKey(k.db, k.toDTO())
	}
}

// SyncCredits fetches live credit usage from the CodeBuddy meter API and
// updates local state. Works for both API keys (ck_*) and OAuth access tokens
// via Authorization: Bearer. Concurrent calls for the same key are collapsed
// via singleflight. Never holds mu during the network round-trip.
//
// On Status==3 (exhausted) or CycleCapacityRemainPrecise<=0 (cycle-accurate
// remain — CapacityRemain is the static subscription size and stays 100 for
// exhausted Free Plans) the key is permanently
// disabled. Permanent disables are never auto-reenabled even if the meter later
// reports remain>0 (operator must re-import / re-enable manually).
func (k *CBKey) SyncCredits() error {
	_, err, _ := k.syncSF.Do(k.Key, func() (any, error) {
		return nil, k.syncCreditsLocked()
	})
	return err
}

func (k *CBKey) syncCreditsLocked() error {
	// OAuth: ensure access token is fresh before the meter call.
	if k.GetCredType() == CBAuthOAuth {
		if err := k.EnsureValid(); err != nil {
			return fmt.Errorf("cb credit sync ensure valid: %w", err)
		}
	}

	auth := k.AuthHeader()
	display := k.DisplayID()

	req, err := http.NewRequest("POST", CB_CREDIT_METER_URL, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", auth)

	client, proxyID := getClient(tokenRefreshClient, "codebuddy")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return fmt.Errorf("cb credit sync: %w", err)
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("cb credit sync [%d]: %s", resp.StatusCode, truncateLog(string(body), 200))
	}

	accounts, err := parseCBMeterAccounts(body)
	if err != nil {
		return err
	}

	// applyMeterAccounts takes the lock itself — no outer Lock/Unlock here.
	sum, disabledNow, reenabledNow := k.applyMeterAccounts(accounts)

	if k.db != nil {
		saveCBKey(k.db, k.toDTO())
	}
	if disabledNow {
		slog.Warn("key disabled (meter exhausted)",
			"module", "cb-meter",
			"key", display,
			"remain", sum.remain,
			"status", sum.status,
			"package", sum.primary)
	}
	if reenabledNow {
		slog.Info("key re-enabled (meter has credits)",
			"module", "cb-meter",
			"key", display,
			"remain", sum.remain,
			"package", sum.primary)
	}
	slog.Debug("credit sync ok",
		"module", "cb-meter",
		"key", display,
		"used", sum.used,
		"remain", sum.remain,
		"limit", sum.size,
		"status", sum.status)
	return nil
}

// meterSummary is the aggregated result of applyMeterAccounts.
type meterSummary struct {
	size    float64
	used    float64
	remain  float64
	status  int
	primary string
}

// applyMeterAccounts aggregates ALL meter plans (a key can hold Bonus Pack +
// Free Plan Subscription concurrently) and writes the result under k.mu.
// Returns the summary, whether this call newly made the key permanently
// disabled, and whether it lifted a previous permanent disable.
// Sums size/used/remain across plans; keeps the plan with the most remain as
// the display package; status = 3 only when EVERY plan is exhausted (an
// exhausted Bonus Pack must NOT kill a key that still has Free Plan credits
// left — disable only when TOTAL remain <= 0).
//
// Re-enable semantics: a meter sync that reports credits (remain > 0 and not
// all-exhausted) LIFTS a permanent disable. This is safe because SyncCredits
// fails earlier for auth-dead keys (401 / revoked refresh token hit the same
// Authorization header before this code runs), so only keys whose meter call
// succeeds get re-enabled. Truly exhausted keys still report remain <= 0 and
// stay disabled.
// cbMeterDisableReason marks disables caused by the meter (credit) sync
// worker. Operator-initiated disables (DisableKey) carry arbitrary reasons
// and must NEVER be auto-re-enabled by the worker.
const cbMeterDisableReason = "meter exhausted"

// cycleEndAfter reports whether a > b as RFC3339 timestamps, falling back to
// lexicographic comparison when either string fails to parse.
func cycleEndAfter(a, b string) bool {
	ta, ea := time.Parse(time.RFC3339, a)
	tb, eb := time.Parse(time.RFC3339, b)
	if ea == nil && eb == nil {
		return ta.After(tb)
	}
	return a > b
}

func (k *CBKey) applyMeterAccounts(accounts []cbMeterAccount) (meterSummary, bool, bool) {
	// H1: empty/partial meter payload must not panic or touch state.
	if len(accounts) == 0 {
		return meterSummary{}, false, false
	}
	var size, used, remain float64
	status := 0
	primary := accounts[0].PackageName
	maxRemain := -1.0
	cycleEnd := ""
	allExhausted := len(accounts) > 0
	for _, acc := range accounts {
		aSize := meterSize(acc)
		aUsed := meterUsed(acc)
		aRemain := meterRemain(acc)
		size += aSize
		used += aUsed
		remain += aRemain
		if acc.Status != 3 {
			allExhausted = false
		}
		if aRemain > maxRemain {
			maxRemain = aRemain
			primary = acc.PackageName
		}
		if cycleEndAfter(acc.CycleEndTime, cycleEnd) {
			cycleEnd = acc.CycleEndTime
		}
	}
	if allExhausted {
		status = 3
	}

	k.mu.Lock()
	defer k.mu.Unlock()
	// M4: update the three credit fields atomically — a zero-size payload
	// (schema drift / partial response) must not overwrite good data with 0
	// nor leave a stale limit paired with a fresh remain.
	if size > 0 {
		k.creditsUsed = used
		k.creditLimit = size
		k.creditsRemain = remain
	}
	k.packageName = primary
	k.cycleEnd = cycleEnd
	k.meterStatus = status
	k.meterSyncedAt = time.Now()

	disabledNow := false
	reenabledNow := false
	// H2: only Status 3 is authoritative on its own. A zero-size payload must
	// not be read as exhaustion — require evidence (size > 0) for remain <= 0.
	exhausted := status == 3 || (size > 0 && remain <= 0)
	if exhausted {
		// Either not disabled, or only on cooldown — make permanent.
		if !k.disabled || !k.disabledAt.IsZero() {
			k.disabled = true
			k.disabledAt = time.Time{}
			k.disabledReason = cbMeterDisableReason // L3: explain the disable
			disabledNow = true
		}
	} else {
		// H3: meter says credits remain — lift a disable ONLY when it came
		// from the meter path (or is a cooldown with a non-zero timestamp).
		// Operator disables (arbitrary reason, zero timestamp) fail closed.
		if k.disabled && (k.disabledReason == cbMeterDisableReason || !k.disabledAt.IsZero()) {
			k.disabled = false
			k.disabledAt = time.Time{}
			k.disabledReason = ""
			reenabledNow = true
		}
	}
	return meterSummary{size: size, used: used, remain: remain, status: status, primary: primary}, disabledNow, reenabledNow
}

// cbMeterAccount is one entry from the meter API Accounts[] list. A key can
// hold MULTIPLE plans concurrently (e.g. Bonus Pack trial + Free Plan
// Subscription) — the gateway aggregates all of them.
//
// IMPORTANT field semantics (verified live 2026-08-14): CapacityRemain is
// the STATIC subscription capacity for CapacityType=4 (Free Plan) — it
// reports 100 even when the cycle quota is fully consumed. CycleCapacityRemain
// is the real remaining quota for the current cycle and is the value that
// drives 14018 "credits exhausted" upstream. Cycle* fields must be preferred.
type cbMeterAccount struct {
	PackageName                string `json:"PackageName"`
	CapacitySize               int    `json:"CapacitySize"`
	CapacityUsed               int    `json:"CapacityUsed"`
	CapacityRemain             int    `json:"CapacityRemain"`
	CapacitySizePrecise        string `json:"CapacitySizePrecise"`
	CapacityUsedPrecise        string `json:"CapacityUsedPrecise"`
	CapacityRemainPrecise      string `json:"CapacityRemainPrecise"`
	CycleCapacitySize          int    `json:"CycleCapacitySize"`
	CycleCapacityUsed          int    `json:"CycleCapacityUsed"`
	CycleCapacityRemain        int    `json:"CycleCapacityRemain"`
	CycleCapacitySizePrecise   string `json:"CycleCapacitySizePrecise"`
	CycleCapacityUsedPrecise   string `json:"CycleCapacityUsedPrecise"`
	CycleCapacityRemainPrecise string `json:"CycleCapacityRemainPrecise"`
	CycleStartTime             string `json:"CycleStartTime"`
	CycleEndTime               string `json:"CycleEndTime"`
	Status                     int    `json:"Status"`
}

// meterCyclePresent reports whether the API response carried CycleCapacity*
// fields. CycleCapacitySize is the reliable presence probe: real responses
// always carry size > 0 (100/250), while older/partial responses leave it 0.
// Distinguishing "cycle absent" from "cycle exhausted" is impossible from
// remain alone (both give 0) — size is the tiebreaker.
func meterCyclePresent(acc cbMeterAccount) bool {
	if acc.CycleCapacitySize != 0 {
		return true
	}
	// L5: a literal "0" string is NOT evidence of a populated Cycle block —
	// treat it as absent and fall back to the legacy Capacity* fields.
	s := strings.TrimSpace(acc.CycleCapacitySizePrecise)
	if s == "" {
		return false
	}
	if v, err := strconv.ParseFloat(s, 64); err == nil && v == 0 {
		return false
	}
	return true
}

// meterRemain returns the cycle-accurate remaining credits. When Cycle* is
// present (the normal case), CycleCapacityRemain is authoritative and a 0
// means genuinely exhausted — it must NOT fall back to CapacityRemain, which
// stays 100 for exhausted Free Plan accounts. Falls back to legacy Capacity*
// only when the response carries no Cycle fields at all.
func meterRemain(acc cbMeterAccount) float64 {
	if meterCyclePresent(acc) {
		return parseFloatOr(acc.CycleCapacityRemainPrecise, float64(acc.CycleCapacityRemain))
	}
	return parseFloatOr(acc.CapacityRemainPrecise, float64(acc.CapacityRemain))
}

// meterUsed mirrors meterRemain for the used side.
func meterUsed(acc cbMeterAccount) float64 {
	if meterCyclePresent(acc) {
		return parseFloatOr(acc.CycleCapacityUsedPrecise, float64(acc.CycleCapacityUsed))
	}
	return parseFloatOr(acc.CapacityUsedPrecise, float64(acc.CapacityUsed))
}

// meterSize mirrors meterRemain for the plan size.
func meterSize(acc cbMeterAccount) float64 {
	if meterCyclePresent(acc) {
		return parseFloatOr(acc.CycleCapacitySizePrecise, float64(acc.CycleCapacitySize))
	}
	return parseFloatOr(acc.CapacitySizePrecise, float64(acc.CapacitySize))
}

// parseCBMeterAccounts extracts the full Accounts[] list from the nested meter
// response. Shape: {"code":0,"data":{"Response":{"Data":{"Accounts":[...]}}}}
func parseCBMeterAccounts(body []byte) ([]cbMeterAccount, error) {
	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Response struct {
				Data struct {
					Accounts []cbMeterAccount `json:"Accounts"`
				} `json:"Data"`
			} `json:"Response"`
		} `json:"data"`
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("cb credit sync parse: %w", err)
	}
	if envelope.Code != 0 {
		return nil, fmt.Errorf("cb credit sync code=%d msg=%s", envelope.Code, envelope.Msg)
	}
	if len(envelope.Data.Response.Data.Accounts) == 0 {
		return nil, fmt.Errorf("cb credit sync: empty Accounts")
	}
	return envelope.Data.Response.Data.Accounts, nil
}

// parseFloatOr parses s as float64; on failure returns fallback.
func parseFloatOr(s string, fallback float64) float64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fallback
	}
	return v
}
