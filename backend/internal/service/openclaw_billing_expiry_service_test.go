package service

import (
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/stretchr/testify/require"
)

func TestOpenClawBillingLeaseExpiryServiceUsesBoundedTTLInterval(t *testing.T) {
	repo := &openClawBillingRepositoryStub{}
	billing := newOpenClawBillingServiceForTest(repo)
	worker := NewOpenClawBillingLeaseExpiryService(billing)
	require.Equal(t, openClawBillingLeaseExpiryMaxInterval, worker.interval)

	billing.config.LeaseTTLSeconds = 10
	worker = NewOpenClawBillingLeaseExpiryService(billing)
	require.Equal(t, 5*time.Second, worker.interval)

	billing.config.LeaseTTLSeconds = 1
	worker = NewOpenClawBillingLeaseExpiryService(billing)
	require.Equal(t, time.Second, worker.interval)
}

func TestOpenClawBillingLeaseExpiryServiceExpiresThroughBillingService(t *testing.T) {
	repo := &openClawBillingRepositoryStub{}
	billing := newOpenClawBillingServiceForTest(repo)
	worker := NewOpenClawBillingLeaseExpiryService(billing)

	worker.expireOnce()
	require.Equal(t, 1, repo.expireCalls)
}

func TestOpenClawBillingLeaseExpiryServiceDoesNotStartWhenBridgeIsDisabled(t *testing.T) {
	repo := &openClawBillingRepositoryStub{}
	billing := NewOpenClawBillingService(repo, &config.Config{OpenClawBilling: config.OpenClawBillingConfig{
		LeaseTTLSeconds: 1,
	}})
	worker := NewOpenClawBillingLeaseExpiryService(billing)
	worker.Start()
	defer worker.Stop()

	require.Zero(t, repo.expireCalls)
}
