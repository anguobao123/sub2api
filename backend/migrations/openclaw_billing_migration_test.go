package migrations

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOpenClawBillingMigrationKeepsBillingDataIsolatedAndPricedSafely(t *testing.T) {
	content, err := FS.ReadFile("221_openclaw_usd_billing_bridge.sql")
	require.NoError(t, err)

	sql := string(content)
	for _, table := range []string{
		"openclaw_billing_accounts",
		"openclaw_billing_grants",
		"openclaw_task_budget_leases",
		"openclaw_task_budget_lease_operations",
		"openclaw_billing_usage_events",
	} {
		require.Contains(t, sql, "CREATE TABLE IF NOT EXISTS "+table)
	}
	require.Contains(t, sql, "platform_user_hmac")
	require.NotContains(t, sql, "platform_user_id")
	require.Contains(t, sql, "users.balance / users.frozen_balance")
	require.Contains(t, sql, "DECIMAL(20,8)")
	require.Contains(t, sql, "status IN ('pending_pricing', 'recorded')")
	require.Contains(t, sql, "status = 'pending_pricing' AND (pricing_version IS NULL OR price_snapshot IS NULL)")
	require.Contains(t, sql, "status = 'recorded' AND pricing_version IS NOT NULL AND price_snapshot IS NOT NULL")
	require.NotContains(t, strings.ToLower(sql), "usage_logs")
}

func TestOpenClawBillingUsageMigrationProtectsEventsFromAllDeletionPaths(t *testing.T) {
	content, err := FS.ReadFile("221_openclaw_usd_billing_bridge.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "CREATE OR REPLACE FUNCTION prevent_openclaw_billing_usage_event_mutation()")
	require.Contains(t, sql, "CREATE TRIGGER trg_prevent_openclaw_billing_usage_event_mutation BEFORE UPDATE OR DELETE ON openclaw_billing_usage_events FOR EACH ROW")
	require.Contains(t, sql, "CREATE TRIGGER trg_prevent_openclaw_billing_usage_event_truncate BEFORE TRUNCATE ON openclaw_billing_usage_events FOR EACH STATEMENT")
	require.Contains(t, sql, "cannot be modified, deleted, or truncated")
}
