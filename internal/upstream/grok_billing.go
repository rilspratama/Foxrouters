package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

var billingSF singleflight.Group

type grokBillingResponse struct {
	Config struct {
		CurrentPeriod struct {
			Type  string `json:"type"`
			Start string `json:"start"`
			End   string `json:"end"`
		} `json:"currentPeriod"`
		OnDemandCap      *grokCent `json:"onDemandCap"`
		OnDemandUsed     *grokCent `json:"onDemandUsed"`
		PrepaidBalance   *grokCent `json:"prepaidBalance"`
		IsUnifiedBilling bool      `json:"isUnifiedBillingUser"`
		BillingPeriodEnd string    `json:"billingPeriodEnd"`
	} `json:"config"`
}

// grokCent is the { "val": <cents> } wrapper used by the billing API.
type grokCent struct {
	Val int64 `json:"val"`
}

// SyncBilling fetches the current billing/usage state from cli-chat-proxy
// and persists it to the account + Redis. Singleflight per email.
func (a *GrokAccount) SyncBilling() error {
	_, err, _ := billingSF.Do(a.Email, func() (any, error) {
		return nil, a.syncBillingLocked()
	})
	return err
}

func (a *GrokAccount) syncBillingLocked() error {
	// Ensure token is fresh before the billing call.
	if err := a.EnsureValid(); err != nil {
		return fmt.Errorf("grok billing ensure valid: %w", err)
	}

	token := a.GetAccessToken()

	req, err := http.NewRequest("GET", GROK_BILLING_URL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-XAI-Token-Auth", "xai-grok-cli")
	req.Header.Set("x-authenticateresponse", "authenticate-response")
	req.Header.Set("x-grok-client-version", GROK_CLIENT_VERSION)
	req.Header.Set("x-grok-client-identifier", GROK_CLIENT_IDENTIFIER)
	req.Header.Set("x-grok-client-mode", "tui")
	req.Header.Set("User-Agent", fmt.Sprintf("grok-shell/%s (linux; x86_64)", GROK_CLIENT_VERSION))
	if a.Sub != "" {
		req.Header.Set("x-userid", a.Sub)
	}
	if a.Email != "" {
		req.Header.Set("x-email", a.Email)
	}

	client, proxyID := getClient(upstreamClient, "grok")
	resp, err := client.Do(req)
	if err != nil {
		markProxyResult(proxyID, err, 0)
		return fmt.Errorf("grok billing: %w", err)
	}
	markProxyResult(proxyID, nil, resp.StatusCode)
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fmt.Errorf("grok billing [%d]: %s", resp.StatusCode, truncateLog(string(body), 200))
	}

	var billing grokBillingResponse
	if err := json.Unmarshal(body, &billing); err != nil {
		return fmt.Errorf("grok billing parse: %w", err)
	}

	var cap, used, prepaid int64
	if billing.Config.OnDemandCap != nil {
		cap = billing.Config.OnDemandCap.Val
	}
	if billing.Config.OnDemandUsed != nil {
		used = billing.Config.OnDemandUsed.Val
	}
	if billing.Config.PrepaidBalance != nil {
		prepaid = billing.Config.PrepaidBalance.Val
	}

	// Detect weekly period rollover — if periodEnd changed, reset usage counters
	newPeriodEnd := billing.Config.CurrentPeriod.End
	a.mu.Lock()
	periodChanged := newPeriodEnd != "" && a.periodEnd != "" && newPeriodEnd != a.periodEnd
	a.billingSyncedAt = time.Now()
	a.periodStart = billing.Config.CurrentPeriod.Start
	a.periodEnd = newPeriodEnd
	a.periodType = billing.Config.CurrentPeriod.Type
	a.onDemandCap = cap
	a.onDemandUsed = used
	a.prepaidBalance = prepaid
	a.unifiedBilling = billing.Config.IsUnifiedBilling
	if periodChanged {
		a.tokensUsed = 0
		a.promptTokens = 0
		a.completionTokens = 0
		a.usageResetAt = time.Now()
	}
	a.mu.Unlock()

	if periodChanged {
		slog.Info("grok billing period rolled over, usage reset",
			"module", "grok-billing",
			"email", a.Email,
			"old_period_end", a.periodEnd,
			"new_period_end", newPeriodEnd)
	}

	if a.db != nil {
		saveGrokAccount(a.db, a.toDTO())
	}
	slog.Debug("grok billing sync ok",
		"module", "grok-billing",
		"email", a.Email,
		"period_end", a.periodEnd,
		"on_demand_cap", cap,
		"on_demand_used", used,
		"prepaid", prepaid,
		"unified", billing.Config.IsUnifiedBilling)
	return nil
}

// GROK_FREE_TIER_QUOTA is the free-tier weekly token quota (1M as of Aug 2026,
// down from 2M). Used for dashboard display only — enforcement is server-side.
const GROK_FREE_TIER_QUOTA = 1_000_000

// RecordUsage accumulates per-account token usage from a completed request's
// usage fields. Called after ProxyGrok finishes reading the upstream response.
// Non-blocking: persists to Redis best-effort. Does NOT return an error —
// usage tracking is telemetry, not critical path.
func (a *GrokAccount) RecordUsage(promptTokens, completionTokens int) {
	if promptTokens <= 0 && completionTokens <= 0 {
		return
	}
	total := int64(promptTokens + completionTokens)
	a.mu.Lock()
	a.promptTokens += int64(promptTokens)
	a.completionTokens += int64(completionTokens)
	a.tokensUsed += total
	a.mu.Unlock()

	// Persist best-effort (fire-and-forget — don't block the response path).
	if a.db != nil {
		go saveGrokAccount(a.db, a.toDTO())
	}
}

// ResetUsage zeroes the per-account usage counters. Called by SyncBilling
// when it detects a new billing period (periodEnd changed).
func (a *GrokAccount) ResetUsage(resetAt time.Time) {
	a.mu.Lock()
	a.tokensUsed = 0
	a.promptTokens = 0
	a.completionTokens = 0
	a.usageResetAt = resetAt
	a.mu.Unlock()
	if a.db != nil {
		go saveGrokAccount(a.db, a.toDTO())
	}
}

// GrokBillingSyncWorker periodically syncs billing data for all non-banned
// Grok accounts. Mirrors CBCreditSyncWorker. Pass a context cancelled on
// SIGTERM for clean shutdown.
func GrokBillingSyncWorker(ctx context.Context, am *GrokAccountManager) {
	syncAllGrokBilling(am, true)

	ticker := time.NewTicker(GROK_BILLING_SYNC_TICK)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			syncAllGrokBilling(am, false)
		}
	}
}

func syncAllGrokBilling(am *GrokAccountManager, stagger bool) {
	accounts := am.GetAll()
	var wg sync.WaitGroup
	sem := make(chan struct{}, GROK_BILLING_SYNC_CONCURRENCY)
	idx := 0
	for _, a := range accounts {
		a.mu.RLock()
		perm := a.disabled && a.disabledAt.IsZero() // banned — skip
		a.mu.RUnlock()
		if perm {
			continue
		}

		if stagger && idx > 0 {
			time.Sleep(200 * time.Millisecond)
		}
		idx++

		wg.Add(1)
		sem <- struct{}{}
		go func(acc *GrokAccount) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := acc.SyncBilling(); err != nil {
				slog.Warn("grok billing sync error",
					"module", "grok-billing",
					"email", acc.Email,
					"error", err)
			}
		}(a)
	}
	wg.Wait()
}
