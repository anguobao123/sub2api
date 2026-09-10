package handler

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

func ProvideOpenClawBillingHandler(billing *service.OpenClawBillingService, sessions *service.OpenClawTaskSessionService) *OpenClawBillingHandler {
	h := NewOpenClawBillingHandler(billing)
	h.sessions = sessions
	return h
}

func openClawData(c *gin.Context, data any, err error) {
	if err != nil {
		writeOpenClawBillingError(c, err)
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"data": data})
}

func (h *OpenClawBillingHandler) AccountOverview(c *gin.Context) {
	var request openClawAccountResolveRequest
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	data, err := h.sessions.Overview(c.Request.Context(), request.PlatformUserID)
	openClawData(c, data, err)
}
func (h *OpenClawBillingHandler) AdjustBalance(c *gin.Context) {
	var request struct {
		PlatformUserID string            `json:"platformUserId"`
		Operation      string            `json:"operation"`
		AmountUSD      openClawUSDAmount `json:"amountUsd"`
		IdempotencyKey string            `json:"idempotencyKey"`
		Reason         string            `json:"reason"`
	}
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	if !request.AmountUSD.present {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingInvalid)
		return
	}
	a, err := h.sessions.AdjustBalance(c.Request.Context(), request.PlatformUserID, request.Operation, request.AmountUSD.value, request.IdempotencyKey, request.Reason)
	if err != nil {
		openClawData(c, nil, err)
		return
	}
	openClawData(c, service.OpenClawAccountViewOf(a), nil)
}
func (h *OpenClawBillingHandler) GetBillingSettings(c *gin.Context) {
	data, err := h.sessions.GetSettings(c.Request.Context())
	openClawData(c, data, err)
}
func (h *OpenClawBillingHandler) SetBillingSettings(c *gin.Context) {
	var request struct {
		InitialGrantUSD openClawUSDAmount `json:"initialGrantUsd"`
	}
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	if !request.InitialGrantUSD.present {
		writeOpenClawBillingError(c, service.ErrOpenClawBillingInvalid)
		return
	}
	data, err := h.sessions.SetSettings(c.Request.Context(), request.InitialGrantUSD.value)
	openClawData(c, data, err)
}
func (h *OpenClawBillingHandler) SubscriptionOptions(c *gin.Context) {
	var request struct{}
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	data, err := h.sessions.SubscriptionOptions(c.Request.Context())
	openClawData(c, data, err)
}
func (h *OpenClawBillingHandler) AssignSubscription(c *gin.Context) {
	var request struct {
		PlatformUserID string `json:"platformUserId"`
		GroupID        int64  `json:"groupId"`
		Days           int    `json:"days"`
		IdempotencyKey string `json:"idempotencyKey"`
		Reason         string `json:"reason"`
	}
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	data, err := h.sessions.AssignSubscription(c.Request.Context(), request.PlatformUserID, request.GroupID, request.Days, request.IdempotencyKey, request.Reason)
	openClawData(c, data, err)
}
func (h *OpenClawBillingHandler) OpenTaskSession(c *gin.Context) {
	var request service.OpenClawTaskOpenInput
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	data, err := h.sessions.Open(c.Request.Context(), request)
	openClawData(c, data, err)
}
func (h *OpenClawBillingHandler) RenewTaskSession(c *gin.Context) {
	var request service.OpenClawTaskIdentity
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	expires, err := h.sessions.Renew(c.Request.Context(), c.Param("leaseId"), request)
	openClawData(c, gin.H{"leaseId": c.Param("leaseId"), "expiresAt": expires}, err)
}
func (h *OpenClawBillingHandler) CloseTaskSession(c *gin.Context) {
	var request struct {
		service.OpenClawTaskIdentity
		IdempotencyKey string `json:"idempotencyKey"`
	}
	if !bindOpenClawBillingJSON(c, &request) {
		return
	}
	data, err := h.sessions.Close(c.Request.Context(), c.Param("leaseId"), request.OpenClawTaskIdentity, request.IdempotencyKey)
	openClawData(c, data, err)
}
