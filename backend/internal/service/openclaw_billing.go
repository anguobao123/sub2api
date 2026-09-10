package service

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

const (
	OpenClawLeaseStatusActive   = "active"
	OpenClawLeaseStatusCaptured = "captured"
	OpenClawLeaseStatusReleased = "released"
	OpenClawLeaseStatusExpired  = "expired"

	OpenClawUsageStatusPendingPricing = "pending_pricing"
	OpenClawUsageStatusRecorded       = "recorded"
	openClawOpaqueIDMaxLength         = 255
	openClawPriceSnapshotMaxBytes     = 64 * 1024
	// DECIMAL(20,8) leaves at most twelve whole-dollar digits. Keep the
	// application bound conservative so values are rejected before PostgreSQL.
	OpenClawUSDAmountScale int32 = 8
)

var openClawMaxUSD = decimal.NewFromInt(999_999_999_999)

var (
	ErrOpenClawBillingDisabled     = infraerrors.New(http.StatusNotFound, "OPENCLAW_BILLING_DISABLED", "openclaw billing bridge is disabled")
	ErrOpenClawBillingInvalid      = infraerrors.New(http.StatusBadRequest, "OPENCLAW_BILLING_INVALID_REQUEST", "invalid openclaw billing request")
	ErrOpenClawInsufficientBalance = infraerrors.New(http.StatusPaymentRequired, "OPENCLAW_INSUFFICIENT_BALANCE", "insufficient balance for openclaw budget reservation")
	ErrOpenClawAccountNotFound     = infraerrors.New(http.StatusNotFound, "OPENCLAW_BILLING_ACCOUNT_NOT_FOUND", "openclaw billing account not found")
	ErrOpenClawLeaseNotFound       = infraerrors.New(http.StatusNotFound, "OPENCLAW_BUDGET_LEASE_NOT_FOUND", "openclaw budget lease not found")
	ErrOpenClawLeaseExpired        = infraerrors.New(http.StatusConflict, "OPENCLAW_BUDGET_LEASE_EXPIRED", "openclaw budget lease has expired")
	ErrOpenClawLeaseFinalized      = infraerrors.New(http.StatusConflict, "OPENCLAW_BUDGET_LEASE_FINALIZED", "openclaw budget lease is already finalized")
	ErrOpenClawLeaseActive         = infraerrors.New(http.StatusConflict, "OPENCLAW_BUDGET_LEASE_ACTIVE", "an active openclaw budget lease already exists for this task")
	ErrOpenClawBudgetExceeded      = infraerrors.New(http.StatusConflict, "OPENCLAW_BUDGET_EXCEEDED", "openclaw budget lease amount would be exceeded")
	ErrOpenClawIdempotencyConflict = infraerrors.New(http.StatusConflict, "OPENCLAW_IDEMPOTENCY_CONFLICT", "openclaw idempotency key was reused with a different request")
	ErrOpenClawUsageEventConflict  = infraerrors.New(http.StatusConflict, "OPENCLAW_USAGE_EVENT_CONFLICT", "openclaw usage event id was reused with different content")
)

// OpenClawBillingAccount is an internal account bound to a HMAC-derived
// platform identity locator. BillingUserID is intentionally never serialized
// by the HTTP layer.
type OpenClawBillingAccount struct {
	BillingAccountID string          `json:"billing_account_id"`
	BillingUserID    int64           `json:"-"`
	Balance          decimal.Decimal `json:"-"`
	FrozenBalance    decimal.Decimal `json:"-"`
	InitialGrantUSD  decimal.Decimal `json:"-"`
	Created          bool            `json:"created"`
}

// OpenClawAccountProvision is the only mapping command accepted by the
// persistence layer. PlatformUserHMAC is irreversible without the server key;
// raw platform user IDs are intentionally absent.
type OpenClawAccountProvision struct {
	PlatformUserHMAC string
	BillingAccountID string
	SyntheticEmail   string
	PasswordHash     string
	DefaultGrantUSD  decimal.Decimal
}

// OpenClawBudgetLease is a persisted task-budget hold.
type OpenClawBudgetLease struct {
	LeaseID          string          `json:"lease_id"`
	BillingAccountID string          `json:"billing_account_id"`
	BillingUserID    int64           `json:"-"`
	TaskID           string          `json:"task_id"`
	SessionID        string          `json:"session_id"`
	NodeID           string          `json:"node_id"`
	ReservedUSD      decimal.Decimal `json:"-"`
	CapturedUSD      decimal.Decimal `json:"-"`
	ReleasedUSD      decimal.Decimal `json:"-"`
	Status           string          `json:"status"`
	ExpiresAt        time.Time       `json:"expires_at"`
	CreatedAt        time.Time       `json:"created_at"`
	FinalizedAt      *time.Time      `json:"finalized_at,omitempty"`
}

// OpenClawReserveLeaseInput is accepted only by the application service. The
// raw platform ID is HMACed before the repository is called.
type OpenClawReserveLeaseInput struct {
	PlatformUserID string
	TaskID         string
	SessionID      string
	NodeID         string
	AmountUSD      decimal.Decimal
	IdempotencyKey string
}

type OpenClawReserveLeaseCommand struct {
	Account            OpenClawBillingAccount
	LeaseID            string
	TaskID             string
	SessionID          string
	NodeID             string
	AmountUSD          decimal.Decimal
	IdempotencyKey     string
	RequestFingerprint string
	ExpiresAt          time.Time
}

type OpenClawLeaseOperationInput struct {
	PlatformUserID string
	LeaseID        string
	AmountUSD      decimal.Decimal
	IdempotencyKey string
}

type OpenClawLeaseOperationCommand struct {
	Account            OpenClawBillingAccount
	LeaseID            string
	AmountUSD          decimal.Decimal
	IdempotencyKey     string
	RequestFingerprint string
}

type OpenClawLeaseResult struct {
	Applied       bool                `json:"applied"`
	Lease         OpenClawBudgetLease `json:"lease"`
	BalanceUSD    decimal.Decimal     `json:"-"`
	FrozenBalance decimal.Decimal     `json:"-"`
}

// OpenClawUsageEventInput mirrors Suite's internal batch contract. The raw
// platform ID remains in this in-memory input only.
type OpenClawUsageEventInput struct {
	EventID          string
	PlatformUserID   string
	TaskID           string
	SessionID        string
	NodeID           string
	CodexThreadID    *string
	Model            string
	InputTokens      *int64
	OutputTokens     *int64
	CacheReadTokens  *int64
	CacheWriteTokens *int64
	ObservedAt       time.Time
	ReportedAt       time.Time
	PricingVersion   *string
	PriceSnapshot    json.RawMessage
	EstimatedUSD     *decimal.Decimal
}

// OpenClawUsageEventCommand contains only internal-account and billable event
// fields, making it impossible for a repository implementation to persist the
// raw external platform user identity by accident.
type OpenClawUsageEventCommand struct {
	Account           OpenClawBillingAccount
	EventID           string
	TaskID            string
	SessionID         string
	NodeID            string
	CodexThreadID     *string
	Model             string
	InputTokens       *int64
	OutputTokens      *int64
	CacheReadTokens   *int64
	CacheWriteTokens  *int64
	ObservedAt        time.Time
	ReportedAt        time.Time
	PricingVersion    *string
	PriceSnapshot     json.RawMessage
	EstimatedUSD      *decimal.Decimal
	PricingComplete   bool
	IngestFingerprint string
}

type OpenClawUsageEventResult struct {
	EventID string `json:"event_id"`
	Applied bool   `json:"applied"`
	Status  string `json:"status"`
}

// OpenClawBillingRepository defines the bounded PostgreSQL surface needed by
// the bridge. Its commands contain no raw platform identity or credentials.
type OpenClawBillingRepository interface {
	EnsureAccount(ctx context.Context, provision OpenClawAccountProvision) (*OpenClawBillingAccount, error)
	FindAccount(ctx context.Context, platformUserHMAC string) (*OpenClawBillingAccount, error)
	ReserveLease(ctx context.Context, command OpenClawReserveLeaseCommand) (*OpenClawLeaseResult, error)
	CaptureLease(ctx context.Context, command OpenClawLeaseOperationCommand) (*OpenClawLeaseResult, error)
	ReleaseLease(ctx context.Context, command OpenClawLeaseOperationCommand) (*OpenClawLeaseResult, error)
	ExpireLeases(ctx context.Context, limit int) (int, error)
	IngestUsageEvent(ctx context.Context, command OpenClawUsageEventCommand) (*OpenClawUsageEventResult, error)
}

type OpenClawBillingService struct {
	repository OpenClawBillingRepository
	config     config.OpenClawBillingConfig
	now        func() time.Time
}

func NewOpenClawBillingService(repository OpenClawBillingRepository, cfg *config.Config) *OpenClawBillingService {
	bridgeConfig := config.OpenClawBillingConfig{}
	if cfg != nil {
		bridgeConfig = cfg.OpenClawBilling
	}
	return &OpenClawBillingService{
		repository: repository,
		config:     bridgeConfig,
		now:        time.Now,
	}
}

func (s *OpenClawBillingService) ResolveAccount(ctx context.Context, platformUserID string) (*OpenClawBillingAccount, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	if err := validateOpenClawRequired(platformUserID, "platform user id"); err != nil {
		return nil, err
	}
	if s.repository == nil {
		return nil, errors.New("openclaw billing repository is nil")
	}

	billingAccountID := newOpenClawOpaqueID("ocba")
	provision := OpenClawAccountProvision{
		PlatformUserHMAC: hashOpenClawPlatformUserID(s.config.IdentityHMACKey, platformUserID),
		BillingAccountID: billingAccountID,
		SyntheticEmail:   "openclaw-" + billingAccountID + "@bridge.invalid",
		PasswordHash:     "!openclaw-internal-login-disabled!",
		DefaultGrantUSD:  openClawUSDFromConfig(s.config.DefaultGrantUSD),
	}
	return s.repository.EnsureAccount(ctx, provision)
}

func (s *OpenClawBillingService) ReserveLease(ctx context.Context, input OpenClawReserveLeaseInput) (*OpenClawLeaseResult, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	if err := validateReserveLeaseInput(input); err != nil {
		return nil, err
	}
	// Avoid duplicate grants and balance movement when the caller retries a
	// stable reservation key. The repository still owns the authoritative SQL
	// uniqueness check; this lookup makes retries deterministic before account
	// provisioning.
	if account, found, err := s.findAccountIfPresent(ctx, input.PlatformUserID); err != nil {
		return nil, err
	} else if found {
		return s.reserveLeaseForAccount(ctx, *account, input)
	}
	account, err := s.ResolveAccount(ctx, input.PlatformUserID)
	if err != nil {
		return nil, err
	}
	return s.reserveLeaseForAccount(ctx, *account, input)
}

func (s *OpenClawBillingService) reserveLeaseForAccount(ctx context.Context, account OpenClawBillingAccount, input OpenClawReserveLeaseInput) (*OpenClawLeaseResult, error) {
	if err := s.expireLeases(ctx); err != nil {
		return nil, err
	}

	command := OpenClawReserveLeaseCommand{
		Account:        account,
		LeaseID:        newOpenClawOpaqueID("ocbl"),
		TaskID:         strings.TrimSpace(input.TaskID),
		SessionID:      strings.TrimSpace(input.SessionID),
		NodeID:         strings.TrimSpace(input.NodeID),
		AmountUSD:      input.AmountUSD,
		IdempotencyKey: strings.TrimSpace(input.IdempotencyKey),
		ExpiresAt:      s.now().UTC().Add(time.Duration(s.config.LeaseTTLSeconds) * time.Second),
	}
	command.RequestFingerprint = openClawReserveFingerprint(command)
	return s.repository.ReserveLease(ctx, command)
}

func (s *OpenClawBillingService) CaptureLease(ctx context.Context, input OpenClawLeaseOperationInput) (*OpenClawLeaseResult, error) {
	return s.applyLeaseOperation(ctx, input, "capture")
}

func (s *OpenClawBillingService) ReleaseLease(ctx context.Context, input OpenClawLeaseOperationInput) (*OpenClawLeaseResult, error) {
	return s.applyLeaseOperation(ctx, input, "release")
}

func (s *OpenClawBillingService) applyLeaseOperation(ctx context.Context, input OpenClawLeaseOperationInput, operation string) (*OpenClawLeaseResult, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	if err := validateLeaseOperationInput(input); err != nil {
		return nil, err
	}
	account, err := s.findAccount(ctx, input.PlatformUserID)
	if err != nil {
		return nil, err
	}
	if err := s.expireLeases(ctx); err != nil {
		return nil, err
	}
	command := OpenClawLeaseOperationCommand{
		Account:        *account,
		LeaseID:        strings.TrimSpace(input.LeaseID),
		AmountUSD:      input.AmountUSD,
		IdempotencyKey: strings.TrimSpace(input.IdempotencyKey),
	}
	command.RequestFingerprint = openClawLeaseOperationFingerprint(operation, command)
	if operation == "capture" {
		return s.repository.CaptureLease(ctx, command)
	}
	return s.repository.ReleaseLease(ctx, command)
}

func (s *OpenClawBillingService) ExpireLeases(ctx context.Context) (int, error) {
	if err := s.requireEnabled(); err != nil {
		return 0, err
	}
	if s.repository == nil {
		return 0, errors.New("openclaw billing repository is nil")
	}
	return s.repository.ExpireLeases(ctx, s.config.ExpiryBatchSize)
}

func (s *OpenClawBillingService) IngestUsageEvents(ctx context.Context, events []OpenClawUsageEventInput) ([]OpenClawUsageEventResult, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	if len(events) == 0 || len(events) > s.config.MaxUsageEventsPerBatch {
		return nil, ErrOpenClawBillingInvalid
	}
	if s.repository == nil {
		return nil, errors.New("openclaw billing repository is nil")
	}

	prepared := make([]OpenClawUsageEventInput, len(events))
	eventIDs := make(map[string]struct{}, len(events))
	for i := range events {
		item, err := prepareOpenClawUsageEvent(events[i])
		if err != nil {
			return nil, err
		}
		if _, exists := eventIDs[item.EventID]; exists {
			return nil, ErrOpenClawBillingInvalid
		}
		eventIDs[item.EventID] = struct{}{}
		prepared[i] = item
	}
	if err := s.expireLeases(ctx); err != nil {
		return nil, err
	}

	results := make([]OpenClawUsageEventResult, 0, len(prepared))
	for i := range prepared {
		account, err := s.ResolveAccount(ctx, prepared[i].PlatformUserID)
		if err != nil {
			return nil, err
		}
		command := newOpenClawUsageEventCommand(*account, prepared[i])
		result, err := s.repository.IngestUsageEvent(ctx, command)
		if err != nil {
			return nil, err
		}
		results = append(results, *result)
	}
	return results, nil
}

func (s *OpenClawBillingService) requireEnabled() error {
	if s == nil || !s.config.Enabled {
		return ErrOpenClawBillingDisabled
	}
	if len(s.config.InternalBearer) < 32 || len(s.config.IdentityHMACKey) < 32 {
		return ErrOpenClawBillingDisabled
	}
	return nil
}

func (s *OpenClawBillingService) expireLeases(ctx context.Context) error {
	if s.repository == nil {
		return errors.New("openclaw billing repository is nil")
	}
	_, err := s.repository.ExpireLeases(ctx, s.config.ExpiryBatchSize)
	return err
}

// findAccount deliberately does not provision a mapping. Capture and release
// must only settle a prior successful reservation, never create a $50 grant as
// a side effect of a malformed or replayed finalization request.
func (s *OpenClawBillingService) findAccount(ctx context.Context, platformUserID string) (*OpenClawBillingAccount, error) {
	if err := s.requireEnabled(); err != nil {
		return nil, err
	}
	if err := validateOpenClawRequired(platformUserID, "platform user id"); err != nil {
		return nil, err
	}
	if s.repository == nil {
		return nil, errors.New("openclaw billing repository is nil")
	}
	return s.repository.FindAccount(ctx, hashOpenClawPlatformUserID(s.config.IdentityHMACKey, platformUserID))
}

func (s *OpenClawBillingService) findAccountIfPresent(ctx context.Context, platformUserID string) (*OpenClawBillingAccount, bool, error) {
	account, err := s.findAccount(ctx, platformUserID)
	if errors.Is(err, ErrOpenClawAccountNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return account, true, nil
}

func prepareOpenClawUsageEvent(input OpenClawUsageEventInput) (OpenClawUsageEventInput, error) {
	input.EventID = strings.TrimSpace(input.EventID)
	input.PlatformUserID = strings.TrimSpace(input.PlatformUserID)
	input.TaskID = strings.TrimSpace(input.TaskID)
	input.SessionID = strings.TrimSpace(input.SessionID)
	input.NodeID = strings.TrimSpace(input.NodeID)
	input.Model = strings.TrimSpace(input.Model)
	if input.CodexThreadID != nil {
		value := strings.TrimSpace(*input.CodexThreadID)
		if value == "" {
			input.CodexThreadID = nil
		} else {
			input.CodexThreadID = &value
		}
	}
	if input.PricingVersion != nil {
		value := strings.TrimSpace(*input.PricingVersion)
		if value == "" {
			input.PricingVersion = nil
		} else {
			input.PricingVersion = &value
		}
	}

	for _, value := range []struct {
		value string
		name  string
	}{
		{input.EventID, "event id"},
		{input.PlatformUserID, "platform user id"},
		{input.TaskID, "task id"},
		{input.SessionID, "session id"},
		{input.NodeID, "node id"},
		{input.Model, "model"},
	} {
		if err := validateOpenClawRequired(value.value, value.name); err != nil {
			return OpenClawUsageEventInput{}, err
		}
	}
	if input.CodexThreadID != nil {
		if err := validateOpenClawOptional(*input.CodexThreadID); err != nil {
			return OpenClawUsageEventInput{}, err
		}
	}
	if input.PricingVersion != nil {
		if err := validateOpenClawOptional(*input.PricingVersion); err != nil {
			return OpenClawUsageEventInput{}, err
		}
	}
	if !validOpenClawTokenCount(input.InputTokens) ||
		!validOpenClawTokenCount(input.OutputTokens) ||
		!validOpenClawTokenCount(input.CacheReadTokens) ||
		!validOpenClawTokenCount(input.CacheWriteTokens) ||
		input.ObservedAt.IsZero() || input.ReportedAt.IsZero() {
		return OpenClawUsageEventInput{}, ErrOpenClawBillingInvalid
	}
	if input.EstimatedUSD != nil {
		amount, err := normalizeOpenClawUSD(*input.EstimatedUSD, true)
		if err != nil {
			return OpenClawUsageEventInput{}, err
		}
		input.EstimatedUSD = &amount
	}
	// json.RawMessage preserves an explicit JSON null as the bytes "null".
	// The bridge contract makes priceSnapshot nullable, so normalize that value
	// to absence before deciding whether the event has complete pricing.
	input.PriceSnapshot = bytes.TrimSpace(input.PriceSnapshot)
	if bytes.Equal(input.PriceSnapshot, []byte("null")) {
		input.PriceSnapshot = nil
	}
	if len(input.PriceSnapshot) > 0 {
		canonical, err := canonicalOpenClawPriceSnapshot(input.PriceSnapshot)
		if err != nil {
			return OpenClawUsageEventInput{}, err
		}
		input.PriceSnapshot = canonical
	}
	input.ObservedAt = input.ObservedAt.UTC()
	input.ReportedAt = input.ReportedAt.UTC()
	return input, nil
}

func newOpenClawUsageEventCommand(account OpenClawBillingAccount, input OpenClawUsageEventInput) OpenClawUsageEventCommand {
	command := OpenClawUsageEventCommand{
		Account:          account,
		EventID:          input.EventID,
		TaskID:           input.TaskID,
		SessionID:        input.SessionID,
		NodeID:           input.NodeID,
		CodexThreadID:    input.CodexThreadID,
		Model:            input.Model,
		InputTokens:      input.InputTokens,
		OutputTokens:     input.OutputTokens,
		CacheReadTokens:  input.CacheReadTokens,
		CacheWriteTokens: input.CacheWriteTokens,
		ObservedAt:       input.ObservedAt,
		ReportedAt:       input.ReportedAt,
		PricingVersion:   input.PricingVersion,
		PriceSnapshot:    input.PriceSnapshot,
		EstimatedUSD:     input.EstimatedUSD,
		PricingComplete:  input.PricingVersion != nil && len(input.PriceSnapshot) > 0,
	}
	command.IngestFingerprint = openClawUsageEventFingerprint(command)
	return command
}

func validateReserveLeaseInput(input OpenClawReserveLeaseInput) error {
	for _, value := range []struct {
		value string
		name  string
	}{
		{input.PlatformUserID, "platform user id"},
		{input.TaskID, "task id"},
		{input.SessionID, "session id"},
		{input.NodeID, "node id"},
		{input.IdempotencyKey, "idempotency key"},
	} {
		if err := validateOpenClawRequired(value.value, value.name); err != nil {
			return err
		}
	}
	_, err := normalizeOpenClawUSD(input.AmountUSD, false)
	return err
}

func validateLeaseOperationInput(input OpenClawLeaseOperationInput) error {
	for _, value := range []struct {
		value string
		name  string
	}{
		{input.PlatformUserID, "platform user id"},
		{input.LeaseID, "lease id"},
		{input.IdempotencyKey, "idempotency key"},
	} {
		if err := validateOpenClawRequired(value.value, value.name); err != nil {
			return err
		}
	}
	_, err := normalizeOpenClawUSD(input.AmountUSD, false)
	return err
}

func validateOpenClawRequired(value, _ string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > openClawOpaqueIDMaxLength {
		return ErrOpenClawBillingInvalid
	}
	return nil
}

func validateOpenClawOptional(value string) error {
	if len(value) > openClawOpaqueIDMaxLength {
		return ErrOpenClawBillingInvalid
	}
	return nil
}

func validOpenClawTokenCount(value *int64) bool {
	return value == nil || *value >= 0
}

func normalizeOpenClawUSD(value decimal.Decimal, allowZero bool) (decimal.Decimal, error) {
	if value.IsNegative() || value.GreaterThan(openClawMaxUSD) || (!allowZero && value.IsZero()) {
		return decimal.Zero, ErrOpenClawBillingInvalid
	}
	if value.Exponent() < -OpenClawUSDAmountScale {
		return decimal.Zero, ErrOpenClawBillingInvalid
	}
	return value, nil
}

func openClawUSDFromConfig(value float64) decimal.Decimal {
	return decimal.NewFromFloat(value).Round(OpenClawUSDAmountScale)
}

func canonicalOpenClawPriceSnapshot(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || len(raw) > openClawPriceSnapshotMaxBytes || !json.Valid(raw) {
		return nil, ErrOpenClawBillingInvalid
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, ErrOpenClawBillingInvalid
	}
	if _, ok := decoded.(map[string]any); !ok {
		return nil, ErrOpenClawBillingInvalid
	}
	if !validOpenClawPriceSnapshotValue(decoded) {
		return nil, ErrOpenClawBillingInvalid
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || len(canonical) > openClawPriceSnapshotMaxBytes {
		return nil, ErrOpenClawBillingInvalid
	}
	return canonical, nil
}

// validOpenClawPriceSnapshotValue allows integer scale only inside the same
// exact USD amount object used by the bridge. Any other JSON number would be
// an ambiguous float-capable price representation and is rejected.
func validOpenClawPriceSnapshotValue(value any) bool {
	switch value := value.(type) {
	case json.Number:
		return false
	case map[string]any:
		if _, hasCurrency := value["currency"]; hasCurrency {
			return validOpenClawPriceSnapshotAmount(value)
		}
		for _, child := range value {
			if !validOpenClawPriceSnapshotValue(child) {
				return false
			}
		}
	case []any:
		for _, child := range value {
			if !validOpenClawPriceSnapshotValue(child) {
				return false
			}
		}
	}
	return true
}

func validOpenClawPriceSnapshotAmount(value map[string]any) bool {
	if len(value) != 3 {
		return false
	}
	currency, currencyOK := value["currency"].(string)
	minorAmount, minorAmountOK := value["minorAmount"].(string)
	scale, scaleOK := value["scale"].(json.Number)
	if !currencyOK || !minorAmountOK || !scaleOK || currency != "USD" || !openClawMinorAmountDigits(minorAmount) {
		return false
	}
	scaleValue, err := scale.Int64()
	return err == nil && scaleValue == int64(OpenClawUSDAmountScale)
}

func openClawMinorAmountDigits(value string) bool {
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

func hashOpenClawPlatformUserID(secret, platformUserID string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(strings.TrimSpace(platformUserID)))
	return hex.EncodeToString(mac.Sum(nil))
}

func newOpenClawOpaqueID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func openClawReserveFingerprint(command OpenClawReserveLeaseCommand) string {
	return openClawFingerprint(
		"reserve",
		command.Account.BillingAccountID,
		command.TaskID,
		command.SessionID,
		command.NodeID,
		formatOpenClawAmount(command.AmountUSD),
	)
}

func openClawLeaseOperationFingerprint(operation string, command OpenClawLeaseOperationCommand) string {
	return openClawFingerprint(
		operation,
		command.Account.BillingAccountID,
		command.LeaseID,
		formatOpenClawAmount(command.AmountUSD),
	)
}

func openClawUsageEventFingerprint(command OpenClawUsageEventCommand) string {
	inputTokens := openClawOptionalInt64FingerprintValue(command.InputTokens)
	outputTokens := openClawOptionalInt64FingerprintValue(command.OutputTokens)
	cacheRead := openClawOptionalInt64FingerprintValue(command.CacheReadTokens)
	cacheWrite := openClawOptionalInt64FingerprintValue(command.CacheWriteTokens)
	threadID := ""
	if command.CodexThreadID != nil {
		threadID = *command.CodexThreadID
	}
	pricingVersion := ""
	if command.PricingVersion != nil {
		pricingVersion = *command.PricingVersion
	}
	estimated := ""
	if command.EstimatedUSD != nil {
		estimated = formatOpenClawAmount(*command.EstimatedUSD)
	}
	return openClawFingerprint(
		"usage-event",
		command.Account.BillingAccountID,
		command.EventID,
		command.TaskID,
		command.SessionID,
		command.NodeID,
		threadID,
		command.Model,
		inputTokens,
		outputTokens,
		cacheRead,
		cacheWrite,
		command.ObservedAt.UTC().Format(time.RFC3339Nano),
		command.ReportedAt.UTC().Format(time.RFC3339Nano),
		pricingVersion,
		string(command.PriceSnapshot),
		estimated,
	)
}

func openClawOptionalInt64FingerprintValue(value *int64) string {
	if value == nil {
		return ""
	}
	return fmt.Sprintf("%d", *value)
}

func openClawFingerprint(parts ...string) string {
	hash := sha256.New()
	for _, part := range parts {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(part))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func formatOpenClawAmount(value decimal.Decimal) string {
	return value.StringFixed(OpenClawUSDAmountScale)
}

// NormalizeOpenClawUSDAmount validates an exact bridge amount at the fixed
// NUMERIC(20,8) scale. HTTP callers must not send binary JSON numbers.
func NormalizeOpenClawUSDAmount(value decimal.Decimal, allowZero bool) (decimal.Decimal, error) {
	return normalizeOpenClawUSD(value, allowZero)
}
