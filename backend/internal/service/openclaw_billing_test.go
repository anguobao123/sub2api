package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

const (
	openClawTestBearer = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	openClawTestHMAC   = "hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh"
)

type openClawBillingRepositoryStub struct {
	ensureProvision *OpenClawAccountProvision
	findHMAC        string
	findAccount     *OpenClawBillingAccount
	findErr         error
	reserveCommand  *OpenClawReserveLeaseCommand
	usageCommands   []OpenClawUsageEventCommand
	expireCalls     int
}

func (s *openClawBillingRepositoryStub) EnsureAccount(_ context.Context, provision OpenClawAccountProvision) (*OpenClawBillingAccount, error) {
	copy := provision
	s.ensureProvision = &copy
	return &OpenClawBillingAccount{
		BillingAccountID: provision.BillingAccountID,
		BillingUserID:    42,
		Balance:          provision.DefaultGrantUSD,
		InitialGrantUSD:  provision.DefaultGrantUSD,
		Created:          true,
	}, nil
}

func (s *openClawBillingRepositoryStub) FindAccount(_ context.Context, platformUserHMAC string) (*OpenClawBillingAccount, error) {
	s.findHMAC = platformUserHMAC
	if s.findErr != nil {
		return nil, s.findErr
	}
	return s.findAccount, nil
}

func (s *openClawBillingRepositoryStub) ReserveLease(_ context.Context, command OpenClawReserveLeaseCommand) (*OpenClawLeaseResult, error) {
	copy := command
	s.reserveCommand = &copy
	return &OpenClawLeaseResult{Applied: true, Lease: OpenClawBudgetLease{LeaseID: command.LeaseID}}, nil
}

func (s *openClawBillingRepositoryStub) CaptureLease(context.Context, OpenClawLeaseOperationCommand) (*OpenClawLeaseResult, error) {
	return nil, errors.New("unexpected capture")
}

func (s *openClawBillingRepositoryStub) ReleaseLease(context.Context, OpenClawLeaseOperationCommand) (*OpenClawLeaseResult, error) {
	return nil, errors.New("unexpected release")
}

func (s *openClawBillingRepositoryStub) ExpireLeases(context.Context, int) (int, error) {
	s.expireCalls++
	return 0, nil
}

func (s *openClawBillingRepositoryStub) IngestUsageEvent(_ context.Context, command OpenClawUsageEventCommand) (*OpenClawUsageEventResult, error) {
	s.usageCommands = append(s.usageCommands, command)
	status := OpenClawUsageStatusRecorded
	if !command.PricingComplete {
		status = OpenClawUsageStatusPendingPricing
	}
	return &OpenClawUsageEventResult{EventID: command.EventID, Applied: true, Status: status}, nil
}

func newOpenClawBillingServiceForTest(repo OpenClawBillingRepository) *OpenClawBillingService {
	return NewOpenClawBillingService(repo, &config.Config{OpenClawBilling: config.OpenClawBillingConfig{
		Enabled:                true,
		InternalBearer:         openClawTestBearer,
		IdentityHMACKey:        openClawTestHMAC,
		DefaultGrantUSD:        50,
		LeaseTTLSeconds:        900,
		MaxUsageEventsPerBatch: 10,
		ExpiryBatchSize:        10,
	}})
}

func TestOpenClawResolveAccountHashesPlatformIdentityAndGrantsDefaultUSD(t *testing.T) {
	repo := &openClawBillingRepositoryStub{}
	service := newOpenClawBillingServiceForTest(repo)

	_, err := service.ResolveAccount(context.Background(), "external-user-123")
	require.NoError(t, err)
	require.NotNil(t, repo.ensureProvision)
	require.Equal(t, hashOpenClawPlatformUserID(openClawTestHMAC, "external-user-123"), repo.ensureProvision.PlatformUserHMAC)
	require.NotContains(t, repo.ensureProvision.PlatformUserHMAC, "external-user-123")
	require.True(t, decimal.NewFromInt(50).Equal(repo.ensureProvision.DefaultGrantUSD))
	require.NotContains(t, repo.ensureProvision.SyntheticEmail, "external-user-123")
}

func TestOpenClawReserveRetryUsesExistingMappingAndStableFingerprint(t *testing.T) {
	repo := &openClawBillingRepositoryStub{findAccount: &OpenClawBillingAccount{BillingAccountID: "ocba_existing", BillingUserID: 42}}
	service := newOpenClawBillingServiceForTest(repo)
	service.now = func() time.Time { return time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC) }
	input := OpenClawReserveLeaseInput{
		PlatformUserID: "external-user-123",
		TaskID:         "task-1",
		SessionID:      "session-1",
		NodeID:         "node-1",
		AmountUSD:      openClawTestAmount("1.25"),
		IdempotencyKey: "reserve-task-1",
	}

	_, err := service.ReserveLease(context.Background(), input)
	require.NoError(t, err)
	require.Nil(t, repo.ensureProvision, "an existing mapping must not receive another grant")
	require.NotNil(t, repo.reserveCommand)
	firstFingerprint := repo.reserveCommand.RequestFingerprint

	service.now = func() time.Time { return time.Date(2026, 8, 13, 12, 5, 0, 0, time.UTC) }
	_, err = service.ReserveLease(context.Background(), input)
	require.NoError(t, err)
	require.Equal(t, firstFingerprint, repo.reserveCommand.RequestFingerprint, "retries must not depend on regenerated expiry")
	require.Equal(t, hashOpenClawPlatformUserID(openClawTestHMAC, input.PlatformUserID), repo.findHMAC)
}

func TestOpenClawUsageWithoutCompletePricingPersistsPendingWithoutCapture(t *testing.T) {
	repo := &openClawBillingRepositoryStub{}
	service := newOpenClawBillingServiceForTest(repo)
	observed := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	reported := observed.Add(time.Second)

	result, err := service.IngestUsageEvents(context.Background(), []OpenClawUsageEventInput{{
		EventID:        "event-1",
		PlatformUserID: "external-user-123",
		TaskID:         "task-1",
		SessionID:      "session-1",
		NodeID:         "node-1",
		Model:          "gpt-5.4",
		ObservedAt:     observed,
		ReportedAt:     reported,
		EstimatedUSD:   pointerToOpenClawTestAmount("1.25"),
	}})
	require.NoError(t, err)
	require.Len(t, result, 1)
	require.Equal(t, OpenClawUsageStatusPendingPricing, result[0].Status)
	require.Len(t, repo.usageCommands, 1)
	require.False(t, repo.usageCommands[0].PricingComplete)
	require.NotNil(t, repo.usageCommands[0].EstimatedUSD)
}

func TestOpenClawUsagePreservesNullableTokenCountsAndCanonicalPriceSnapshot(t *testing.T) {
	repo := &openClawBillingRepositoryStub{}
	service := newOpenClawBillingServiceForTest(repo)
	zero := int64(0)
	priceSnapshot := json.RawMessage(` { "output" : {"currency":"USD","minorAmount":"1000","scale":8}, "input" : {"currency":"USD","minorAmount":"500","scale":8} } `)
	version := "suite-price-2026-08"
	observed := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)

	result, err := service.IngestUsageEvents(context.Background(), []OpenClawUsageEventInput{{
		EventID:        "event-2",
		PlatformUserID: "external-user-123",
		TaskID:         "task-2",
		SessionID:      "session-2",
		NodeID:         "node-2",
		Model:          "gpt-5.4",
		InputTokens:    &zero,
		ObservedAt:     observed,
		ReportedAt:     observed.Add(time.Second),
		PricingVersion: &version,
		PriceSnapshot:  priceSnapshot,
		EstimatedUSD:   pointerToOpenClawTestAmount("0"),
	}})
	require.NoError(t, err)
	require.Equal(t, OpenClawUsageStatusRecorded, result[0].Status)
	command := repo.usageCommands[0]
	require.True(t, command.PricingComplete)
	require.NotNil(t, command.InputTokens)
	require.EqualValues(t, 0, *command.InputTokens)
	require.Nil(t, command.OutputTokens)
	require.JSONEq(t, `{"input":{"currency":"USD","minorAmount":"500","scale":8},"output":{"currency":"USD","minorAmount":"1000","scale":8}}`, string(command.PriceSnapshot))
}

func TestOpenClawUsageRejectsNumericPriceSnapshot(t *testing.T) {
	repo := &openClawBillingRepositoryStub{}
	bridge := newOpenClawBillingServiceForTest(repo)
	observed := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	version := "suite-price-2026-08"

	_, err := bridge.IngestUsageEvents(context.Background(), []OpenClawUsageEventInput{{
		EventID: "event-numeric-price", PlatformUserID: "external-user-123", TaskID: "task-3", SessionID: "session-3", NodeID: "node-3", Model: "gpt-5.4",
		ObservedAt: observed, ReportedAt: observed.Add(time.Second), PricingVersion: &version, PriceSnapshot: json.RawMessage(`{"input":0.000005}`),
	}})
	require.ErrorIs(t, err, ErrOpenClawBillingInvalid)
	require.Empty(t, repo.usageCommands)
}

func TestOpenClawCaptureDoesNotProvisionMissingMapping(t *testing.T) {
	repo := &openClawBillingRepositoryStub{findErr: ErrOpenClawAccountNotFound}
	service := newOpenClawBillingServiceForTest(repo)
	_, err := service.CaptureLease(context.Background(), OpenClawLeaseOperationInput{
		PlatformUserID: "unknown-external-user",
		LeaseID:        "ocbl_unknown",
		AmountUSD:      openClawTestAmount("1"),
		IdempotencyKey: "capture-1",
	})
	require.ErrorIs(t, err, ErrOpenClawAccountNotFound)
	require.Nil(t, repo.ensureProvision)
}

func openClawTestAmount(value string) decimal.Decimal {
	amount, err := decimal.NewFromString(value)
	if err != nil {
		panic(err)
	}
	return amount
}

func pointerToOpenClawTestAmount(value string) *decimal.Decimal {
	amount := openClawTestAmount(value)
	return &amount
}
