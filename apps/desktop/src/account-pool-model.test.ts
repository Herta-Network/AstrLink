import { describe, expect, it } from "vitest";
import {
  canJoinPool,
  parseAccountPool,
  parseAccountPoolPage,
  parseTokenRouting,
} from "./account-pool-model";
import { testAccountPool, testPoolAccount } from "./account-pool-test-data";

describe("account pool IPC boundaries", () => {
  it("requires an explicit and complete token scope", () => {
    expect(parseTokenRouting({ routing_scope: "global" })).toEqual({
      routing_scope: "global",
    });
    expect(
      parseTokenRouting({ routing_scope: "pool", pool_id: "pool_one" }),
    ).toEqual({ routing_scope: "pool", pool_id: "pool_one" });
    for (const value of [
      {},
      { routing_scope: null },
      { routing_scope: "pool" },
      { routing_scope: "pool", pool_id: null },
      { routing_scope: "global", pool_id: "pool_one" },
      { routing_scope: "global", pool_id: null },
      { routing_scope: "pool", pool_id: "../global" },
    ])
      expect(() => parseTokenRouting(value)).toThrow();
  });
  it("keeps saved proxy credentials opaque and scoped to their pool", () => {
    const pool = testAccountPool({
      proxy: {
        mode: "custom",
        url: "socks5://127.0.0.1:1080",
        credential_ref: "local://pool-proxy/pool_claude_test",
      },
    });
    expect(parseAccountPool(pool)).toEqual(pool);
    for (const proxy of [
      { ...pool.proxy, credential_ref: "local://pool-proxy/pool_other" },
      { ...pool.proxy, credential: { password: "never-public" } },
      { ...pool.proxy, url: "socks5://user:secret@127.0.0.1:1080" },
    ])
      expect(() => parseAccountPool({ ...pool, proxy })).toThrow();
  });
  it("retains both quota windows and rejects inconsistent member status", () => {
    const pool = testAccountPool({
      members: [
        {
          pool_id: "pool_claude_test",
          service_id: "service_a",
          enabled: true,
          max_concurrent_requests: 2,
        },
      ],
      runtime: {
        active_requests: 0,
        waiting_requests: 1,
        available_accounts: 0,
        members: [
          {
            service_id: "service_a",
            state: "quota_cooldown",
            active_requests: 0,
            proxy_source: "pool",
            quota_cooldown: {
              service_id: "service_a",
              observed_at: "2026-11-03T00:00:00Z",
              source: "usage",
              windows: [
                {
                  id: "short",
                  limit_window_seconds: 18000,
                  reset_at: "2026-11-03T01:00:00Z",
                },
                { id: "weekly", limit_window_seconds: 604800 },
              ],
            },
          },
        ],
      },
    });
    expect(parseAccountPool(pool)).toEqual(pool);
    expect(() =>
      parseAccountPool({ ...pool, runtime: { ...pool.runtime, members: [] } }),
    ).toThrow();
    expect(() =>
      parseAccountPool({
        ...pool,
        members: [...pool.members, ...pool.members],
      }),
    ).toThrow();
    expect(() => parseAccountPoolPage({ items: [pool, pool] })).toThrow();
  });
  it("only offers matching direct accounts outside other pools", () => {
    const pool = testAccountPool(),
      account = testPoolAccount("service_a");
    expect(canJoinPool(account, pool, [pool])).toBe(true);
    expect(
      canJoinPool(
        {
          ...account,
          subscription: {
            provider: "claude_code",
            status: "connected",
            mode: "official",
          },
        },
        pool,
        [pool],
      ),
    ).toBe(false);
    expect(
      canJoinPool(
        {
          ...account,
          kind: "kiro_subscription",
          subscription: { provider: "kiro", status: "connected" },
        },
        pool,
        [pool],
      ),
    ).toBe(false);
    const other = testAccountPool({
      id: "pool_other",
      members: [
        {
          pool_id: "pool_other",
          service_id: account.id,
          enabled: true,
          max_concurrent_requests: 2,
        },
      ],
    });
    expect(canJoinPool(account, pool, [pool, other])).toBe(false);
  });
});
