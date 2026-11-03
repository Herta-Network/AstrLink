import {
  subscriptionKindProviders,
  type Service,
  type SubscriptionServiceKind,
} from "./service-model";
import type { SubscriptionProvider } from "./subscription-model";
import {
  validProxyURL,
  type ServiceProxy,
  type ServiceProxyInput,
} from "./service-proxy-model";

export const poolProviderNames: Record<SubscriptionProvider, string> = {
  openai_codex: "OpenAI Codex",
  claude_code: "Claude",
  xai_grok: "Grok",
  antigravity: "Antigravity",
  github_copilot: "GitHub Copilot",
  factory_droid: "Factory Droid",
  kiro: "Kiro",
};

export type AccessTokenRouting =
  | { routing_scope: "global"; pool_id?: never }
  | { routing_scope: "pool"; pool_id: string };

export interface AccountPoolMember {
  pool_id: string;
  service_id: string;
  enabled: boolean;
  max_concurrent_requests: number;
}

export interface QuotaCooldown {
  service_id: string;
  observed_at: string;
  source: "usage" | "upstream";
  windows: { id: string; limit_window_seconds?: number; reset_at?: string }[];
}

export interface PoolMemberStatus {
  usage_observed_at?: string;
  service_id: string;
  state:
    | "ready"
    | "disabled"
    | "disconnected"
    | "paused"
    | "quota_cooldown"
    | "unknown";
  active_requests: number;
  proxy_source: "global" | "pool" | "account";
  quota_cooldown?: QuotaCooldown;
}

export interface AccountPool {
  id: string;
  name: string;
  provider: SubscriptionProvider;
  enabled: boolean;
  sticky: { enabled: boolean; ttl_seconds: number };
  queue: { max_waiting_requests: number; timeout_ms: number };
  proxy?: ServiceProxy;
  members: AccountPoolMember[];
  created_at: string;
  updated_at: string;
  etag: string;
  runtime: {
    active_requests: number;
    waiting_requests: number;
    available_accounts: number;
    members: PoolMemberStatus[];
  };
}

export interface AccountPoolInput {
  name: string;
  provider: SubscriptionProvider;
  enabled?: boolean;
  sticky?: AccountPool["sticky"];
  queue?: AccountPool["queue"];
  proxy?: ServiceProxyInput | null;
}
export type AccountPoolPatch = Partial<Omit<AccountPoolInput, "provider">>;
export type AccountPoolMemberInput = Omit<AccountPoolMember, "pool_id">;

type Json = Record<string, unknown>;
const idPattern = /^[a-z][a-z0-9_-]{2,95}$/;
function invalid(path: string): never {
  throw new Error(`Invalid account-pool IPC response at ${path}`);
}
function object(value: unknown, path: string): Json {
  if (!value || typeof value !== "object" || Array.isArray(value))
    return invalid(path);
  return value as Json;
}
function fields(
  value: Json,
  required: string[],
  optional: string[],
  path: string,
) {
  if (
    required.some((key) => !Object.hasOwn(value, key)) ||
    Object.keys(value).some(
      (key) => !required.includes(key) && !optional.includes(key),
    )
  )
    invalid(path);
}
function text(value: unknown, path: string, min = 1, max = 128): string {
  if (
    typeof value !== "string" ||
    [...value].length < min ||
    [...value].length > max
  )
    return invalid(path);
  return value;
}
function id(value: unknown, path: string): string {
  const result = text(value, path, 3, 96);
  if (!idPattern.test(result)) invalid(path);
  return result;
}
function number(
  value: unknown,
  path: string,
  min = 0,
  max = Number.MAX_SAFE_INTEGER,
): number {
  if (
    typeof value !== "number" ||
    !Number.isSafeInteger(value) ||
    value < min ||
    value > max
  )
    return invalid(path);
  return value;
}
function bool(value: unknown, path: string): boolean {
  if (typeof value !== "boolean") return invalid(path);
  return value;
}
function date(value: unknown, path: string): string {
  const result = text(value, path, 20, 64);
  if (!/^\d{4}-\d{2}-\d{2}T/.test(result) || Number.isNaN(Date.parse(result)))
    invalid(path);
  return result;
}
function array(value: unknown, path: string): unknown[] {
  if (!Array.isArray(value)) return invalid(path);
  return value;
}

export function parseTokenRouting(
  value: unknown,
  path = "$",
): AccessTokenRouting {
  const input = object(value, path);
  if (input.routing_scope === "global" && !Object.hasOwn(input, "pool_id"))
    return { routing_scope: "global" };
  if (input.routing_scope === "pool")
    return {
      routing_scope: "pool",
      pool_id: id(input.pool_id, `${path}.pool_id`),
    };
  return invalid(`${path}.routing_scope`);
}

function cooldown(
  value: unknown,
  serviceID: string,
  path: string,
): QuotaCooldown {
  const input = object(value, path);
  fields(input, ["service_id", "observed_at", "source", "windows"], [], path);
  if (
    input.service_id !== serviceID ||
    !["usage", "upstream"].includes(String(input.source))
  )
    invalid(path);
  const windows = array(input.windows, `${path}.windows`).map(
    (value, index) => {
      const at = `${path}.windows[${index}]`,
        row = object(value, at);
      fields(row, ["id"], ["limit_window_seconds", "reset_at"], at);
      return {
        id: text(row.id, `${at}.id`),
        ...(row.limit_window_seconds !== undefined
          ? { limit_window_seconds: number(row.limit_window_seconds, at, 1) }
          : {}),
        ...(row.reset_at !== undefined
          ? { reset_at: date(row.reset_at, at) }
          : {}),
      };
    },
  );
  if (
    windows.length < 1 ||
    windows.length > 32 ||
    new Set(windows.map((w) => w.id)).size !== windows.length
  )
    invalid(path);
  return {
    service_id: serviceID,
    observed_at: date(input.observed_at, `${path}.observed_at`),
    source: input.source as QuotaCooldown["source"],
    windows,
  };
}

export function parseAccountPool(value: unknown, path = "$"): AccountPool {
  const input = object(value, path);
  fields(
    input,
    [
      "id",
      "name",
      "provider",
      "enabled",
      "sticky",
      "queue",
      "members",
      "created_at",
      "updated_at",
      "etag",
      "runtime",
    ],
    ["proxy"],
    path,
  );
  const poolID = id(input.id, `${path}.id`),
    name = text(input.name, `${path}.name`);
  if (
    name.trim() !== name ||
    ["\0", "\r", "\n"].some((character) => name.includes(character)) ||
    !Object.hasOwn(poolProviderNames, String(input.provider))
  )
    invalid(path);
  const sticky = object(input.sticky, `${path}.sticky`);
  fields(sticky, ["enabled", "ttl_seconds"], [], path);
  const queue = object(input.queue, `${path}.queue`);
  fields(queue, ["max_waiting_requests", "timeout_ms"], [], path);
  const members = array(input.members, `${path}.members`).map(
    (value, index) => {
      const at = `${path}.members[${index}]`,
        row = object(value, at);
      fields(
        row,
        ["pool_id", "service_id", "enabled", "max_concurrent_requests"],
        [],
        at,
      );
      if (row.pool_id !== poolID) invalid(at);
      return {
        pool_id: poolID,
        service_id: id(row.service_id, `${at}.service_id`),
        enabled: bool(row.enabled, at),
        max_concurrent_requests: number(
          row.max_concurrent_requests,
          at,
          1,
          1024,
        ),
      };
    },
  );
  const memberIDs = new Set(members.map((m) => m.service_id));
  if (memberIDs.size !== members.length) invalid(`${path}.members`);
  let proxy: ServiceProxy | undefined;
  if (input.proxy !== undefined) {
    const row = object(input.proxy, `${path}.proxy`);
    fields(row, ["mode"], ["url", "credential_ref"], path);
    if (
      row.mode !== "inherit" &&
      row.mode !== "direct" &&
      row.mode !== "custom"
    )
      invalid(`${path}.proxy.mode`);
    if (
      row.mode !== "custom" &&
      (row.url !== undefined || row.credential_ref !== undefined)
    )
      invalid(`${path}.proxy`);
    if (
      row.mode === "custom" &&
      (!validProxyURL(String(row.url)) ||
        (row.credential_ref !== undefined &&
          row.credential_ref !== `local://pool-proxy/${poolID}`))
    )
      invalid(`${path}.proxy`);
    proxy = {
      mode: row.mode,
      ...(row.url !== undefined ? { url: String(row.url) } : {}),
      ...(row.credential_ref !== undefined
        ? { credential_ref: String(row.credential_ref) }
        : {}),
    };
  }
  const status = object(input.runtime, `${path}.runtime`);
  fields(
    status,
    ["active_requests", "waiting_requests", "available_accounts", "members"],
    [],
    path,
  );
  const states: PoolMemberStatus["state"][] = [
    "ready",
    "disabled",
    "disconnected",
    "paused",
    "quota_cooldown",
    "unknown",
  ];
  const runtimeMembers = array(status.members, `${path}.runtime.members`).map(
    (value, index): PoolMemberStatus => {
      const at = `${path}.runtime.members[${index}]`,
        row = object(value, at);
      fields(
        row,
        ["service_id", "state", "active_requests", "proxy_source"],
        ["quota_cooldown", "usage_observed_at"],
        at,
      );
      const serviceID = id(row.service_id, at);
      if (
        !memberIDs.has(serviceID) ||
        !states.includes(row.state as PoolMemberStatus["state"]) ||
        !["global", "pool", "account"].includes(String(row.proxy_source))
      )
        invalid(at);
      return {
        service_id: serviceID,
        ...(row.usage_observed_at !== undefined
          ? {
              usage_observed_at: date(
                row.usage_observed_at,
                `${at}.usage_observed_at`,
              ),
            }
          : {}),
        state: row.state as PoolMemberStatus["state"],
        active_requests: number(row.active_requests, at),
        proxy_source: row.proxy_source as PoolMemberStatus["proxy_source"],
        ...(row.quota_cooldown !== undefined
          ? { quota_cooldown: cooldown(row.quota_cooldown, serviceID, at) }
          : {}),
      };
    },
  );
  if (
    runtimeMembers.length !== members.length ||
    new Set(runtimeMembers.map((m) => m.service_id)).size !== members.length
  )
    invalid(`${path}.runtime.members`);
  const etag = text(input.etag, `${path}.etag`, 3, 128);
  if (!/^"[^"\r\n]+"$/.test(etag)) invalid(`${path}.etag`);
  return {
    id: poolID,
    name,
    provider: input.provider as SubscriptionProvider,
    enabled: bool(input.enabled, path),
    sticky: {
      enabled: bool(sticky.enabled, path),
      ttl_seconds: number(sticky.ttl_seconds, path, 60, 86400),
    },
    queue: {
      max_waiting_requests: number(queue.max_waiting_requests, path, 0, 128),
      timeout_ms: number(queue.timeout_ms, path, 100, 60000),
    },
    ...(proxy ? { proxy } : {}),
    members,
    created_at: date(input.created_at, path),
    updated_at: date(input.updated_at, path),
    etag,
    runtime: {
      active_requests: number(status.active_requests, path),
      waiting_requests: number(status.waiting_requests, path),
      available_accounts: number(
        status.available_accounts,
        path,
        0,
        members.length,
      ),
      members: runtimeMembers,
    },
  };
}

export function parseAccountPoolPage(value: unknown): AccountPool[] {
  const page = object(value, "$");
  fields(page, ["items"], [], "$");
  const pools = array(page.items, "$.items").map((value, index) =>
    parseAccountPool(value, `$.items[${index}]`),
  );
  if (new Set(pools.map((p) => p.id)).size !== pools.length) invalid("$.items");
  return pools;
}

export function poolServiceKind(
  provider: SubscriptionProvider,
): SubscriptionServiceKind {
  const kind = (
    Object.keys(subscriptionKindProviders) as SubscriptionServiceKind[]
  ).find((kind) => subscriptionKindProviders[kind] === provider);
  if (!kind) throw new Error("Unsupported pool provider");
  return kind;
}

export function canJoinPool(
  service: Service,
  pool: AccountPool,
  pools: readonly AccountPool[],
): boolean {
  return (
    service.kind === poolServiceKind(pool.provider) &&
    service.subscription?.provider === pool.provider &&
    !(
      pool.provider === "claude_code" &&
      service.subscription.mode === "official"
    ) &&
    !pools.some(
      (other) =>
        other.id !== pool.id &&
        other.members.some((member) => member.service_id === service.id),
    )
  );
}
