package repository

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/require"
)

func TestAPIKeyRequestUsageRepository(t *testing.T) {
	db, mock := newSQLMock(t)
	repo := &usageLogRepository{sql: db}
	columns := []string{"model", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens", "service_tier", "actual_cost"}
	mock.ExpectQuery(regexp.QuoteMeta(apiKeyRequestUsageQuery)).WithArgs(int64(7), "client:request-one").
		WillReturnRows(sqlmock.NewRows(columns).AddRow("gpt-5.5", 10, 20, 30, 4, "priority", "0.0000000123"))
	usage, err := repo.GetAPIKeyRequestUsage(context.Background(), 7, "client:request-one")
	require.NoError(t, err)
	require.Equal(t, "0.0000000123", usage.ActualCostUSD)
	require.Equal(t, int64(10), usage.InputTokens)
	require.Equal(t, int64(30), usage.CacheReadTokens)
	require.Equal(t, int64(4), usage.CacheCreationTokens)
	require.Equal(t, "priority", *usage.ServiceTier)

	// The same request reference under another key is indistinguishable from pending usage.
	mock.ExpectQuery(regexp.QuoteMeta(apiKeyRequestUsageQuery)).WithArgs(int64(8), "client:request-one").
		WillReturnRows(sqlmock.NewRows(columns))
	missing, err := repo.GetAPIKeyRequestUsage(context.Background(), 8, "client:request-one")
	require.NoError(t, err)
	require.Nil(t, missing)

	mock.ExpectQuery(regexp.QuoteMeta(apiKeyRequestUsageQuery)).WithArgs(int64(7), "client:pending").
		WillReturnError(errors.New("database unavailable"))
	missing, err = repo.GetAPIKeyRequestUsage(context.Background(), 7, "client:pending")
	require.Error(t, err)
	require.Nil(t, missing)
	require.NoError(t, mock.ExpectationsWereMet())
}
