package service

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/shopspring/decimal"
)

var ErrOpenClawGatewayUnconfigured = infraerrors.New(http.StatusServiceUnavailable, "OPENCLAW_GATEWAY_UNCONFIGURED", "OpenClaw gateway group is not configured")
var ErrOpenClawUnsettled = infraerrors.New(http.StatusPaymentRequired, "OPENCLAW_UNSETTLED", "OpenClaw account has unsettled requests")

type OpenClawMoney struct {
	Currency    string `json:"currency"`
	MinorAmount string `json:"minorAmount"`
	Scale       int32  `json:"scale"`
}

func OpenClawMoneyFromDecimal(value decimal.Decimal) OpenClawMoney {
	return OpenClawMoney{Currency: "USD", MinorAmount: value.Shift(8).StringFixed(0), Scale: 8}
}

type OpenClawAccountView struct {
	BillingAccountID string        `json:"billingAccountId"`
	BalanceUSD       OpenClawMoney `json:"balanceUsd"`
	FrozenUSD        OpenClawMoney `json:"frozenUsd"`
	InitialGrantUSD  OpenClawMoney `json:"initialGrantUsd"`
}

func OpenClawAccountViewOf(a *OpenClawBillingAccount) OpenClawAccountView {
	return OpenClawAccountView{a.BillingAccountID, OpenClawMoneyFromDecimal(a.Balance), OpenClawMoneyFromDecimal(a.FrozenBalance), OpenClawMoneyFromDecimal(a.InitialGrantUSD)}
}

type OpenClawTaskIdentity struct {
	PlatformUserID string `json:"platformUserId"`
	TaskID         string `json:"taskId"`
	SessionID      string `json:"sessionId"`
	NodeID         string `json:"nodeId"`
}

type OpenClawTaskOpenInput struct {
	OpenClawTaskIdentity
	AllowedModels []string `json:"allowedModels"`
}

type OpenClawTaskOpenResult struct {
	LeaseID       string              `json:"leaseId"`
	ExpiresAt     time.Time           `json:"expiresAt"`
	GatewayAPIKey string              `json:"gatewayApiKey"`
	Account       OpenClawAccountView `json:"account"`
}

type OpenClawTaskCloseResult struct {
	LeaseID    *string        `json:"leaseId"`
	Status     string         `json:"status"`
	ChargedUSD *OpenClawMoney `json:"chargedUsd"`
}

type OpenClawGatewayRequest struct {
	LeaseID        string
	RequestID      string
	APIKeyID       int64
	UserID         int64
	Model          string
	Dispatched     bool
	UsageScheduled bool
	PricingKnown   bool
	ReportFailure  func(context.Context) error
}

type openClawRequestContextKey struct{}

func WithOpenClawGatewayRequest(ctx context.Context, request *OpenClawGatewayRequest) context.Context {
	return context.WithValue(ctx, openClawRequestContextKey{}, request)
}

func OpenClawGatewayRequestFromContext(ctx context.Context) *OpenClawGatewayRequest {
	if ctx == nil {
		return nil
	}
	request, _ := ctx.Value(openClawRequestContextKey{}).(*OpenClawGatewayRequest)
	return request
}

type OpenClawTaskOpenCommand struct {
	Account          OpenClawBillingAccount
	Identity         OpenClawTaskIdentity
	PlatformUserHMAC string
	LeaseID          string
	APIKeyID         int64
	AllowedModels    []string
	Subscription     bool
	ExpiresAt        time.Time
}

type OpenClawTaskSessionRepository interface {
	EnsureGatewayKey(context.Context, OpenClawBillingAccount, string, int64) (int64, string, error)
	GatewayGroup(context.Context, string) (int64, error)
	SetGatewayGroup(context.Context, string, int64) error
	OpenTaskSession(context.Context, OpenClawTaskOpenCommand) (*OpenClawTaskOpenResult, error)
	RenewTaskSession(context.Context, string, string, string, string, time.Time) (time.Time, error)
	CloseTaskSession(context.Context, string, string, string, string, string) (*OpenClawTaskCloseResult, error)
	CloseTaskSessionByTask(context.Context, string, OpenClawTaskIdentity, string) (*OpenClawTaskCloseResult, error)
	ManagedGatewayAccount(context.Context, int64) (*OpenClawBillingAccount, error)
	BeginGatewayRequest(context.Context, *OpenClawGatewayRequest) error
	FinishGatewayRequest(context.Context, *OpenClawGatewayRequest, bool) error
	Transactions(context.Context, string) ([]map[string]any, error)
	AdjustBalance(context.Context, OpenClawBillingAccount, string, decimal.Decimal, string, string) (*OpenClawBillingAccount, error)
	FindSubscriptionAssignment(context.Context, string, string, int64, int, string) (*int64, error)
	RecordSubscriptionAssignment(context.Context, string, string, int64, int, string, int64) error
	ExpireTaskSessions(context.Context, int) (int, error)
}

type OpenClawTaskSessionService struct {
	repository    OpenClawTaskSessionRepository
	billing       *OpenClawBillingService
	apiKeys       *APIKeyService
	subscriptions *SubscriptionService
	groups        GroupRepository
	settings      SettingRepository
	cfg           *config.Config
}

func NewOpenClawTaskSessionService(repo OpenClawTaskSessionRepository, billing *OpenClawBillingService, keys *APIKeyService, subscriptions *SubscriptionService, groups GroupRepository, settings SettingRepository, cfg *config.Config) *OpenClawTaskSessionService {
	billing.settings = settings
	return &OpenClawTaskSessionService{repo, billing, keys, subscriptions, groups, settings, cfg}
}

func (s *OpenClawTaskSessionService) Open(ctx context.Context, input OpenClawTaskOpenInput) (*OpenClawTaskOpenResult, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	if s.cfg.OpenClawBilling.GatewayGroupID <= 0 {
		return nil, ErrOpenClawGatewayUnconfigured
	}
	if err := validateOpenClawTaskIdentity(input.OpenClawTaskIdentity, true); err != nil {
		return nil, err
	}
	if len(input.AllowedModels) == 0 || len(input.AllowedModels) > 100 {
		return nil, ErrOpenClawBillingInvalid
	}
	for _, model := range input.AllowedModels {
		if model == "" || strings.TrimSpace(model) != model || len(model) > 200 {
			return nil, ErrOpenClawBillingInvalid
		}
	}
	account, err := s.billing.ResolveAccount(ctx, input.PlatformUserID)
	if err != nil {
		return nil, err
	}
	groupID, err := s.repository.GatewayGroup(ctx, account.BillingAccountID)
	if err != nil {
		return nil, err
	}
	if groupID == 0 {
		groupID = s.cfg.OpenClawBilling.GatewayGroupID
	}
	group, err := s.groups.GetByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if group.Status != StatusActive || group.Platform != PlatformOpenAI {
		return nil, ErrOpenClawGatewayUnconfigured
	}
	if group.IsSubscriptionType() {
		subscription, err := s.subscriptions.GetActiveSubscription(ctx, account.BillingUserID, groupID)
		if err != nil {
			return nil, err
		}
		needsMaintenance, err := s.subscriptions.ValidateAndCheckLimits(subscription, group)
		if needsMaintenance {
			subscription, err = s.subscriptions.EnsureWindowMaintenance(ctx, subscription)
			if err != nil {
				return nil, err
			}
			_, err = s.subscriptions.ValidateAndCheckLimits(subscription, group)
		}
		if err != nil {
			return nil, err
		}
	}
	key, err := s.apiKeys.GenerateKey()
	if err != nil {
		return nil, err
	}
	keyID, key, err := s.repository.EnsureGatewayKey(ctx, *account, key, groupID)
	if err != nil {
		return nil, err
	}
	s.apiKeys.InvalidateAuthCacheByKey(ctx, key)
	result, err := s.repository.OpenTaskSession(ctx, OpenClawTaskOpenCommand{
		Account: *account, Identity: input.OpenClawTaskIdentity,
		PlatformUserHMAC: hashOpenClawPlatformUserID(s.billing.config.IdentityHMACKey, input.PlatformUserID),
		LeaseID:          newOpenClawOpaqueID("octs"), APIKeyID: keyID, AllowedModels: input.AllowedModels,
		Subscription: group.IsSubscriptionType(), ExpiresAt: time.Now().UTC().Add(time.Duration(s.billing.config.LeaseTTLSeconds) * time.Second),
	})
	if err != nil {
		return nil, err
	}
	result.GatewayAPIKey = key
	return result, nil
}

func validateOpenClawTaskIdentity(input OpenClawTaskIdentity, sessionRequired bool) error {
	values := []string{input.PlatformUserID, input.TaskID, input.NodeID}
	if sessionRequired {
		values = append(values, input.SessionID)
	}
	for _, value := range values {
		if err := validateOpenClawRequired(value, "identity"); err != nil {
			return err
		}
	}
	return nil
}

func (s *OpenClawTaskSessionService) Renew(ctx context.Context, leaseID string, input OpenClawTaskIdentity) (time.Time, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return time.Time{}, err
	}
	if err := validateOpenClawTaskIdentity(input, false); err != nil {
		return time.Time{}, err
	}
	account, err := s.billing.findAccount(ctx, input.PlatformUserID)
	if err != nil {
		return time.Time{}, err
	}
	return s.repository.RenewTaskSession(ctx, account.BillingAccountID, leaseID, input.TaskID, input.NodeID, time.Now().UTC().Add(time.Duration(s.billing.config.LeaseTTLSeconds)*time.Second))
}

func (s *OpenClawTaskSessionService) Close(ctx context.Context, leaseID string, input OpenClawTaskIdentity, idempotencyKey string) (*OpenClawTaskCloseResult, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	if err := validateOpenClawTaskIdentity(input, leaseID == ""); err != nil {
		return nil, err
	}
	if err := validateOpenClawRequired(idempotencyKey, "idempotency key"); err != nil {
		return nil, err
	}
	if leaseID == "" {
		return s.repository.CloseTaskSessionByTask(ctx, hashOpenClawPlatformUserID(s.billing.config.IdentityHMACKey, input.PlatformUserID), input, idempotencyKey)
	}
	account, err := s.billing.findAccount(ctx, input.PlatformUserID)
	if err != nil {
		return nil, err
	}
	return s.repository.CloseTaskSession(ctx, account.BillingAccountID, leaseID, input.TaskID, input.NodeID, idempotencyKey)
}

func (s *OpenClawTaskSessionService) ManagedAccount(ctx context.Context, userID int64) (*OpenClawBillingAccount, error) {
	return s.repository.ManagedGatewayAccount(ctx, userID)
}

func (s *OpenClawTaskSessionService) BeginRequest(ctx context.Context, request *OpenClawGatewayRequest) error {
	if err := s.billing.requireEnabled(); err != nil {
		return err
	}
	if request.LeaseID == "" || request.Model == "" || len(request.LeaseID) > 255 || len(request.Model) > 200 {
		return ErrOpenClawBillingInvalid
	}
	return s.repository.BeginGatewayRequest(ctx, request)
}

func (s *OpenClawTaskSessionService) FinishRequest(ctx context.Context, request *OpenClawGatewayRequest) error {
	if request.UsageScheduled {
		return nil
	}
	return s.repository.FinishGatewayRequest(ctx, request, request.Dispatched)
}

func (s *OpenClawTaskSessionService) RecordUnknownRequest(ctx context.Context, request *OpenClawGatewayRequest) error {
	return s.repository.FinishGatewayRequest(ctx, request, true)
}
func (s *OpenClawTaskSessionService) Expire(ctx context.Context) (int, error) {
	return s.repository.ExpireTaskSessions(ctx, s.billing.config.ExpiryBatchSize)
}

const openClawInitialGrantSetting = "openclaw_billing_initial_grant_minor"

func readOpenClawInitialGrant(ctx context.Context, settings SettingRepository, fallback float64) (decimal.Decimal, error) {
	if settings == nil {
		return openClawUSDFromConfig(fallback), nil
	}
	value, err := settings.GetValue(ctx, openClawInitialGrantSetting)
	if errors.Is(err, ErrSettingNotFound) {
		return openClawUSDFromConfig(fallback), nil
	}
	if err != nil {
		return decimal.Zero, err
	}
	minor, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Zero, ErrOpenClawBillingInvalid
	}
	return NormalizeOpenClawUSDAmount(minor.Shift(-8), true)
}
