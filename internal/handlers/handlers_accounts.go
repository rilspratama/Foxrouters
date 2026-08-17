package handlers

import (
	"crypto/hmac"
	crypto_rand "crypto/rand"
	"crypto/sha256"
	"fmt"
	"foxrouters/internal/upstream"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

func HandleAccounts(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		up := strings.ToLower(strings.TrimSpace(c.Query("upstream")))
		page := ParsePage(c.Query("page"))
		pageSize := ParsePageSize(c.Query("page_size"))

		result := gin.H{
			"grok_total":         grokAM.Len(),
			"cb_total":           cbKM.Len(),
			"grok_selector_mode": string(upstream.GetGrokSelectorMode()),
			"cb_selector_mode":   string(upstream.GetSelectorMode()),
		}

		if up == "" || up == "grok" {
			grokAccs := grokAM.GetAll()
			start, end := PageRange(page, pageSize, len(grokAccs))
			grokResult := make([]gin.H, 0, end-start)
			for _, a := range grokAccs[start:end] {
				s := a.Snapshot()
				grokResult = append(grokResult, gin.H{
					"provider": "grok", "email": s.Email, "sub": s.Sub,
					"expires_at": s.Expired, "expires_in": s.ExpiresIn,
					"last_refresh": s.LastRefresh, "disabled": s.Disabled,
					"disabled_at": s.DisabledAt, "token_status": s.TokenStatus,
					"billing_synced_at": s.BillingSyncedAt,
					"period_end":        s.PeriodEnd,
					"period_type":       s.PeriodType,
					"on_demand_cap":     s.OnDemandCap,
					"on_demand_used":    s.OnDemandUsed,
					"prepaid_balance":   s.PrepaidBalance,
					"unified_billing":   s.UnifiedBilling,
					"tokens_used":       s.TokensUsed,
					"prompt_tokens":     s.PromptTokens,
					"completion_tokens": s.CompletionTokens,
					"usage_reset_at":    s.UsageResetAt,
					"quota":             upstream.GROK_FREE_TIER_QUOTA,
					"img_quota_used":    s.ImgQuotaUsed,
					"img_quota_limit":   s.ImgQuotaLimit,
					"vid_quota_used":    s.VidQuotaUsed,
					"vid_quota_limit":   s.VidQuotaLimit,
					"chat_quota_used":   s.ChatQuotaUsed,
					"chat_quota_limit":  s.ChatQuotaLimit,
					"quota_synced_at":   s.QuotaSyncedAt,
				})
			}
			result["grok"] = grokResult
			result["grok_page"] = page
			result["grok_page_size"] = pageSize
		}

		if up == "" || up == "codebuddy" {
			cbKeys := cbKM.GetAll()
			start, end := PageRange(page, pageSize, len(cbKeys))
			cbResult := make([]gin.H, 0, end-start)
			for _, k := range cbKeys[start:end] {
				s := k.Snapshot()
				remain := s.CreditsRemain
				if remain == 0 && s.MeterSyncedAt.IsZero() {
					// Never synced — derive from fallback limit
					remain = s.CreditLimit - s.CreditsUsed
				}
				entry := gin.H{
					"provider":       "codebuddy",
					"cred_type":      string(s.CredType),
					"disabled":       s.Disabled,
					"credits_used":   s.CreditsUsed,
					"credit_limit":   s.CreditLimit,
					"credits_remain": remain,
					"credits_left":   remain,
					"total_requests": s.TotalReqs,
					"package_name":   s.PackageName,
					"cycle_end":      s.CycleEnd,
					"meter_status":   s.MeterStatus,
				}
				if !s.MeterSyncedAt.IsZero() {
					entry["meter_synced_at"] = s.MeterSyncedAt.Format(time.RFC3339)
				}
				if s.CredType == upstream.CBAuthOAuth {
					entry["email"] = s.Email
					entry["key"] = s.Email
					if !s.ExpiresAt.IsZero() {
						entry["expires_at"] = s.ExpiresAt.Format(time.RFC3339)
					}
				} else {
					entry["key"] = s.Key[:8] + "..." + s.Key[len(s.Key)-4:]
				}
				cbResult = append(cbResult, entry)
			}
			result["codebuddy"] = cbResult
			result["cb_page"] = page
			result["cb_page_size"] = pageSize
		}

		c.JSON(200, result)
	}
}

// parsePage parses a 1-based page query param (default 1).
func ParsePage(v string) int {
	p, err := strconv.Atoi(v)
	if err != nil || p < 1 {
		return 1
	}
	return p
}

// parsePageSize parses page_size (default 50, capped at 200).
func ParsePageSize(v string) int {
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 50
	}
	if n > 200 {
		return 200
	}
	return n
}

// pageRange returns the half-open [start, end) slice bounds for a page.
func PageRange(page, pageSize, total int) (int, int) {
	if page < 1 {
		page = 1
	}
	start := (page - 1) * pageSize
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	return start, end
}

// HandleRefresh forces a refresh on every Grok account (admin only).
func HandleRefresh(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		accounts := grokAM.GetAll()
		results := make([]gin.H, 0)
		for _, a := range accounts {
			err := a.Refresh()
			status := "ok"
			if err != nil {
				// Sanitize: don't leak upstream OAuth/internal details to client
				slog.Warn("refresh failed", "module", "grok", "email", a.Email, "error", err)
				status = "refresh_failed"
			}
			results = append(results, gin.H{"email": a.Email, "status": status})
		}
		c.JSON(200, gin.H{"results": results})
	}
}

func HandleImportAccount(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email        string `json:"email"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			IDToken      string `json:"id_token"`
			ExpiresIn    int    `json:"expires_in"`
			Sub          string `json:"sub"`
			SSO          string `json:"sso"`
			SSORW        string `json:"sso_rw"`
			Password     string `json:"password"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		if req.Email == "" || req.AccessToken == "" || req.RefreshToken == "" {
			c.JSON(400, gin.H{"error": "email, access_token, refresh_token are required"})
			return
		}
		_, total, acc := grokAM.UpsertAccount(req.Email, req.AccessToken, req.RefreshToken, req.IDToken, req.Sub, req.ExpiresIn)
		if req.SSO != "" {
			grokAM.SetSSO(req.Email, req.SSO, req.SSORW)
		}
		if req.Password != "" {
			grokAM.SetPassword(req.Email, req.Password)
		}
		slog.Info("imported account", "module", "grok", "email", req.Email, "total", total, "sso", req.SSO != "")
		c.JSON(200, gin.H{
			"status":  "imported",
			"email":   req.Email,
			"expired": acc.Expired,
			"total":   total,
		})
	}
}

func HandleImportAccountBulk(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Accounts []struct {
				Email        string `json:"email"`
				AccessToken  string `json:"access_token"`
				RefreshToken string `json:"refresh_token"`
				IDToken      string `json:"id_token"`
				ExpiresIn    int    `json:"expires_in"`
				Sub          string `json:"sub"`
				SSO          string `json:"sso"`
				SSORW        string `json:"sso_rw"`
				Password     string `json:"password"`
			} `json:"accounts"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(400, gin.H{"error": "invalid JSON body"})
			return
		}
		if len(req.Accounts) == 0 {
			c.JSON(400, gin.H{"error": "accounts array is required"})
			return
		}
		added := 0
		updated := 0
		failed := 0
		for _, a := range req.Accounts {
			if a.Email == "" || a.AccessToken == "" || a.RefreshToken == "" {
				failed++
				continue
			}
			created, _, _ := grokAM.UpsertAccount(a.Email, a.AccessToken, a.RefreshToken, a.IDToken, a.Sub, a.ExpiresIn)
			if a.SSO != "" {
				grokAM.SetSSO(a.Email, a.SSO, a.SSORW)
			}
			if a.Password != "" {
				grokAM.SetPassword(a.Email, a.Password)
			}
			if created {
				added++
			} else {
				updated++
			}
		}
		slog.Info("bulk import", "module", "grok", "added", added, "updated", updated, "failed", failed, "total", grokAM.Len())
		c.JSON(200, gin.H{
			"added":   added,
			"updated": updated,
			"failed":  failed,
			"total":   grokAM.Len(),
		})
	}
}

// HandleDeleteAccount removes a Grok account from Redis + runtime pool.
func HandleDeleteAccount(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		email := c.Param("email")
		if email == "" {
			c.JSON(400, gin.H{"error": "email required"})
			return
		}
		if !grokAM.DeleteAccount(email) {
			c.JSON(404, gin.H{"error": "account not found", "email": email})
			return
		}
		remaining := grokAM.Len()
		slog.Info("deleted account", "module", "grok", "email", email, "remaining", remaining)
		c.JSON(200, gin.H{"status": "deleted", "email": email, "remaining": remaining})
	}
}

func HandleCleanupDisabled(grokAM *upstream.GrokAccountManager, cbKM *upstream.CBKeyManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		typ := c.DefaultQuery("type", "all")
		result := gin.H{"type": typ}

		if typ == "grok" || typ == "all" {
			removed := grokAM.CleanupDisabled()
			result["grok_removed"] = removed
			result["grok_remaining"] = grokAM.Len()
		}
		if typ == "cb" || typ == "all" {
			removed := cbKM.CleanupDisabled()
			result["cb_removed"] = removed
			result["cb_remaining"] = cbKM.Len()
		}

		slog.Info("cleanup disabled", "module", "admin", "type", typ,
			"grok_removed", result["grok_removed"], "cb_removed", result["cb_removed"])
		c.JSON(200, result)
	}
}

// HandleCleanupBanned removes all banned Grok accounts (token_status == "banned").
// Query param ?type=grok|all (default: grok). CB has no "banned" status.
// Note: banned ≡ permanently disabled (disabled && disabledAt zero). Cooldown preserved.
func HandleCleanupBanned(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		typ := c.DefaultQuery("type", "grok")
		result := gin.H{"type": typ}

		if typ == "grok" || typ == "all" {
			removed := grokAM.CleanupBanned()
			result["grok_removed"] = removed
			result["grok_remaining"] = grokAM.Len()
		}

		slog.Info("cleanup banned", "module", "admin", "type", typ,
			"grok_removed", result["grok_removed"])
		c.JSON(200, result)
	}
}

// HandleSyncGrokBilling triggers a billing sync for one or all Grok accounts.
// Body optional: {"email": "..."} to sync one; empty = all non-banned accounts.
func HandleSyncGrokBilling(grokAM *upstream.GrokAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req struct {
			Email string `json:"email"`
		}
		_ = c.ShouldBindJSON(&req)
		target := strings.TrimSpace(req.Email)

		type result struct {
			Email          string `json:"email"`
			PeriodEnd      string `json:"period_end,omitempty"`
			OnDemandCap    int64  `json:"on_demand_cap"`
			OnDemandUsed   int64  `json:"on_demand_used"`
			PrepaidBalance int64  `json:"prepaid_balance"`
			UnifiedBilling bool   `json:"unified_billing"`
			Error          string `json:"error,omitempty"`
		}

		var accounts []*upstream.GrokAccount
		if target != "" {
			found := false
			for _, a := range grokAM.GetAll() {
				if a.Email == target {
					accounts = append(accounts, a)
					found = true
					break
				}
			}
			if !found {
				c.JSON(404, gin.H{"error": "account not found"})
				return
			}
		} else {
			accounts = grokAM.GetAll()
		}

		results := make([]result, 0, len(accounts))
		synced, failed := 0, 0
		for _, a := range accounts {
			r := result{Email: a.Email}
			if err := a.SyncBilling(); err != nil {
				failed++
				r.Error = err.Error()
			} else {
				synced++
				s := a.Snapshot()
				r.PeriodEnd = s.PeriodEnd
				r.OnDemandCap = s.OnDemandCap
				r.OnDemandUsed = s.OnDemandUsed
				r.PrepaidBalance = s.PrepaidBalance
				r.UnifiedBilling = s.UnifiedBilling
			}
			results = append(results, r)
		}
		c.JSON(200, gin.H{
			"synced":  synced,
			"failed":  failed,
			"results": results,
		})
	}
}

// HandleCacheTemp exposes the per-account cache-temperature map (which
// accounts hold warm upstream prompt caches for which content prefixes).
// Admin-only observability for cache-temperature-aware routing. Account
// identifiers are REDACTED (full API keys / OAuth tokens never leave the
// server); prefix keys are already short hashes.
func HandleCacheTemp(cbKM *upstream.CBKeyManager, grokAM *upstream.GrokAccountManager, fbAM *upstream.FreebuffAccountManager) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.JSON(200, gin.H{
			"codebuddy": maskCacheTempKeys(cbKM.SnapshotCacheTemp()),
			"grok":      maskCacheTempKeys(grokAM.SnapshotCacheTemp()),
			"freebuff":  maskCacheTempKeys(fbAM.SnapshotCacheTemp()),
			"note":      "ema = exponential moving average of upstream cache hit %; entries older than 30m are considered cold; account keys are masked, prefix keys are hashes",
		})
	}
}

// maskCacheTempKeys redacts the account-identifier dimension of a cache-temp
// snapshot: ck_*/eyJ* keys keep only a prefix hint, email-like identifiers
// keep first 3 chars + domain. When two distinct keys mask to the same string
// (e.g. ck_ keys sharing a 12-char prefix, or emails sharing local-part prefix
// + domain), a short FNV hash suffix is appended so no entry is silently
// dropped from the debug output.
func maskCacheTempKeys(m map[string]map[string]upstream.CacheTempSnap) map[string]map[string]upstream.CacheTempSnap {
	if m == nil {
		return nil
	}
	out := make(map[string]map[string]upstream.CacheTempSnap, len(m))
	seen := make(map[string]string, len(m)) // masked -> original key that claimed it
	for k, v := range m {
		mk := maskAccountKey(k)
		if orig, exists := seen[mk]; exists && orig != k {
			// collision — disambiguate with a short hash of the full key
			h := fnv.New64a()
			h.Write([]byte(k))
			mk = mk + "#" + fmt.Sprintf("%016x", h.Sum64())[:8]
		}
		seen[mk] = k
		out[mk] = v
	}
	return out
}

// maskKeyPepper is a per-process random key for maskAccountKey's HMAC
// (SEC2-06): a bare unsalted SHA-256 digest is a stable, deployment-
// independent confirmation oracle — anyone holding the /debug/cache-temp
// response plus a candidate credential list could verify membership offline.
// The per-process random key makes the digest meaningless outside this
// process lifetime.
var maskKeyPepper = func() []byte {
	b := make([]byte, 32)
	if _, err := crypto_rand.Read(b); err != nil {
		// Practically unreachable; fall back to time-seeded entropy so the
		// mask is still not deployment-stable.
		return []byte(fmt.Sprintf("fp-%d", time.Now().UnixNano()))
	}
	return b
}()

// maskAccountKey redacts an account identifier for display. Emails keep the
// first 3 chars of the local-part + domain; non-email credentials (API keys,
// OAuth tokens) are NEVER returned raw or as plaintext prefixes — they map to
// a per-process HMAC-SHA256 digest, regardless of length (SEC-03: short
// credentials must not leak verbatim; SEC2-06: digest must not be a stable
// confirmation oracle).
func maskAccountKey(k string) string {
	if i := strings.IndexByte(k, '@'); i > 0 {
		pre := k[:i]
		if len(pre) > 3 {
			pre = pre[:3]
		}
		return pre + "***" + k[i:]
	}
	mac := hmac.New(sha256.New, maskKeyPepper)
	mac.Write([]byte(k))
	return "mask:" + fmt.Sprintf("%x", mac.Sum(nil))[:12]
}
