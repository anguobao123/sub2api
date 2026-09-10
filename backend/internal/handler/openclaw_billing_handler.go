package handler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
)

const openClawBillingMaxRequestBodyBytes int64 = 2 * 1024 * 1024
const openClawUSDCurrency = "USD"

// openClawUSDAmount rejects JSON numbers. Amounts cross the bridge as a
// decimal-integer count of fixed-scale minor units.
type openClawUSDAmount struct {
	value decimal.Decimal
}

func (a *openClawUSDAmount) UnmarshalJSON(data []byte) error {
	if a == nil || bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return fmt.Errorf("openclaw usd amount must be an object")
	}
	var wire struct {
		Currency    string `json:"currency"`
		MinorAmount string `json:"minorAmount"`
		Scale       int32  `json:"scale"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return err
	}
	if wire.Currency != openClawUSDCurrency || wire.Scale != service.OpenClawUSDAmountScale || !openClawHTTPMinorAmountDigits(wire.MinorAmount) {
		return fmt.Errorf("invalid openclaw usd amount")
	}
	minor, err := decimal.NewFromString(wire.MinorAmount)
	if err != nil {
		return err
	}
	value := minor.Shift(-service.OpenClawUSDAmountScale)
	if _, err := service.NormalizeOpenClawUSDAmount(value, true); err != nil {
		return err
	}
	a.value = value
	return nil
}

func (a openClawUSDAmount) MarshalJSON() ([]byte, error) {
	if _, err := service.NormalizeOpenClawUSDAmount(a.value, true); err != nil {
		return nil, err
	}
	return json.Marshal(struct {
		Currency    string `json:"currency"`
		MinorAmount string `json:"minorAmount"`
		Scale       int32  `json:"scale"`
	}{
		Currency:    openClawUSDCurrency,
		MinorAmount: a.value.Shift(service.OpenClawUSDAmountScale).StringFixed(0),
		Scale:       service.OpenClawUSDAmountScale,
	})
}

func openClawHTTPMinorAmountDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

// OpenClawBillingHandler exposes the narrow bridge API used by Suite. It never
// logs or returns platformUserId, and routes are isolated from browser/API-key
// authentication behind the bridge's own bearer middleware.
type OpenClawBillingHandler struct {
	service *service.OpenClawBillingService
}

func NewOpenClawBillingHandler(service *service.OpenClawBillingService) *OpenClawBillingHandler {
	return &OpenClawBillingHandler{service: service}
}

type openClawAccountResolveRequest struct {
	PlatformUserID string `json:"platformUserId"`
}

type openClawReserveLeaseRequest struct {
	PlatformUserID string            `json:"platformUserId"`
	TaskID         string            `json:"taskId"`
	SessionID      string            `json:"sessionId"`
	NodeID         string            `json:"nodeId"`
	AmountUSD      openClawUSDAmount `json:"amountUsd"`
	IdempotencyKey string            `json:"idempotencyKey"`
}

type openClawLeaseOperationRequest struct {
	PlatformUserID string            `json:"platformUserId"`
	AmountUSD      openClawUSDAmount `json:"amountUsd"`
	IdempotencyKey string            `json:"idempotencyKey"`
}

type openClawUsageEventsBatchRequest struct {
	Events []openClawUsageEventRequest `json:"events"`
}

// All token fields are pointers so `null` remains distinguishable from zero.
type openClawUsageEventRequest struct {
	EventID          string             `json:"eventId"`
	PlatformUserID   string             `json:"platformUserId"`
	TaskID           string             `json:"taskId"`
	SessionID        string             `json:"sessionId"`
	NodeID           string             `json:"nodeId"`
	CodexThreadID    *string            `json:"codexThreadId"`
	Model            string             `json:"model"`
	InputTokens      *int64             `json:"inputTokens"`
	OutputTokens     *int64             `json:"outputTokens"`
	CacheReadTokens  *int64             `json:"cacheReadTokens"`
	CacheWriteTokens *int64             `json:"cacheWriteTokens"`
	ObservedAt       time.Time          `json:"observedAt"`
	ReportedAt       time.Time          `json:"reportedAt"`
	PricingVersion   *string            `json:"pricingVersion"`
	PriceSnapshot    json.RawMessage    `json:"priceSnapshot"`
	EstimatedUSD     *openClawUSDAmount `json:"estimatedUsd"`
}

func (h *OpenClawBillingHandler) ResolveAccount(c *gin.Context) {
	var request openClawAccountResolveRequest
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	if h == nil || h.service == nil {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingDisabled)
		return
	}
	account, err := h.service.ResolveAccount(c.Request.Context(), request.PlatformUserID)
	if err != nil {
		writeOpenClawBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, openClawAccountResponse(account))
}

func (h *OpenClawBillingHandler) ReserveLease(c *gin.Context) {
	var request openClawReserveLeaseRequest
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	if h == nil || h.service == nil {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingDisabled)
		return
	}
	result, err := h.service.ReserveLease(c.Request.Context(), service.OpenClawReserveLeaseInput{
		PlatformUserID: request.PlatformUserID,
		TaskID:         request.TaskID,
		SessionID:      request.SessionID,
		NodeID:         request.NodeID,
		AmountUSD:      request.AmountUSD.value,
		IdempotencyKey: request.IdempotencyKey,
	})
	if err != nil {
		writeOpenClawBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, openClawLeaseResponse(result))
}

func (h *OpenClawBillingHandler) CaptureLease(c *gin.Context) {
	h.applyLeaseOperation(c, true)
}

func (h *OpenClawBillingHandler) ReleaseLease(c *gin.Context) {
	h.applyLeaseOperation(c, false)
}

func (h *OpenClawBillingHandler) applyLeaseOperation(c *gin.Context, capture bool) {
	var request openClawLeaseOperationRequest
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	if h == nil || h.service == nil {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingDisabled)
		return
	}
	input := service.OpenClawLeaseOperationInput{
		PlatformUserID: request.PlatformUserID,
		LeaseID:        c.Param("leaseId"),
		AmountUSD:      request.AmountUSD.value,
		IdempotencyKey: request.IdempotencyKey,
	}
	var (
		result *service.OpenClawLeaseResult
		err    error
	)
	if capture {
		result, err = h.service.CaptureLease(c.Request.Context(), input)
	} else {
		result, err = h.service.ReleaseLease(c.Request.Context(), input)
	}
	if err != nil {
		writeOpenClawBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, result)
}

// IngestUsageEvents implements Suite's equivalent of
// POST /v1/nodes/{nodeId}/usage-events:batch without accepting Suite's node
// credential, browser JWTs, admin keys, or gateway API keys.
func (h *OpenClawBillingHandler) IngestUsageEvents(c *gin.Context) {
	var request openClawUsageEventsBatchRequest
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	if h == nil || h.service == nil {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingDisabled)
		return
	}
	events := make([]service.OpenClawUsageEventInput, len(request.Events))
	for i := range request.Events {
		event := request.Events[i]
		events[i] = service.OpenClawUsageEventInput{
			EventID:          event.EventID,
			PlatformUserID:   event.PlatformUserID,
			TaskID:           event.TaskID,
			SessionID:        event.SessionID,
			NodeID:           event.NodeID,
			CodexThreadID:    event.CodexThreadID,
			Model:            event.Model,
			InputTokens:      event.InputTokens,
			OutputTokens:     event.OutputTokens,
			CacheReadTokens:  event.CacheReadTokens,
			CacheWriteTokens: event.CacheWriteTokens,
			ObservedAt:       event.ObservedAt,
			ReportedAt:       event.ReportedAt,
			PricingVersion:   event.PricingVersion,
			PriceSnapshot:    event.PriceSnapshot,
			EstimatedUSD:     openClawOptionalUSDAmount(event.EstimatedUSD),
		}
	}
	results, err := h.service.IngestUsageEvents(c.Request.Context(), events)
	if err != nil {
		writeOpenClawBillingError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"events": results})
}

func bindOpenClawBillingJSON(c *gin.Context, target any) bool {
	if c.Request == nil || c.Request.Body == nil {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingInvalid)
		return false
	}
	reader := http.MaxBytesReader(c.Writer, c.Request.Body, openClawBillingMaxRequestBodyBytes)
	defer reader.Close()
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingInvalid)
		return false
	}
	if strings.TrimSpace(c.GetHeader("Content-Type")) != "" && !strings.HasPrefix(strings.ToLower(c.GetHeader("Content-Type")), "application/json") {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingInvalid)
		return false
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingInvalid)
		return false
	}
	return true
}

func openClawAccountResponse(account *service.OpenClawBillingAccount) gin.H {
	if account == nil {
		return gin.H{}
	}
	return gin.H{
		"billingAccountId": account.BillingAccountID,
		"balanceUsd":       openClawUSDAmount{value: account.Balance},
		"frozenBalanceUsd": openClawUSDAmount{value: account.FrozenBalance},
		"initialGrantUsd":  openClawUSDAmount{value: account.InitialGrantUSD},
		"created":          account.Created,
	}
}

func openClawOptionalUSDAmount(amount *openClawUSDAmount) *decimal.Decimal {
	if amount == nil {
		return nil
	}
	value := amount.value
	return &value
}

func openClawLeaseResponse(result *service.OpenClawLeaseResult) gin.H {
	if result == nil {
		return gin.H{}
	}
	lease := result.Lease
	return gin.H{
		"applied": result.Applied,
		"lease": gin.H{
			"lease_id": lease.LeaseID, "billing_account_id": lease.BillingAccountID,
			"task_id": lease.TaskID, "session_id": lease.SessionID, "node_id": lease.NodeID,
			"reserved_usd": openClawUSDAmount{value: lease.ReservedUSD},
			"captured_usd": openClawUSDAmount{value: lease.CapturedUSD},
			"released_usd": openClawUSDAmount{value: lease.ReleasedUSD},
			"status":       lease.Status, "expires_at": lease.ExpiresAt, "created_at": lease.CreatedAt,
			"finalized_at": lease.FinalizedAt,
		},
		"balance_usd":        openClawUSDAmount{value: result.BalanceUSD},
		"frozen_balance_usd": openClawUSDAmount{value: result.FrozenBalance},
	}
}

func writeOpenClawBillingError(c *gin.Context, err error) {
	if err == nil {
		return
	}
	statusCode, status := infraerrors.ToHTTP(err)
	// Do not include request values, raw identity, bearer material, or server
	// detail that could become an identity/logging side channel.
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"code":    status.Reason,
			"message": status.Message,
		},
	})
}
