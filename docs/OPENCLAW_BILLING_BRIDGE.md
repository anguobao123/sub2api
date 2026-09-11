# Fusion upstream usage and retired OpenClaw wallet bridge

## Current integration

Since the 2026-09-11 cutover, Suite on Lexi owns customer balances, prices, plans and charges. Sub2API supplies upstream model execution and platform cost information. It is replaceable, and its cost records do not directly debit Suite customers.

Two ordinary native accounts separate workloads: `fusion-user-workloads` aggregates user-task requests, while `fusion-platform-services` is reserved for platform-internal requests and currently has no key or traffic. They are not customer wallets or OpenClaw mappings. Their keys remain private to the control plane; do not reuse the administrator's key or create one native account/key per Suite customer.

### Native request usage lookup

`GET /v1/usage/requests?request_id=<native X-Client-Request-ID>` uses the existing ordinary API-key authentication. Supply the header value returned by the model response; the server adds the internal `client:` prefix and filters by the authenticated API key ID. The Responses body `id` is not this lookup key, and a caller-supplied header is not trusted as the native identifier.

The response is a plain JSON object, without a `data` wrapper:

```json
{
  "ready": true,
  "requestId": "native-request-id",
  "model": "gpt-5.5",
  "inputTokens": 10,
  "outputTokens": 20,
  "cacheReadTokens": 30,
  "cacheCreationTokens": 4,
  "serviceTier": null,
  "actualCostUsd": "0.0000000123"
}
```

Input and cache counters are disjoint; reconstruct total Responses input as `inputTokens + cacheReadTokens + cacheCreationTokens`. `actualCostUsd` preserves the database decimal precision as a string. These are upstream accounting fields; Suite applies its own saved retail prices to usage and hides upstream cost from customer billing views.

No matching record, including a record owned by another key, returns `{"ready":false}`. This means unavailable usage, not zero cost or permission to replay a model request. The endpoint uses `Cache-Control: no-store`, performs no wallet mutation or last-used update, and remains available when the old bridge is disabled or the service wallet/quota is exhausted. Disabled users/keys are rejected; invalid request references return 400 and unavailable storage returns 503 without raw database details.

### Migration and recovery boundary

The cutover preserved 2 customer balances and imported 3 settled request records, 2 grants, 1 adjustment and 6 initial price cards into Suite. Customer pricing is maintained independently in Suite after that initial import. Old native mapped users/keys are disabled, their exact authentication caches were invalidated, and `openclaw_billing.enabled=false` is effective after restart. Balances, historical rows and identity mapping secrets remain retained.

The following bridge documentation describes the **retired** per-user native-wallet implementation. Keep it for historical records and controlled recovery; do not re-enable it or reconnect Suite customer charging during an ordinary rollback. Neither program rollback nor this document authorizes deleting history or replacing the migrated Suite ledger.

## Retired bridge configuration

The retired bridge is disabled by default. Its internal HTTP API used a separate bearer credential and must remain private. It used native Sub2API users, API keys, USD prices, subscription assignments and request billing transactions.

Configuration:

```yaml
openclaw_billing:
  enabled: false
  gateway_group_id: 0
  default_grant_usd: 50
  lease_ttl_seconds: 900
```

`gateway_group_id` must identify an active native OpenAI group. Zero explicitly prevents opening task sessions. Secrets remain in private environment configuration. Initial grants can be changed through the settings endpoint; this changes future first mappings only.

Retired bridge amount objects use `{ "currency": "USD", "minorAmount": "5000000000", "scale": 8 }`. Its slash-form account/task-session endpoints return `{ "data": ... }`. The original colon-form bridge endpoints retain their existing response forms.

## Retired internal endpoints

All paths below are under `/internal/openclaw/v1` and are retained implementation documentation, not active Suite endpoints.

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

## Retired gateway and settlement

Internal billing users retain an invalid password hash. The bridge enables API access with a native personal key and native `concurrency=0` (unlimited); it does not introduce an execution-count limit.

Task keys can read model and usage metadata. Paid requests require `X-OpenClaw-Lease` and the matching key, user, unexpired task session and allowed model. This implementation supports HTTP Responses and compact endpoints. WebSocket and other paid protocols are explicitly rejected until they carry the same accounting lifecycle.

A wallet task initially reserves `min(USD 1, available balance)`. This is an initial hold, not a task price or hard spending cap. A valid native subscription permits opening without a wallet hold. Before each paid request the gateway records an independent in-flight request ID; task IDs do not replace request IDs.

The native gateway calculates `ActualCost` with its existing model, group, tier and subscription policy. The native request/key dedup transaction captures that amount instead of taking a second wallet debit. If the task's own hold is insufficient, the transaction can use remaining available personal balance. It cannot consume another task's hold or create an overdraft. Subscription costs stay in the native subscription branch and do not also debit the wallet.

When funds are insufficient, the real amount remains unsettled and new paid requests are blocked. Adding available funds retries these known wallet debts transactionally. Missing pricing or uncertain usage remains unknown; it is not treated as zero or automatically refunded.

Close/expiry rejects new requests. In-flight or unsettled requests retain their funds; a late known charge can still settle. Only after pending requests clear does close release the unused remainder. The existing automatic expiry path excludes these managed sessions from legacy unconditional release.

Subscription fields use `subscriptionId`, `groupId`, `name`, `status`, `expiresAt`, `dailyUsedUsd`, `weeklyUsedUsd`, `monthlyUsedUsd` and matching `*LimitUsd`. Missing metadata/limits are null. Transactions include `requestId`, `taskId`, `model`, token counts, `fundingSource`, `status`, `chargedUsd`, `actualCostUsd`, and `createdAt`. `chargedUsd` stays null while unsettled; `actualCostUsd` can retain a known unpaid amount. Overview returns the latest 100 request records.

## Historical verification boundary

Repository integration tests use `OPENCLAW_TEST_DATABASE_URL` only for a dedicated disposable PostgreSQL database. They apply the real native migrations and exercise grants, holds, native billing dedup, refunds, parallel tasks, insufficient funds, top-up settlement, expiry and subscription charging. Synthetic test prices and records do not constitute production charges or payment acceptance.
