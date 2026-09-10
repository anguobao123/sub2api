package service

import (
	"context"
	"strings"

	"github.com/shopspring/decimal"
)

func (s *OpenClawTaskSessionService) Overview(ctx context.Context, platformUserID string) (map[string]any, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	a, err := s.billing.findAccount(ctx, platformUserID)
	if err != nil {
		return nil, err
	}
	view := OpenClawAccountViewOf(a)
	subscriptions, err := s.subscriptions.ListUserSubscriptions(ctx, a.BillingUserID)
	if err != nil {
		return nil, err
	}
	subs := []map[string]any{}
	for _, sub := range subscriptions {
		subs = append(subs, openClawSubscriptionView(&sub))
	}
	transactions, err := s.repository.Transactions(ctx, a.BillingAccountID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"billingAccountId": view.BillingAccountID, "balanceUsd": view.BalanceUSD, "frozenUsd": view.FrozenUSD, "initialGrantUsd": view.InitialGrantUSD, "subscriptions": subs, "transactions": transactions}, nil
}

func (s *OpenClawTaskSessionService) AdjustBalance(ctx context.Context, platformUserID, operation string, amount decimal.Decimal, key, reason string) (*OpenClawBillingAccount, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	if operation != "add" && operation != "set" {
		return nil, ErrOpenClawBillingInvalid
	}
	if _, err := NormalizeOpenClawUSDAmount(amount, true); err != nil {
		return nil, err
	}
	if err := validateOpenClawRequired(key, "idempotency key"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) == "" || len(reason) > 1000 {
		return nil, ErrOpenClawBillingInvalid
	}
	a, err := s.billing.findAccount(ctx, platformUserID)
	if err != nil {
		return nil, err
	}
	return s.repository.AdjustBalance(ctx, *a, operation, amount, key, reason)
}

func (s *OpenClawTaskSessionService) GetSettings(ctx context.Context) (map[string]any, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	amount, err := readOpenClawInitialGrant(ctx, s.settings, s.billing.config.DefaultGrantUSD)
	if err != nil {
		return nil, err
	}
	return map[string]any{"initialGrantUsd": OpenClawMoneyFromDecimal(amount)}, nil
}

func (s *OpenClawTaskSessionService) SetSettings(ctx context.Context, amount decimal.Decimal) (map[string]any, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	if _, err := NormalizeOpenClawUSDAmount(amount, true); err != nil {
		return nil, err
	}
	if err := s.settings.Set(ctx, openClawInitialGrantSetting, amount.Shift(8).StringFixed(0)); err != nil {
		return nil, err
	}
	return map[string]any{"initialGrantUsd": OpenClawMoneyFromDecimal(amount)}, nil
}

func (s *OpenClawTaskSessionService) SubscriptionOptions(ctx context.Context) ([]map[string]any, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	groups, err := s.groups.ListActive(ctx)
	if err != nil {
		return nil, err
	}
	result := []map[string]any{}
	for _, group := range groups {
		if group.Platform != PlatformOpenAI || !group.IsSubscriptionType() {
			continue
		}
		result = append(result, map[string]any{"groupId": group.ID, "name": group.Name, "defaultDays": group.DefaultValidityDays, "dailyLimitUsd": openClawOptionalNativeMoney(group.DailyLimitUSD), "weeklyLimitUsd": openClawOptionalNativeMoney(group.WeeklyLimitUSD), "monthlyLimitUsd": openClawOptionalNativeMoney(group.MonthlyLimitUSD)})
	}
	return result, nil
}

func (s *OpenClawTaskSessionService) AssignSubscription(ctx context.Context, platformUserID string, groupID int64, days int, key, reason string) (map[string]any, error) {
	if err := s.billing.requireEnabled(); err != nil {
		return nil, err
	}
	if groupID <= 0 || days < 1 || days > 36500 || strings.TrimSpace(reason) == "" || len(reason) > 1000 {
		return nil, ErrOpenClawBillingInvalid
	}
	if err := validateOpenClawRequired(key, "idempotency key"); err != nil {
		return nil, err
	}
	a, err := s.billing.findAccount(ctx, platformUserID)
	if err != nil {
		return nil, err
	}
	coordinator := DefaultIdempotencyCoordinator()
	if coordinator == nil {
		return nil, ErrIdempotencyStoreUnavail
	}
	outcome, err := coordinator.Execute(ctx, IdempotencyExecuteOptions{
		Scope: "openclaw.subscription.assign:" + a.BillingAccountID, ActorScope: a.BillingAccountID,
		Method: "POST", Route: "/internal/openclaw/v1/accounts/assign-subscription", IdempotencyKey: key,
		Payload: map[string]any{"groupId": groupID, "days": days, "reason": reason}, RequireKey: true, TTL: DefaultWriteIdempotencyTTL(),
	}, func(operationCtx context.Context) (any, error) {
		return s.assignSubscription(operationCtx, a, groupID, days, key, reason)
	})
	if err != nil {
		return nil, err
	}
	view, ok := outcome.Data.(map[string]any)
	if !ok {
		return nil, ErrOpenClawBillingInvalid
	}
	return view, nil
}

func (s *OpenClawTaskSessionService) assignSubscription(ctx context.Context, a *OpenClawBillingAccount, groupID int64, days int, key, reason string) (map[string]any, error) {
	group, err := s.groups.GetByID(ctx, groupID)
	if err != nil {
		return nil, err
	}
	if group.Platform != PlatformOpenAI || !group.IsSubscriptionType() || group.Status != StatusActive {
		return nil, ErrOpenClawBillingInvalid
	}
	id, err := s.repository.FindSubscriptionAssignment(ctx, a.BillingAccountID, key, groupID, days, reason)
	if err != nil {
		return nil, err
	}
	var subscription *UserSubscription
	if id != nil {
		subscription, err = s.subscriptions.GetByID(ctx, *id)
	} else {
		subscription, err = s.subscriptions.AssignSubscription(ctx, &AssignSubscriptionInput{UserID: a.BillingUserID, GroupID: groupID, ValidityDays: days, Notes: "openclaw:" + key})
		if err == nil {
			err = s.repository.RecordSubscriptionAssignment(ctx, a.BillingAccountID, key, groupID, days, reason, subscription.ID)
		}
	}
	if err != nil {
		return nil, err
	}
	if err = s.repository.SetGatewayGroup(ctx, a.BillingAccountID, groupID); err != nil {
		return nil, err
	}
	proposed, err := s.apiKeys.GenerateKey()
	if err != nil {
		return nil, err
	}
	_, gatewayKey, err := s.repository.EnsureGatewayKey(ctx, *a, proposed, groupID)
	if err != nil {
		return nil, err
	}
	s.apiKeys.InvalidateAuthCacheByKey(ctx, gatewayKey)
	return openClawSubscriptionView(subscription), nil
}

func openClawOptionalNativeMoney(value *float64) any {
	if value == nil {
		return nil
	}
	return OpenClawMoneyFromDecimal(decimal.NewFromFloat(QuantizeUsageBillingAmount(*value)))
}

func openClawSubscriptionView(sub *UserSubscription) map[string]any {
	view := map[string]any{"subscriptionId": sub.ID, "groupId": sub.GroupID, "name": nil, "dailyLimitUsd": nil, "weeklyLimitUsd": nil, "monthlyLimitUsd": nil, "status": sub.Status, "expiresAt": sub.ExpiresAt, "dailyUsedUsd": OpenClawMoneyFromDecimal(decimal.NewFromFloat(QuantizeUsageBillingAmount(sub.DailyUsageUSD))), "weeklyUsedUsd": OpenClawMoneyFromDecimal(decimal.NewFromFloat(QuantizeUsageBillingAmount(sub.WeeklyUsageUSD))), "monthlyUsedUsd": OpenClawMoneyFromDecimal(decimal.NewFromFloat(QuantizeUsageBillingAmount(sub.MonthlyUsageUSD)))}
	if sub.Group != nil {
		view["name"] = sub.Group.Name
		view["dailyLimitUsd"] = openClawOptionalNativeMoney(sub.Group.DailyLimitUSD)
		view["weeklyLimitUsd"] = openClawOptionalNativeMoney(sub.Group.WeeklyLimitUSD)
		view["monthlyLimitUsd"] = openClawOptionalNativeMoney(sub.Group.MonthlyLimitUSD)
	}
	return view
}
