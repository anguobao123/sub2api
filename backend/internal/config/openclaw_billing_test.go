package config

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenClawBillingConfigurationDefaultsDisabled(t *testing.T) {
	resetViperWithJWTSecret(t)
	cfg, err := Load()
	require.NoError(t, err)
	require.False(t, cfg.OpenClawBilling.Enabled)
	require.Equal(t, 50.0, cfg.OpenClawBilling.DefaultGrantUSD)
	require.Equal(t, 900, cfg.OpenClawBilling.LeaseTTLSeconds)
	require.Equal(t, 100, cfg.OpenClawBilling.MaxUsageEventsPerBatch)
}

func TestOpenClawBillingConfigurationRequiresIndependentSecretsWhenEnabled(t *testing.T) {
	tests := []struct {
		name   string
		bearer string
		hmac   string
	}{
		{name: "missing both"},
		{name: "short bearer", bearer: "short", hmac: strings.Repeat("h", 32)},
		{name: "short hmac", bearer: strings.Repeat("b", 32), hmac: "short"},
		{name: "valid", bearer: strings.Repeat("b", 32), hmac: strings.Repeat("h", 32)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			resetViperWithJWTSecret(t)
			t.Setenv("OPENCLAW_BILLING_ENABLED", "true")
			t.Setenv("OPENCLAW_BILLING_INTERNAL_BEARER", test.bearer)
			t.Setenv("OPENCLAW_BILLING_IDENTITY_HMAC_KEY", test.hmac)
			cfg, err := Load()
			if test.name == "valid" {
				require.NoError(t, err)
				require.True(t, cfg.OpenClawBilling.Enabled)
				require.Equal(t, test.bearer, cfg.OpenClawBilling.InternalBearer)
				return
			}
			require.Error(t, err)
		})
	}
}

func TestOpenClawBillingConfigurationRejectsInvalidLeaseLimits(t *testing.T) {
	resetViperWithJWTSecret(t)
	t.Setenv("OPENCLAW_BILLING_LEASE_TTL_SECONDS", "0")
	_, err := Load()
	require.ErrorContains(t, err, "openclaw_billing.lease_ttl_seconds")
}
