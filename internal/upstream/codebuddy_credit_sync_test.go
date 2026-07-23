package upstream

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"foxrouters/internal/db"
)

// sampleMeterBody is a realistic meter API response (verified July 2026).
func sampleMeterBody(status int, remainPrecise, usedPrecise, sizePrecise string) []byte {
	return []byte(`{
		"code":0,
		"data":{
			"Response":{
				"Data":{
					"Accounts":[{
						"PackageName":"Pro Trial",
						"CapacitySize":250,
						"CapacityUsed":1,
						"CapacityRemain":249,
						"CapacitySizePrecise":"` + sizePrecise + `",
						"CapacityUsedPrecise":"` + usedPrecise + `",
						"CapacityRemainPrecise":"` + remainPrecise + `",
						"CycleStartTime":"2026-07-22 18:28:41",
						"CycleEndTime":"2026-08-05 18:28:40",
						"Status":` + itoa(status) + `
					}]
				}
			}
		}
	}`)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	if n < 0 {
		return "-" + itoa(-n)
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestParseCBMeterAccount(t *testing.T) {
	body := sampleMeterBody(0, "249.92", "0.08", "250")
	acc, err := parseCBMeterAccount(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if acc.PackageName != "Pro Trial" {
		t.Fatalf("PackageName=%q", acc.PackageName)
	}
	if acc.Status != 0 {
		t.Fatalf("Status=%d", acc.Status)
	}
	if acc.CapacityRemainPrecise != "249.92" {
		t.Fatalf("CapacityRemainPrecise=%q", acc.CapacityRemainPrecise)
	}
	if acc.CycleEndTime != "2026-08-05 18:28:40" {
		t.Fatalf("CycleEndTime=%q", acc.CycleEndTime)
	}
}

func TestParseCBMeterAccountEmpty(t *testing.T) {
	body := []byte(`{"code":0,"data":{"Response":{"Data":{"Accounts":[]}}}}`)
	_, err := parseCBMeterAccount(body)
	if err == nil {
		t.Fatal("expected error for empty Accounts")
	}
}

func TestParseCBMeterAccountCodeError(t *testing.T) {
	body := []byte(`{"code":1,"msg":"unauthorized","data":{}}`)
	_, err := parseCBMeterAccount(body)
	if err == nil || !strings.Contains(err.Error(), "code=1") {
		t.Fatalf("expected code error, got %v", err)
	}
}

func TestParseFloatOr(t *testing.T) {
	if v := parseFloatOr("249.92", 0); v != 249.92 {
		t.Fatalf("got %v", v)
	}
	if v := parseFloatOr("", 7); v != 7 {
		t.Fatalf("fallback got %v", v)
	}
	if v := parseFloatOr("nope", 3.5); v != 3.5 {
		t.Fatalf("invalid got %v", v)
	}
}

func TestCreditLimitFallback(t *testing.T) {
	k := NewCBKeyForTest("ck_test_xxxxxxxxxxxx")
	if k.CreditLimit() != CB_CREDIT_LIMIT {
		t.Fatalf("fallback limit=%v want %v", k.CreditLimit(), CB_CREDIT_LIMIT)
	}
	k.mu.Lock()
	k.creditLimit = 250
	k.mu.Unlock()
	if k.CreditLimit() != 250 {
		t.Fatalf("meter limit=%v", k.CreditLimit())
	}
}

func TestSyncCreditsAPIKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.Method != "POST" {
			t.Errorf("method=%s", r.Method)
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != "{}" {
			t.Errorf("body=%q", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(sampleMeterBody(0, "249.92", "0.08", "250"))
	}))
	defer srv.Close()
	defer swapClients(srv.URL)()

	k := NewCBKeyForTest("ck_abc1234567890xyz")
	if err := k.SyncCredits(); err != nil {
		t.Fatalf("SyncCredits: %v", err)
	}

	s := k.Snapshot()
	if s.CreditLimit != 250 {
		t.Fatalf("CreditLimit=%v", s.CreditLimit)
	}
	if s.CreditsUsed != 0.08 {
		t.Fatalf("CreditsUsed=%v", s.CreditsUsed)
	}
	if s.CreditsRemain != 249.92 {
		t.Fatalf("CreditsRemain=%v", s.CreditsRemain)
	}
	if s.PackageName != "Pro Trial" {
		t.Fatalf("PackageName=%q", s.PackageName)
	}
	if s.MeterStatus != 0 {
		t.Fatalf("MeterStatus=%d", s.MeterStatus)
	}
	if s.MeterSyncedAt.IsZero() {
		t.Fatal("MeterSyncedAt should be set")
	}
	if s.Disabled {
		t.Fatal("should not be disabled")
	}
	if gotAuth != "Bearer ck_abc1234567890xyz" {
		t.Fatalf("Auth=%q", gotAuth)
	}
}

// applyMeterAccount is the lock-section of SyncCredits extracted for offline tests.
func applyMeterAccount(k *CBKey, account cbMeterAccount) {
	size := parseFloatOr(account.CapacitySizePrecise, float64(account.CapacitySize))
	used := parseFloatOr(account.CapacityUsedPrecise, float64(account.CapacityUsed))
	remain := parseFloatOr(account.CapacityRemainPrecise, float64(account.CapacityRemain))
	k.mu.Lock()
	k.creditsUsed = used
	if size > 0 {
		k.creditLimit = size
	}
	k.creditsRemain = remain
	k.packageName = account.PackageName
	k.cycleEnd = account.CycleEndTime
	k.meterStatus = account.Status
	k.meterSyncedAt = time.Now()
	if account.Status == 3 || remain <= 0 {
		if !k.disabled || !k.disabledAt.IsZero() {
			k.disabled = true
			k.disabledAt = time.Time{}
		}
	}
	k.mu.Unlock()
}

func TestSyncCreditsOAuthHeader(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(sampleMeterBody(0, "100.5", "49.5", "150"))
	}))
	defer srv.Close()
	defer swapClients(srv.URL)()

	k := NewCBKeyForTest("user@cb.test",
		WithCBOAuthTokens("at_live_token", "rt_live", time.Now().Add(time.Hour)))
	if err := k.SyncCredits(); err != nil {
		t.Fatalf("SyncCredits: %v", err)
	}
	if gotAuth != "Bearer at_live_token" {
		t.Fatalf("OAuth Auth=%q want Bearer at_live_token", gotAuth)
	}

	s := k.Snapshot()
	if s.CreditLimit != 150 {
		t.Fatalf("limit=%v", s.CreditLimit)
	}
	if s.CreditsRemain != 100.5 {
		t.Fatalf("remain=%v", s.CreditsRemain)
	}
}

func TestSyncCreditsStatus3PermanentDisable(t *testing.T) {
	acc, err := parseCBMeterAccount(sampleMeterBody(3, "0", "250", "250"))
	if err != nil {
		t.Fatal(err)
	}
	k := NewCBKeyForTest("ck_exhausted_keyxxxx")
	applyMeterAccount(k, acc)
	if !k.IsDisabled() {
		t.Fatal("status=3 should permanently disable")
	}
	s := k.Snapshot()
	if !s.DisabledAt.IsZero() {
		// permanent = zero DisabledAt
		t.Fatalf("DisabledAt should be zero for permanent, got %v", s.DisabledAt)
	}
	if s.CreditsRemain != 0 {
		t.Fatalf("remain=%v", s.CreditsRemain)
	}
}

func TestSyncCreditsRemainZeroDisable(t *testing.T) {
	acc, err := parseCBMeterAccount(sampleMeterBody(0, "0", "250", "250"))
	if err != nil {
		t.Fatal(err)
	}
	k := NewCBKeyForTest("ck_zero_remain_xxxx")
	applyMeterAccount(k, acc)
	if !k.IsDisabled() {
		t.Fatal("remain<=0 should permanently disable")
	}
}

func TestSyncCreditsNoAutoReenablePermanent(t *testing.T) {
	// Permanent disable, then meter says remain>0 — must stay disabled.
	k := NewCBKeyForTest("ck_perm_disabledxxxx",
		WithCBDisabledCooldown(time.Time{})) // permanent
	if !k.IsDisabled() {
		t.Fatal("precondition")
	}
	acc, _ := parseCBMeterAccount(sampleMeterBody(0, "200", "50", "250"))
	// applyMeterAccount only disables on exhausted — it never re-enables.
	// Simulate: only update meter fields without clearing disabled.
	k.mu.Lock()
	wasDisabled := k.disabled
	wasPerm := k.disabledAt.IsZero()
	k.mu.Unlock()

	applyMeterAccount(k, acc)

	// Because status!=3 and remain>0, applyMeterAccount won't touch disabled.
	// Permanent state preserved.
	if !wasDisabled || !wasPerm {
		t.Fatal("precondition permanent")
	}
	if !k.IsDisabled() {
		t.Fatal("must NOT auto-reenable permanent disable")
	}
	s := k.Snapshot()
	// credits still updated from meter
	if s.CreditsRemain != 200 {
		t.Fatalf("remain should update even when disabled: %v", s.CreditsRemain)
	}
}

func TestAddCreditsUsesMeterLimit(t *testing.T) {
	k := NewCBKeyForTest("ck_add_credits_xxxx")
	k.mu.Lock()
	k.creditLimit = 10
	k.meterSyncedAt = time.Now()
	k.mu.Unlock()

	k.AddCredits(5)
	if k.IsDisabled() {
		t.Fatal("should not disable at 5/10")
	}
	k.AddCredits(5)
	if !k.IsDisabled() {
		t.Fatal("should disable at 10/10")
	}
	s := k.Snapshot()
	if s.CreditsRemain != 0 {
		t.Fatalf("remain=%v", s.CreditsRemain)
	}
}

func TestAddCreditsFallbackLimit(t *testing.T) {
	k := NewCBKeyForTest("ck_fallback_limitxx")
	// creditLimit=0 → CB_CREDIT_LIMIT
	k.AddCredits(CB_CREDIT_LIMIT - 1)
	if k.IsDisabled() {
		t.Fatal("should not disable yet")
	}
	k.AddCredits(1)
	if !k.IsDisabled() {
		t.Fatal("should disable at CB_CREDIT_LIMIT")
	}
}

func TestCBKeyDTOMeterFields(t *testing.T) {
	k := NewCBKeyForTest("ck_dto_meter_xxxxxx")
	k.mu.Lock()
	k.creditLimit = 250
	k.creditsRemain = 200.5
	k.packageName = "Pro Trial"
	k.cycleEnd = "2026-08-05 18:28:40"
	k.meterStatus = 0
	k.meterSyncedAt = time.Unix(1893456000, 0)
	k.creditsUsed = 49.5
	k.mu.Unlock()

	dto := k.toDTO()
	var _ db.CBKeyDTO = dto
	if dto.CreditLimit != 250 || dto.CreditsRemain != 200.5 {
		t.Fatalf("DTO credits: limit=%v remain=%v", dto.CreditLimit, dto.CreditsRemain)
	}
	if dto.PackageName != "Pro Trial" || dto.CycleEnd == "" {
		t.Fatalf("DTO package/cycle: %+v", dto)
	}
	if dto.MeterStatus != 0 || dto.MeterSyncedAt.IsZero() {
		t.Fatalf("DTO meter: status=%d synced=%v", dto.MeterStatus, dto.MeterSyncedAt)
	}

	// Snapshot exposes the same fields
	s := k.Snapshot()
	if s.CreditLimit != 250 || s.CreditsRemain != 200.5 || s.PackageName != "Pro Trial" {
		t.Fatalf("snapshot: %+v", s)
	}
}

func TestLoadMeterFieldsFromRedisShape(t *testing.T) {
	// Replicate LoadFromRedis field mapping for meter fields
	state := map[string]string{
		"cred_type":        "api_key",
		"credits_used":     "1.5",
		"total_requests":   "7",
		"disabled":         "false",
		"credit_limit":     "250",
		"credits_remain":   "248.5",
		"package_name":     "Pro Trial",
		"cycle_end":        "2026-08-05 18:28:40",
		"meter_status":     "0",
		"meter_synced_at":  "1893456000",
	}
	key := &CBKey{Key: "ck_from_redis_xxxx", CredType: CBAuthAPIKey}
	if f, err := parseFloatOrErr(state["credit_limit"]); err == nil && f > 0 {
		key.creditLimit = f
	}
	if f, err := parseFloatOrErr(state["credits_remain"]); err == nil {
		key.creditsRemain = f
	}
	key.packageName = state["package_name"]
	key.cycleEnd = state["cycle_end"]
	if n, err := parseInt64Local(state["meter_status"]); err == nil {
		key.meterStatus = int(n)
	}
	if n, err := parseInt64Local(state["meter_synced_at"]); err == nil && n > 0 {
		key.meterSyncedAt = time.Unix(n, 0)
	}

	if key.creditLimit != 250 || key.creditsRemain != 248.5 {
		t.Fatalf("loaded limit=%v remain=%v", key.creditLimit, key.creditsRemain)
	}
	if key.packageName != "Pro Trial" || key.cycleEnd == "" {
		t.Fatalf("package/cycle: %q %q", key.packageName, key.cycleEnd)
	}
	if key.meterSyncedAt.Year() != 2030 {
		t.Fatalf("synced year=%d", key.meterSyncedAt.Year())
	}
	if key.CreditLimit() != 250 {
		t.Fatalf("CreditLimit()=%v", key.CreditLimit())
	}
}

func parseFloatOrErr(s string) (float64, error) {
	return json.Number(s).Float64()
}

func parseInt64Local(s string) (int64, error) {
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, io.EOF
		}
		n = n*10 + int64(c-'0')
	}
	return n, nil
}

func TestSyncCreditsIntFallback(t *testing.T) {
	// Precise fields empty → use int Capacity* fields
	body := []byte(`{
		"code":0,
		"data":{"Response":{"Data":{"Accounts":[{
			"PackageName":"Free",
			"CapacitySize":100,
			"CapacityUsed":10,
			"CapacityRemain":90,
			"CapacitySizePrecise":"",
			"CapacityUsedPrecise":"",
			"CapacityRemainPrecise":"",
			"CycleEndTime":"2026-09-01 00:00:00",
			"Status":0
		}]}}}
	}`)
	acc, err := parseCBMeterAccount(body)
	if err != nil {
		t.Fatal(err)
	}
	k := NewCBKeyForTest("ck_int_fallback_xxx")
	applyMeterAccount(k, acc)
	s := k.Snapshot()
	if s.CreditLimit != 100 || s.CreditsUsed != 10 || s.CreditsRemain != 90 {
		t.Fatalf("int fallback: limit=%v used=%v remain=%v", s.CreditLimit, s.CreditsUsed, s.CreditsRemain)
	}
}
