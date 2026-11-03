// @vitest-environment happy-dom
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { settleClosedLayer } from "./lib/test-layer-settle";
const mocks = vi.hoisted(() => ({
  listAccountPools: vi.fn(),
  createAccountPool: vi.fn(),
  updateAccountPool: vi.fn(),
  replaceAccountPoolMembers: vi.fn(),
  deleteAccountPool: vi.fn(),
  probeAccountPoolQuota: vi.fn(),
  probeServiceProxy: vi.fn(),
  createService: vi.fn(),
}));
vi.mock("./bridge", () => mocks);
import { AccountPoolManager } from "./AccountPoolManager";
import { testAccountPool, testPoolAccount } from "./account-pool-test-data";
import type { Service } from "./service-model";
function button(
  label: string,
  scope: ParentNode = document,
): HTMLButtonElement {
  const found = [...scope.querySelectorAll<HTMLButtonElement>("button")].find(
    (button) =>
      button.textContent?.trim() === label ||
      button.getAttribute("aria-label") === label,
  );
  if (!found) throw new Error(`Missing button ${label}`);
  return found;
}
async function click(label: string, scope?: ParentNode) {
  await act(async () => button(label, scope).click());
}
async function input(label: string, value: string) {
  const element = document.querySelector<HTMLInputElement>(
    `input[aria-label="${label}"]`,
  );
  if (!element) throw new Error(`Missing input ${label}`);
  await act(async () => {
    Object.getOwnPropertyDescriptor(
      HTMLInputElement.prototype,
      "value",
    )!.set!.call(element, value);
    element.dispatchEvent(new Event("input", { bubbles: true }));
  });
}
async function select(label: string) {
  await act(async () =>
    button(label).dispatchEvent(
      new KeyboardEvent("keydown", { key: "Enter", bubbles: true }),
    ),
  );
}
describe("AccountPoolManager", () => {
  let root: Root, container: HTMLDivElement;
  const onOpenAccount = vi.fn(),
    onServiceSaved = vi.fn(),
    onDirtyChange = vi.fn();
  const pool = testAccountPool();
  beforeEach(() => {
    (
      globalThis as typeof globalThis & { IS_REACT_ACT_ENVIRONMENT: boolean }
    ).IS_REACT_ACT_ENVIRONMENT = true;
    vi.clearAllMocks();
    mocks.listAccountPools.mockResolvedValue([pool]);
    container = document.createElement("div");
    document.body.append(container);
    root = createRoot(container);
  });
  afterEach(async () => {
    await act(async () => root.unmount());
    container.remove();
    vi.restoreAllMocks();
  });
  async function render(services: Service[] = []) {
    await act(async () => {
      root.render(
        <AccountPoolManager
          isReady
          coreSessionKey="test-session"
          services={services}
          onOpenAccount={onOpenAccount}
          onServiceSaved={onServiceSaved}
          onDirtyChange={onDirtyChange}
        />,
      );
    });
  }
  async function open() {
    await act(async () =>
      container
        .querySelector<HTMLButtonElement>(
          '[data-testid="pool-primary-region"] button',
        )!
        .click(),
    );
  }
  it("creates Claude accounts in direct mode and joins them before opening sign-in", async () => {
    const service = testPoolAccount("service_new", {
      subscription: {
        provider: "claude_code",
        status: "disconnected",
        mode: "legacy",
      },
    });
    mocks.createService.mockResolvedValue({ service, etag: '"service-v1"' });
    mocks.replaceAccountPoolMembers.mockResolvedValue({
      ...pool,
      etag: '"pool-v2"',
      members: [
        {
          pool_id: pool.id,
          service_id: service.id,
          enabled: true,
          max_concurrent_requests: 2,
        },
      ],
    });
    await render();
    await open();
    await click("创建并加入账号");
    await input("账号名称", "新账号");
    await click("创建并加入账号", document.querySelector('[role="dialog"]')!);
    expect(mocks.createService).toHaveBeenCalledWith({
      name: "新账号",
      kind: "claude_subscription",
      subscription_mode: "legacy",
    });
    expect(mocks.replaceAccountPoolMembers).toHaveBeenCalledWith(
      pool.id,
      [{ service_id: service.id, enabled: true, max_concurrent_requests: 2 }],
      pool.etag,
    );
    expect(onServiceSaved).toHaveBeenCalledWith(service);
    expect(onOpenAccount).toHaveBeenCalledWith(service.id);
  });
  it("filters member choices and requires saving the atomic replacement", async () => {
    const eligible = testPoolAccount("service_direct");
    const official = testPoolAccount("service_cli", {
      subscription: {
        provider: "claude_code",
        status: "connected",
        mode: "official",
      },
    });
    const owned = testPoolAccount("service_owned");
    const other = testAccountPool({
      id: "pool_other",
      members: [
        {
          pool_id: "pool_other",
          service_id: owned.id,
          enabled: true,
          max_concurrent_requests: 2,
        },
      ],
    });
    mocks.listAccountPools.mockResolvedValue([pool, other]);
    mocks.replaceAccountPoolMembers.mockResolvedValue({
      ...pool,
      etag: '"pool-v2"',
      members: [
        {
          pool_id: pool.id,
          service_id: eligible.id,
          enabled: true,
          max_concurrent_requests: 2,
        },
      ],
    });
    await render([eligible, official, owned]);
    await open();
    await click("添加现有账号");
    await select("选择账号");
    const options = [
      ...document.querySelectorAll<HTMLElement>('[role="option"]'),
    ];
    expect(options.map((o) => o.textContent)).toEqual([eligible.name]);
    await act(async () => options[0].click());
    await settleClosedLayer();
    await click("添加现有账号", document.querySelector('[role="dialog"]')!);
    await settleClosedLayer();
    expect(onDirtyChange).toHaveBeenLastCalledWith(true);
    expect(mocks.replaceAccountPoolMembers).not.toHaveBeenCalled();
    await click("保存");
    expect(mocks.replaceAccountPoolMembers).toHaveBeenCalledWith(
      pool.id,
      [{ service_id: eligible.id, enabled: true, max_concurrent_requests: 2 }],
      pool.etag,
    );
    expect(onDirtyChange).toHaveBeenLastCalledWith(false);
  });
  it("confirms disabling and keeps cancellation from changing the draft", async () => {
    await render();
    await open();
    await act(async () => button("设置").focus());
    const enabled = document.querySelector<HTMLButtonElement>(
      '[data-testid="pool-settings"] [role="checkbox"]',
    )!;
    await act(async () => enabled.click());
    expect(
      document.querySelector('[role="alertdialog"]')?.textContent,
    ).toContain("保存后，此号池不再接收新请求");
    await click("取消", document.querySelector('[role="alertdialog"]')!);
    await settleClosedLayer();
    expect(enabled.getAttribute("aria-checked")).toBe("true");
    await act(async () => enabled.click());
    await click("停用", document.querySelector('[role="alertdialog"]')!);
    await settleClosedLayer();
    expect(enabled.getAttribute("aria-checked")).toBe("false");
    expect(onDirtyChange).toHaveBeenLastCalledWith(true);
    expect(mocks.updateAccountPool).not.toHaveBeenCalled();
  });

  it("keeps edits after a conditional write conflict and guards back navigation", async () => {
    mocks.updateAccountPool.mockRejectedValue(
      new Error("configuration changed"),
    );
    await render();
    await open();
    await act(async () => button("设置").focus());
    await input("号池名称", "changed");
    await click("保存");
    expect(container.textContent).toContain("configuration changed");
    expect(onDirtyChange).toHaveBeenLastCalledWith(true);
    await click("全部号池");
    expect(document.querySelector('[role="alertdialog"]')).not.toBeNull();
    await click("取消", document.querySelector('[role="alertdialog"]')!);
    await settleClosedLayer();
    expect(
      document.querySelector<HTMLInputElement>('input[aria-label="号池名称"]')
        ?.value,
    ).toBe("changed");
  });
});
