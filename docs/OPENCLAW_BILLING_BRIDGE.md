# OpenClaw personal USD billing

The bridge is disabled by default. Its internal HTTP API uses a separate bearer credential and must remain private. It uses native Sub2API users, API keys, USD prices, subscription assignments and request billing transactions.

Configuration:

```yaml
openclaw_billing:
  enabled: false
  gateway_group_id: 0
  default_grant_usd: 50
  lease_ttl_seconds: 900
```

`gateway_group_id` must identify an active native OpenAI group. Zero explicitly prevents opening task sessions. Secrets remain in private environment configuration. Initial grants can be changed through the settings endpoint; this changes future first mappings only.

All amount objects use `{ "currency": "USD", "minorAmount": "5000000000", "scale": 8 }`. New endpoints return `{ "data": ... }`. The original colon-form bridge endpoints retain their existing response forms.

## Internal endpoints

All paths below are under `/internal/openclaw/v1`.

| Method and path | Input | Result |
| --- | --- | --- |
| `POST accounts:resolve` | `platformUserId` | Existing account or a new account with one initial grant; original response format |
| `POST accounts/overview` | `platformUserId` | Read-only lookup: `billingAccountId`, `balanceUsd`, `frozenUsd`, `initialGrantUsd`, `subscriptions`, `transactions`; absent mapping returns 404 |
| `POST accounts/adjust-balance` | `platformUserId`, `operation` (`add` or `set`), `amountUsd`, `idempotencyKey`, `reason` | Account amount fields; `set` changes available balance and preserves outstanding holds |
| `GET settings` | — | `initialGrantUsd` |
| `POST settings` | `initialGrantUsd` | Saved setting |
| `POST subscriptions/options` | `{}` | Active native OpenAI subscription groups; no fabricated plans |
| `POST accounts/assign-subscription` | `platformUserId`, `groupId`, `days`, `idempotencyKey`, `reason` | Native subscription; synchronizes the personal gateway key's group |
| `POST task-sessions/open` | `platformUserId`, `taskId`, `sessionId`, `nodeId`, `allowedModels` | `leaseId`, `expiresAt`, private `gatewayApiKey`, `account` |
| `POST task-sessions/:leaseId/renew` | `platformUserId`, `taskId`, `nodeId` | `leaseId`, `expiresAt`; terminal or closing sessions keep their original deadline |
| `POST task-sessions/:leaseId/close` | `platformUserId`, `taskId`, `nodeId`, `idempotencyKey` | `leaseId`, `status`, `chargedUsd` |
| `POST task-sessions/close-by-task` | `platformUserId`, `taskId`, `sessionId`, `nodeId`, `idempotencyKey` | Same close result; a missing session returns `leaseId:null`, `status:"closed"`, `chargedUsd:null` |

Closing by task records an intent under the same task lock as opening, so an open request arriving after close cannot reserve funds. A task's repeated open returns its original active session. A different node, session or model list is rejected.

## Gateway and settlement

Internal billing users retain an invalid password hash. The bridge enables API access with a native personal key and native `concurrency=0` (unlimited); it does not introduce an execution-count limit.

Task keys can read model and usage metadata. Paid requests require `X-OpenClaw-Lease` and the matching key, user, unexpired task session and allowed model. This implementation supports HTTP Responses and compact endpoints. WebSocket and other paid protocols are explicitly rejected until they carry the same accounting lifecycle.

A wallet task initially reserves `min(USD 1, available balance)`. This is an initial hold, not a task price or hard spending cap. A valid native subscription permits opening without a wallet hold. Before each paid request the gateway records an independent in-flight request ID; task IDs do not replace request IDs.

The native gateway calculates `ActualCost` with its existing model, group, tier and subscription policy. The native request/key dedup transaction captures that amount instead of taking a second wallet debit. If the task's own hold is insufficient, the transaction can use remaining available personal balance. It cannot consume another task's hold or create an overdraft. Subscription costs stay in the native subscription branch and do not also debit the wallet.

When funds are insufficient, the real amount remains unsettled and new paid requests are blocked. Adding available funds retries these known wallet debts transactionally. Missing pricing or uncertain usage remains unknown; it is not treated as zero or automatically refunded.

Close/expiry rejects new requests. In-flight or unsettled requests retain their funds; a late known charge can still settle. Only after pending requests clear does close release the unused remainder. The existing automatic expiry path excludes these managed sessions from legacy unconditional release.

Subscription fields use `subscriptionId`, `groupId`, `name`, `status`, `expiresAt`, `dailyUsedUsd`, `weeklyUsedUsd`, `monthlyUsedUsd` and matching `*LimitUsd`. Missing metadata/limits are null. Transactions include `requestId`, `taskId`, `model`, token counts, `fundingSource`, `status`, `chargedUsd`, `actualCostUsd`, and `createdAt`. `chargedUsd` stays null while unsettled; `actualCostUsd` can retain a known unpaid amount. Overview returns the latest 100 request records.

## Verification boundary

Repository integration tests use `OPENCLAW_TEST_DATABASE_URL` only for a dedicated disposable PostgreSQL database. They apply the real native migrations and exercise grants, holds, native billing dedup, refunds, parallel tasks, insufficient funds, top-up settlement, expiry and subscription charging. Synthetic test prices and records do not constitute production charges or payment acceptance.
