package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ctxkey"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestOpenClawGatewayUsesNativeActualCostAndRetainsLeaseWhenUsageLogFails(t *testing.T) {
	logs := &openAIRecordUsageLogRepoStub{err: errors.New("usage log unavailable")}
	billing := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(logs, billing, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	usage := OpenAIUsage{InputTokens: 1000, OutputTokens: 250, CacheReadInputTokens: 100}
	expected := expectedOpenAICost(t, svc, "gpt-5.1", usage, svc.cfg.Default.RateMultiplier)
	link := &OpenClawGatewayRequest{LeaseID: "lease-one", RequestID: "client:request-one", APIKeyID: 100, UserID: 200}
	ctx := WithOpenClawGatewayRequest(context.WithValue(context.Background(), ctxkey.ClientRequestID, "request-one"), link)
	err := svc.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "upstream-one", Model: "gpt-5.1", Usage: usage, Duration: time.Second}, APIKey: &APIKey{ID: 100, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 200}, Account: &Account{ID: 300, Type: AccountTypeAPIKey}})
	require.NoError(t, err)
	require.Equal(t, 1, billing.calls)
	require.Equal(t, "client:request-one", billing.lastCmd.RequestID)
	require.Equal(t, "lease-one", billing.lastCmd.OpenClawLeaseID)
	require.Equal(t, "client:request-one", billing.lastCmd.OpenClawRequestID)
	require.True(t, billing.lastCmd.OpenClawPricingKnown)
	require.Equal(t, QuantizeUsageBillingAmount(expected.ActualCost), billing.lastCmd.BalanceCost)
	require.Len(t, billing.lastCmd.RequestFingerprint, 64)
}

func TestOpenClawGatewayMissingPricingIsNotKnownZero(t *testing.T) {
	billing := &openAIRecordUsageBillingRepoStub{result: &UsageBillingApplyResult{Applied: true, OpenClawUnsettled: true}}
	svc := newOpenAIRecordUsageServiceWithBillingRepoForTest(&openAIRecordUsageLogRepoStub{}, billing, &openAIRecordUsageUserRepoStub{}, &openAIRecordUsageSubRepoStub{}, nil)
	ctx := WithOpenClawGatewayRequest(context.Background(), &OpenClawGatewayRequest{LeaseID: "lease", RequestID: "request", APIKeyID: 100, UserID: 200})
	err := svc.RecordUsage(ctx, &OpenAIRecordUsageInput{Result: &OpenAIForwardResult{RequestID: "request", Model: "pricing-missing-test-model", Usage: OpenAIUsage{InputTokens: 100, OutputTokens: 10}}, APIKey: &APIKey{ID: 100, Group: &Group{RateMultiplier: 1}}, User: &User{ID: 200}, Account: &Account{ID: 300, Type: AccountTypeAPIKey}})
	require.NoError(t, err)
	require.False(t, billing.lastCmd.OpenClawPricingKnown)
}

type openClawSettingsStub struct {
	SettingRepository
	value string
}

func (s *openClawSettingsStub) GetValue(context.Context, string) (string, error) {
	if s.value == "" {
		return "", ErrSettingNotFound
	}
	return s.value, nil
}
func (s *openClawSettingsStub) Set(_ context.Context, _ string, value string) error {
	s.value = value
	return nil
}

func TestOpenClawTaskRequiresExplicitGroupBeforeProvisioningAndUsesAdjustableInitialGrant(t *testing.T) {
	cfg := &config.Config{OpenClawBilling: config.OpenClawBillingConfig{Enabled: true, InternalBearer: openClawTestBearer, IdentityHMACKey: openClawTestHMAC, DefaultGrantUSD: 50}}
	repo := &openClawBillingRepositoryStub{}
	billing := NewOpenClawBillingService(repo, cfg)
	settings := &openClawSettingsStub{}
	sessions := NewOpenClawTaskSessionService(nil, billing, nil, nil, nil, settings, cfg)
	_, err := sessions.Open(context.Background(), OpenClawTaskOpenInput{})
	require.ErrorIs(t, err, ErrOpenClawGatewayUnconfigured)
	require.Nil(t, repo.ensureProvision)
	_, err = sessions.SetSettings(context.Background(), decimal.RequireFromString("12.34567890"))
	require.NoError(t, err)
	_, err = billing.ResolveAccount(context.Background(), "platform-user")
	require.NoError(t, err)
	require.True(t, repo.ensureProvision.DefaultGrantUSD.Equal(decimal.RequireFromString("12.34567890")))
	view := openClawSubscriptionView(&UserSubscription{ID: 1, GroupID: 2})
	require.Contains(t, view, "dailyUsedUsd")
	require.NotContains(t, view, "dailyUsageUsd")
	require.Contains(t, view, "name")
	require.Nil(t, view["name"])
	require.Nil(t, view["dailyLimitUsd"])
}
