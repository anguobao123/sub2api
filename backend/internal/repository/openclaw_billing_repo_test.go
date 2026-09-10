package repository

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestOpenClawEnsureAccountCreatesUserGrantAndBalanceInOneTransaction(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)
	provision := service.OpenClawAccountProvision{
		PlatformUserHMAC: strings.Repeat("a", 64),
		BillingAccountID: "ocba_test",
		SyntheticEmail:   "openclaw-ocba_test@bridge.invalid",
		PasswordHash:     "!openclaw-internal-login-disabled!",
		DefaultGrantUSD:  decimal.NewFromInt(50),
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WithArgs("identity\x1f" + provision.PlatformUserHMAC).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT a\.billing_account_id, a\.billing_user_id`).
		WithArgs(provision.PlatformUserHMAC).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO users`).
		WithArgs(provision.SyntheticEmail, provision.PasswordHash).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(42)))
	mock.ExpectExec(`INSERT INTO openclaw_billing_accounts`).
		WithArgs(provision.BillingAccountID, provision.PlatformUserHMAC, int64(42), provision.DefaultGrantUSD).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectExec(`INSERT INTO openclaw_billing_grants`).
		WithArgs(provision.BillingAccountID, int64(42), provision.DefaultGrantUSD).
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectQuery(`UPDATE users`).
		WithArgs(provision.DefaultGrantUSD, int64(42)).
		WillReturnRows(sqlmock.NewRows([]string{"balance", "frozen_balance"}).AddRow(50.0, 0.0))
	mock.ExpectCommit()

	account, err := repository.EnsureAccount(ctx, provision)
	require.NoError(t, err)
	require.True(t, account.Created)
	require.Equal(t, provision.BillingAccountID, account.BillingAccountID)
	require.EqualValues(t, 42, account.BillingUserID)
	require.True(t, decimal.NewFromInt(50).Equal(account.InitialGrantUSD))
	require.True(t, decimal.NewFromInt(50).Equal(account.Balance))
	require.True(t, account.FrozenBalance.IsZero())
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenClawEnsureAccountReplaysExistingMappingWithoutAnotherGrant(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)
	provision := service.OpenClawAccountProvision{
		PlatformUserHMAC: strings.Repeat("a", 64),
		BillingAccountID: "ocba_unused_on_replay",
		SyntheticEmail:   "openclaw-ocba_unused_on_replay@bridge.invalid",
		PasswordHash:     "!openclaw-internal-login-disabled!",
		DefaultGrantUSD:  decimal.NewFromInt(50),
	}

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WithArgs("identity\x1f" + provision.PlatformUserHMAC).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT a\.billing_account_id, a\.billing_user_id`).
		WithArgs(provision.PlatformUserHMAC).
		WillReturnRows(sqlmock.NewRows([]string{
			"billing_account_id", "billing_user_id", "initial_grant_usd", "balance", "frozen_balance",
		}).AddRow("ocba_existing", int64(42), 50.0, 43.25, 1.75))
	mock.ExpectCommit()

	account, err := repository.EnsureAccount(ctx, provision)
	require.NoError(t, err)
	require.False(t, account.Created)
	require.Equal(t, "ocba_existing", account.BillingAccountID)
	require.True(t, decimal.NewFromInt(50).Equal(account.InitialGrantUSD))
	require.True(t, decimal.RequireFromString("43.25").Equal(account.Balance))
	require.True(t, decimal.RequireFromString("1.75").Equal(account.FrozenBalance))
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenClawReserveLeaseUsesStrictAvailableBalanceGuard(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)
	command := openClawReserveCommandForTest()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WithArgs(openClawTaskLockKey(command.Account.BillingAccountID, command.TaskID, command.SessionID, command.NodeID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WithArgs(openClawReserveKeyLockKey(command.Account.BillingAccountID, command.IdempotencyKey)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT lease_id, billing_account_id`).
		WithArgs(command.Account.BillingAccountID, command.IdempotencyKey).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT lease_id, billing_account_id`).
		WithArgs(command.Account.BillingAccountID, command.TaskID, command.SessionID, command.NodeID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`UPDATE users\s+SET balance = balance - \$1,\s+frozen_balance = COALESCE\(frozen_balance, 0\) \+ \$1,\s+updated_at = NOW\(\)\s+WHERE id = \$2\s+AND deleted_at IS NULL\s+AND balance >= \$1\s+RETURNING balance, frozen_balance`).
		WithArgs(command.AmountUSD, command.Account.BillingUserID).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	_, err = repository.ReserveLease(ctx, command)
	require.ErrorIs(t, err, service.ErrOpenClawInsufficientBalance)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenClawUsageEventPendingPricingDoesNotIssueBalanceOrLeaseUpdates(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)
	command := openClawUsageCommandForTest(false)

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO openclaw_billing_usage_events`).
		WithArgs(
			command.EventID, command.Account.BillingAccountID, command.Account.BillingUserID,
			command.TaskID, command.SessionID, command.NodeID, command.CodexThreadID, command.Model,
			command.InputTokens, command.OutputTokens, command.CacheReadTokens, command.CacheWriteTokens,
			command.ObservedAt, command.ReportedAt, command.PricingVersion, nil, command.EstimatedUSD,
			service.OpenClawUsageStatusPendingPricing, command.IngestFingerprint,
		).
		WillReturnRows(sqlmock.NewRows([]string{"event_id"}).AddRow(command.EventID))
	mock.ExpectCommit()

	result, err := repository.IngestUsageEvent(ctx, command)
	require.NoError(t, err)
	require.Equal(t, service.OpenClawUsageStatusPendingPricing, result.Status)
	require.True(t, result.Applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenClawUsageEventDuplicateFingerprintIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)
	command := openClawUsageCommandForTest(false)

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO openclaw_billing_usage_events`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT event_id, status, ingest_fingerprint`).
		WithArgs(command.EventID).
		WillReturnRows(sqlmock.NewRows([]string{"event_id", "status", "ingest_fingerprint"}).AddRow(command.EventID, service.OpenClawUsageStatusPendingPricing, command.IngestFingerprint))
	mock.ExpectCommit()

	result, err := repository.IngestUsageEvent(ctx, command)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, service.OpenClawUsageStatusPendingPricing, result.Status)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenClawUsageEventDuplicateDifferentFingerprintConflicts(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)
	command := openClawUsageCommandForTest(false)

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO openclaw_billing_usage_events`).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT event_id, status, ingest_fingerprint`).
		WithArgs(command.EventID).
		WillReturnRows(sqlmock.NewRows([]string{"event_id", "status", "ingest_fingerprint"}).AddRow(command.EventID, service.OpenClawUsageStatusPendingPricing, "different"))
	mock.ExpectRollback()

	_, err = repository.IngestUsageEvent(ctx, command)
	require.ErrorIs(t, err, service.ErrOpenClawUsageEventConflict)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenClawCaptureOperationSerializesItsIdempotencyKeyBeforeLookup(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)
	command := service.OpenClawLeaseOperationCommand{
		Account:            service.OpenClawBillingAccount{BillingAccountID: "ocba_test", BillingUserID: 42},
		LeaseID:            "ocbl_test",
		AmountUSD:          decimal.RequireFromString("1.25"),
		IdempotencyKey:     "capture-test",
		RequestFingerprint: "capture-fingerprint",
	}
	createdAt := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	expiresAt := createdAt.Add(time.Hour)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta("SELECT pg_advisory_xact_lock(hashtextextended($1, 0))")).
		WithArgs(openClawLeaseOperationLockKey(command.Account.BillingAccountID, "capture", command.IdempotencyKey)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT lease_id, request_fingerprint`).
		WithArgs(command.Account.BillingAccountID, "capture", command.IdempotencyKey).
		WillReturnRows(sqlmock.NewRows([]string{"lease_id", "request_fingerprint"}).AddRow(command.LeaseID, command.RequestFingerprint))
	mock.ExpectQuery(`SELECT lease_id, billing_account_id`).
		WithArgs(command.Account.BillingAccountID, command.LeaseID).
		WillReturnRows(sqlmock.NewRows([]string{
			"lease_id", "billing_account_id", "billing_user_id", "task_id", "session_id", "node_id",
			"reserved_usd", "captured_usd", "released_usd", "status", "expires_at", "created_at", "finalized_at", "reserve_request_fingerprint",
		}).AddRow(command.LeaseID, command.Account.BillingAccountID, command.Account.BillingUserID, "task-test", "session-test", "node-test", 1.25, 1.25, 0, service.OpenClawLeaseStatusCaptured, expiresAt, createdAt, createdAt, "reserve-fingerprint"))
	mock.ExpectQuery(`SELECT balance, COALESCE\(frozen_balance, 0\)`).
		WithArgs(command.Account.BillingUserID).
		WillReturnRows(sqlmock.NewRows([]string{"balance", "frozen_balance"}).AddRow(48.75, 0))
	mock.ExpectCommit()

	result, err := repository.CaptureLease(ctx, command)
	require.NoError(t, err)
	require.False(t, result.Applied)
	require.Equal(t, command.LeaseID, result.Lease.LeaseID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestOpenClawExpiryQueryIncludesLeaseFingerprintForSharedScanner(t *testing.T) {
	ctx := context.Background()
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repository := newOpenClawBillingRepositoryWithDB(db)

	mock.ExpectBegin()
	mock.ExpectQuery(`SELECT lease_id, billing_account_id, billing_user_id, task_id, session_id, node_id,\s+reserved_usd, captured_usd, released_usd, status, expires_at, created_at, finalized_at,\s+reserve_request_fingerprint`).
		WithArgs(10).
		WillReturnRows(sqlmock.NewRows([]string{
			"lease_id", "billing_account_id", "billing_user_id", "task_id", "session_id", "node_id",
			"reserved_usd", "captured_usd", "released_usd", "status", "expires_at", "created_at", "finalized_at", "reserve_request_fingerprint",
		}))
	mock.ExpectCommit()

	count, err := repository.ExpireLeases(ctx, 10)
	require.NoError(t, err)
	require.Zero(t, count)
	require.NoError(t, mock.ExpectationsWereMet())
}

func openClawReserveCommandForTest() service.OpenClawReserveLeaseCommand {
	return service.OpenClawReserveLeaseCommand{
		Account:            service.OpenClawBillingAccount{BillingAccountID: "ocba_test", BillingUserID: 42},
		LeaseID:            "ocbl_test",
		TaskID:             "task-test",
		SessionID:          "session-test",
		NodeID:             "node-test",
		AmountUSD:          decimal.RequireFromString("1.25"),
		IdempotencyKey:     "reserve-test",
		RequestFingerprint: "reserve-fingerprint",
		ExpiresAt:          time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC),
	}
}

func openClawUsageCommandForTest(pricingComplete bool) service.OpenClawUsageEventCommand {
	observed := time.Date(2026, 8, 13, 10, 0, 0, 0, time.UTC)
	command := service.OpenClawUsageEventCommand{
		Account:           service.OpenClawBillingAccount{BillingAccountID: "ocba_test", BillingUserID: 42},
		EventID:           "evt_test",
		TaskID:            "task-test",
		SessionID:         "session-test",
		NodeID:            "node-test",
		Model:             "gpt-5.4",
		ObservedAt:        observed,
		ReportedAt:        observed.Add(time.Second),
		PricingComplete:   pricingComplete,
		IngestFingerprint: "usage-fingerprint",
	}
	if pricingComplete {
		pricingVersion := "suite-price-v1"
		command.PricingVersion = &pricingVersion
		command.PriceSnapshot = []byte(`{"input":{"currency":"USD","minorAmount":"1000","scale":8}}`)
	}
	return command
}
