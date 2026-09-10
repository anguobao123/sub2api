package routes

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

type openClawRouteRepositoryStub struct {
	usageCommands []service.OpenClawUsageEventCommand
	reserveErr    error
	reserveResult *service.OpenClawLeaseResult
}

func (s *openClawRouteRepositoryStub) EnsureAccount(_ context.Context, provision service.OpenClawAccountProvision) (*service.OpenClawBillingAccount, error) {
	return &service.OpenClawBillingAccount{BillingAccountID: provision.BillingAccountID, BillingUserID: 1, Balance: provision.DefaultGrantUSD, InitialGrantUSD: provision.DefaultGrantUSD, Created: true}, nil
}

func (s *openClawRouteRepositoryStub) FindAccount(context.Context, string) (*service.OpenClawBillingAccount, error) {
	return nil, service.ErrOpenClawAccountNotFound
}

func (s *openClawRouteRepositoryStub) ReserveLease(context.Context, service.OpenClawReserveLeaseCommand) (*service.OpenClawLeaseResult, error) {
	if s.reserveErr != nil {
		return nil, s.reserveErr
	}
	if s.reserveResult != nil {
		return s.reserveResult, nil
	}
	return nil, service.ErrOpenClawBillingInvalid
}

func (s *openClawRouteRepositoryStub) CaptureLease(context.Context, service.OpenClawLeaseOperationCommand) (*service.OpenClawLeaseResult, error) {
	return nil, service.ErrOpenClawBillingInvalid
}

func (s *openClawRouteRepositoryStub) ReleaseLease(context.Context, service.OpenClawLeaseOperationCommand) (*service.OpenClawLeaseResult, error) {
	return nil, service.ErrOpenClawBillingInvalid
}

func (s *openClawRouteRepositoryStub) ExpireLeases(context.Context, int) (int, error) { return 0, nil }

func (s *openClawRouteRepositoryStub) IngestUsageEvent(_ context.Context, command service.OpenClawUsageEventCommand) (*service.OpenClawUsageEventResult, error) {
	s.usageCommands = append(s.usageCommands, command)
	status := service.OpenClawUsageStatusRecorded
	if !command.PricingComplete {
		status = service.OpenClawUsageStatusPendingPricing
	}
	return &service.OpenClawUsageEventResult{EventID: command.EventID, Applied: true, Status: status}, nil
}

func TestOpenClawUsageEventsBatchRouteUsesInternalBearerAndPersistsPendingPricing(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := strings.Repeat("b", 32)
	identityKey := strings.Repeat("h", 32)
	repo := &openClawRouteRepositoryStub{}
	bridgeService := service.NewOpenClawBillingService(repo, &config.Config{OpenClawBilling: config.OpenClawBillingConfig{
		Enabled:                true,
		InternalBearer:         bearer,
		IdentityHMACKey:        identityKey,
		DefaultGrantUSD:        50,
		LeaseTTLSeconds:        900,
		MaxUsageEventsPerBatch: 10,
		ExpiryBatchSize:        10,
	}})
	router := gin.New()
	RegisterOpenClawBillingRoutes(router, handler.NewOpenClawBillingHandler(bridgeService), middleware.NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: bearer, IdentityHMACKey: identityKey},
	}))

	body := []byte(`{"events":[{"eventId":"evt-1","platformUserId":"suite-user-123","taskId":"task-1","sessionId":"session-1","nodeId":"node-1","codexThreadId":null,"model":"gpt-5.4","inputTokens":null,"outputTokens":25,"cacheReadTokens":null,"cacheWriteTokens":null,"observedAt":"2026-08-13T10:00:00Z","reportedAt":"2026-08-13T10:00:01Z","pricingVersion":null,"priceSnapshot":null,"estimatedUsd":null}]}`)

	var successfulResponseBody string
	for _, test := range []struct {
		name       string
		authority  string
		apiKey     string
		wantStatus int
	}{
		{name: "missing bearer", wantStatus: http.StatusUnauthorized},
		{name: "browser jwt", authority: "Bearer eyJhbGciOiJIUzI1NiJ9.fake.jwt", wantStatus: http.StatusUnauthorized},
		{name: "api key only", apiKey: "sk-sub2api-not-bridge", wantStatus: http.StatusUnauthorized},
		{name: "independent bearer", authority: "Bearer " + bearer, wantStatus: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/usage-events:batch", bytes.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Authorization", test.authority)
			request.Header.Set("X-API-Key", test.apiKey)
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.wantStatus, recorder.Code)
			require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
			if test.wantStatus == http.StatusOK {
				require.JSONEq(t, `{"events":[{"event_id":"evt-1","applied":true,"status":"pending_pricing"}]}`, recorder.Body.String())
				successfulResponseBody = recorder.Body.String()
			}
		})
	}

	require.Len(t, repo.usageCommands, 1)
	require.False(t, repo.usageCommands[0].PricingComplete)
	require.Nil(t, repo.usageCommands[0].InputTokens)
	require.NotContains(t, successfulResponseBody, "suite-user-123")
}

func TestOpenClawUsageEventsBatchRejectsNearMissLiteralAction(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := strings.Repeat("b", 32)
	router := gin.New()
	RegisterOpenClawBillingRoutes(router, handler.NewOpenClawBillingHandler(nil), middleware.NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: bearer, IdentityHMACKey: strings.Repeat("h", 32)},
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/usage-events:other", strings.NewReader(`{"events":[]}`))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)
	require.Equal(t, http.StatusNotFound, recorder.Code)
}

func TestOpenClawReserveRouteRejectsInsufficientBalanceWithoutIdentityEcho(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := strings.Repeat("b", 32)
	identityKey := strings.Repeat("h", 32)
	repo := &openClawRouteRepositoryStub{reserveErr: service.ErrOpenClawInsufficientBalance}
	bridgeService := service.NewOpenClawBillingService(repo, &config.Config{OpenClawBilling: config.OpenClawBillingConfig{
		Enabled:                true,
		InternalBearer:         bearer,
		IdentityHMACKey:        identityKey,
		DefaultGrantUSD:        50,
		LeaseTTLSeconds:        900,
		MaxUsageEventsPerBatch: 10,
		ExpiryBatchSize:        10,
	}})
	router := gin.New()
	RegisterOpenClawBillingRoutes(router, handler.NewOpenClawBillingHandler(bridgeService), middleware.NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: bearer, IdentityHMACKey: identityKey},
	}))

	const platformUserID = "suite-user-without-balance"
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/task-budget-leases:reserve", strings.NewReader(`{
		"platformUserId":"`+platformUserID+`",
		"taskId":"task-1",
		"sessionId":"session-1",
		"nodeId":"node-1",
		"amountUsd":{"currency":"USD","minorAmount":"500000000","scale":8},
		"idempotencyKey":"openclaw:task-1:reserve:v1"
	}`))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusPaymentRequired, recorder.Code)
	require.Equal(t, "no-store", recorder.Header().Get("Cache-Control"))
	require.JSONEq(t, `{"error":{"code":"OPENCLAW_INSUFFICIENT_BALANCE","message":"insufficient balance for openclaw budget reservation"}}`, recorder.Body.String())
	require.NotContains(t, recorder.Body.String(), platformUserID)
}

func TestOpenClawBridgeAmountsUseExactObjectsAndRejectLegacyNumbers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := strings.Repeat("b", 32)
	identityKey := strings.Repeat("h", 32)
	repo := &openClawRouteRepositoryStub{reserveResult: &service.OpenClawLeaseResult{
		Applied:       true,
		BalanceUSD:    serviceOpenClawAmount("49.87654321"),
		FrozenBalance: serviceOpenClawAmount("0.12345679"),
		Lease: service.OpenClawBudgetLease{
			LeaseID: "ocbl_test", BillingAccountID: "ocba_test", TaskID: "task-1", SessionID: "session-1", NodeID: "node-1",
			ReservedUSD: serviceOpenClawAmount("0.12345679"), CapturedUSD: serviceOpenClawAmount("0"), ReleasedUSD: serviceOpenClawAmount("0"),
			Status: service.OpenClawLeaseStatusActive,
		},
	}}
	bridgeService := service.NewOpenClawBillingService(repo, &config.Config{OpenClawBilling: config.OpenClawBillingConfig{
		Enabled: true, InternalBearer: bearer, IdentityHMACKey: identityKey, DefaultGrantUSD: 50, LeaseTTLSeconds: 900, MaxUsageEventsPerBatch: 10, ExpiryBatchSize: 10,
	}})
	router := gin.New()
	RegisterOpenClawBillingRoutes(router, handler.NewOpenClawBillingHandler(bridgeService), middleware.NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: bearer, IdentityHMACKey: identityKey},
	}))

	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{name: "exact object", body: `{"platformUserId":"user-1","taskId":"task-1","sessionId":"session-1","nodeId":"node-1","amountUsd":{"currency":"USD","minorAmount":"12345679","scale":8},"idempotencyKey":"reserve-1"}`, want: http.StatusOK},
		{name: "legacy json number", body: `{"platformUserId":"user-1","taskId":"task-1","sessionId":"session-1","nodeId":"node-1","amountUsd":0.12345679,"idempotencyKey":"reserve-1"}`, want: http.StatusBadRequest},
		{name: "unknown amount field", body: `{"platformUserId":"user-1","taskId":"task-1","sessionId":"session-1","nodeId":"node-1","amountUsd":{"currency":"USD","minorAmount":"12345679","scale":8,"extra":true},"idempotencyKey":"reserve-1"}`, want: http.StatusBadRequest},
		{name: "wrong scale", body: `{"platformUserId":"user-1","taskId":"task-1","sessionId":"session-1","nodeId":"node-1","amountUsd":{"currency":"USD","minorAmount":"12345679","scale":2},"idempotencyKey":"reserve-1"}`, want: http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/task-budget-leases:reserve", strings.NewReader(test.body))
			request.Header.Set("Authorization", "Bearer "+bearer)
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.want, recorder.Code)
			if test.want == http.StatusOK {
				require.JSONEq(t, `{"applied":true,"lease":{"lease_id":"ocbl_test","billing_account_id":"ocba_test","task_id":"task-1","session_id":"session-1","node_id":"node-1","reserved_usd":{"currency":"USD","minorAmount":"12345679","scale":8},"captured_usd":{"currency":"USD","minorAmount":"0","scale":8},"released_usd":{"currency":"USD","minorAmount":"0","scale":8},"status":"active","expires_at":"0001-01-01T00:00:00Z","created_at":"0001-01-01T00:00:00Z","finalized_at":null},"balance_usd":{"currency":"USD","minorAmount":"4987654321","scale":8},"frozen_balance_usd":{"currency":"USD","minorAmount":"12345679","scale":8}}`, recorder.Body.String())
			}
		})
	}
}

func TestOpenClawAccountResolveResponseUsesExactAmountObjects(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := strings.Repeat("b", 32)
	repo := &openClawRouteRepositoryStub{}
	bridgeService := service.NewOpenClawBillingService(repo, &config.Config{OpenClawBilling: config.OpenClawBillingConfig{
		Enabled: true, InternalBearer: bearer, IdentityHMACKey: strings.Repeat("h", 32), DefaultGrantUSD: 50, LeaseTTLSeconds: 900, MaxUsageEventsPerBatch: 10, ExpiryBatchSize: 10,
	}})
	router := gin.New()
	RegisterOpenClawBillingRoutes(router, handler.NewOpenClawBillingHandler(bridgeService), middleware.NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: bearer, IdentityHMACKey: strings.Repeat("h", 32)},
	}))
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/accounts:resolve", strings.NewReader(`{"platformUserId":"user-1"}`))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(recorder, request)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.JSONEq(t, `{"billingAccountId":"`+extractOpenClawAccountID(t, recorder.Body.String())+`","balanceUsd":{"currency":"USD","minorAmount":"5000000000","scale":8},"frozenBalanceUsd":{"currency":"USD","minorAmount":"0","scale":8},"initialGrantUsd":{"currency":"USD","minorAmount":"5000000000","scale":8},"created":true}`, recorder.Body.String())
}

func TestOpenClawUsageRouteRejectsNumericPriceSnapshotButAllowsExactAmountObject(t *testing.T) {
	gin.SetMode(gin.TestMode)
	bearer := strings.Repeat("b", 32)
	repo := &openClawRouteRepositoryStub{}
	bridgeService := service.NewOpenClawBillingService(repo, &config.Config{OpenClawBilling: config.OpenClawBillingConfig{
		Enabled: true, InternalBearer: bearer, IdentityHMACKey: strings.Repeat("h", 32), DefaultGrantUSD: 50, LeaseTTLSeconds: 900, MaxUsageEventsPerBatch: 10, ExpiryBatchSize: 10,
	}})
	router := gin.New()
	RegisterOpenClawBillingRoutes(router, handler.NewOpenClawBillingHandler(bridgeService), middleware.NewOpenClawBillingAuth(&config.Config{
		OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: bearer, IdentityHMACKey: strings.Repeat("h", 32)},
	}))

	for _, test := range []struct {
		name          string
		priceSnapshot string
		want          int
	}{
		{name: "numeric price", priceSnapshot: `{"input":0.000005}`, want: http.StatusBadRequest},
		{name: "exact object price", priceSnapshot: `{"input":{"currency":"USD","minorAmount":"500","scale":8}}`, want: http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `{"events":[{"eventId":"evt-` + strings.ReplaceAll(test.name, " ", "-") + `","platformUserId":"user-1","taskId":"task-1","sessionId":"session-1","nodeId":"node-1","model":"gpt-5.4","inputTokens":null,"outputTokens":null,"cacheReadTokens":null,"cacheWriteTokens":null,"observedAt":"2026-08-13T10:00:00Z","reportedAt":"2026-08-13T10:00:01Z","pricingVersion":"price-v1","priceSnapshot":` + test.priceSnapshot + `,"estimatedUsd":{"currency":"USD","minorAmount":"500","scale":8}}]}`
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodPost, "/internal/openclaw/v1/usage-events:batch", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer "+bearer)
			request.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(recorder, request)
			require.Equal(t, test.want, recorder.Code)
		})
	}
	require.Len(t, repo.usageCommands, 1)
}

func serviceOpenClawAmount(value string) decimal.Decimal {
	return decimal.RequireFromString(value)
}

func extractOpenClawAccountID(t *testing.T, body string) string {
	t.Helper()
	var response struct {
		BillingAccountID string `json:"billingAccountId"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &response))
	require.NotEmpty(t, response.BillingAccountID)
	return response.BillingAccountID
}
