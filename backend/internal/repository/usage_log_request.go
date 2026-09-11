package repository

import (
	"context"

	"github.com/Wei-Shaw/sub2api/internal/service"
)

const apiKeyRequestUsageQuery = `SELECT model,input_tokens,output_tokens,cache_read_tokens,cache_creation_tokens,service_tier,actual_cost::text
	FROM usage_logs WHERE api_key_id=$1 AND request_id=$2`

func (r *usageLogRepository) GetAPIKeyRequestUsage(ctx context.Context, apiKeyID int64, requestID string) (*service.APIKeyRequestUsage, error) {
	rows, err := r.sql.QueryContext(ctx, apiKeyRequestUsageQuery, apiKeyID, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	usage := &service.APIKeyRequestUsage{}
	if err := rows.Scan(&usage.Model, &usage.InputTokens, &usage.OutputTokens, &usage.CacheReadTokens,
		&usage.CacheCreationTokens, &usage.ServiceTier, &usage.ActualCostUSD); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return usage, nil
}
