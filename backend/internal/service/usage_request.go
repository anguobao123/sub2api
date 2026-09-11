package service

import "context"

// APIKeyRequestUsage is a native upstream usage record. Input and cache token
// counts are disjoint; ActualCostUSD preserves the database decimal precision.
type APIKeyRequestUsage struct {
	RequestID           string  `json:"requestId"`
	Model               string  `json:"model"`
	InputTokens         int64   `json:"inputTokens"`
	OutputTokens        int64   `json:"outputTokens"`
	CacheReadTokens     int64   `json:"cacheReadTokens"`
	CacheCreationTokens int64   `json:"cacheCreationTokens"`
	ServiceTier         *string `json:"serviceTier"`
	ActualCostUSD       string  `json:"actualCostUsd"`
}

// GetAPIKeyRequestUsage resolves the response's X-Client-Request-ID in the
// current key's native usage namespace. Missing rows remain pending, not zero.
func (s *UsageService) GetAPIKeyRequestUsage(ctx context.Context, apiKeyID int64, clientRequestID string) (*APIKeyRequestUsage, error) {
	usage, err := s.usageRepo.GetAPIKeyRequestUsage(ctx, apiKeyID, "client:"+clientRequestID)
	if usage != nil {
		usage.RequestID = clientRequestID
	}
	return usage, err
}
