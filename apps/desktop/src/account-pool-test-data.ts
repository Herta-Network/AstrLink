import type { AccountPool } from "./account-pool-model";
import type { Service } from "./service-model";

export function testAccountPool(
  overrides: Partial<AccountPool> = {},
): AccountPool {
  return {
    id: "pool_claude_test",
    name: "Claude 开发",
    provider: "claude_code",
    enabled: true,
    sticky: { enabled: true, ttl_seconds: 3600 },
    queue: { max_waiting_requests: 32, timeout_ms: 5000 },
    members: [],
    created_at: "2026-11-03T00:00:00Z",
    updated_at: "2026-11-03T00:00:00Z",
    etag: '"pool-v1"',
    runtime: {
      active_requests: 0,
      waiting_requests: 0,
      available_accounts: 0,
      members: [],
    },
    ...overrides,
  };
}
export function testPoolAccount(
  id: string,
  overrides: Partial<Service> = {},
): Service {
  return {
    id,
    name: id,
    kind: "claude_subscription",
    enabled: true,
    models: ["claude-test"],
    capabilities: [
      { protocol: "anthropic.messages", mode: "native", streaming: true },
    ],
    subscription: {
      provider: "claude_code",
      status: "connected",
      mode: "legacy",
    },
    created_at: "2026-11-03T00:00:00Z",
    updated_at: "2026-11-03T00:00:00Z",
    ...overrides,
  };
}
