import { Label } from "./components/ui/label";
import { useCallback, useEffect, useRef, useState } from "react";
import { useT, currentLocale } from "./i18n";
import { PageHeader } from "./PageHeader";
import {
  createAccountPool,
  createService,
  deleteAccountPool,
  listAccountPools,
  probeAccountPoolQuota,
  probeServiceProxy,
  replaceAccountPoolMembers,
  updateAccountPool,
} from "./bridge";
import {
  canJoinPool,
  poolProviderNames,
  poolServiceKind,
  type AccountPool,
  type AccountPoolMemberInput,
} from "./account-pool-model";
import type { Service } from "./service-model";
import type { SubscriptionProvider } from "./subscription-model";
import {
  proxyDraft,
  proxyInput,
  serviceProxyTestTarget,
  validProxyDraft,
} from "./service-proxy-model";
import { controlErrorCode, controlErrorMessage } from "./control-error";
import { notify } from "./notify";
import { Panel, PanelBody } from "./components/Panel";
import { Field } from "./components/Field";
import { EmptyState } from "./components/EmptyState";
import { FormMessage } from "./components/FormMessage";
import { ListToolbar } from "./components/ListToolbar";
import { HelpPopover } from "./components/HelpPopover";
import { ConfirmDialog } from "./components/ConfirmDialog";
import { ServiceProxyFields } from "./components/ServiceProxyFields";
import { Button } from "./components/ui/button";
import { Input } from "./components/ui/input";
import { Checkbox } from "./components/ui/checkbox";
import { Badge } from "./components/ui/badge";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "./components/ui/select";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "./components/ui/tabs";
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
} from "./components/ui/dialog";
import {
  Plus,
  RefreshCw,
  Shredder as Trash2,
  Server,
} from "./components/icons";
import { useExitSnapshot } from "./lib/exit-snapshot";

function failure(error: unknown, fallback: string): string {
  return controlErrorMessage(error, fallback);
}
function memberInputs(pool: AccountPool): AccountPoolMemberInput[] {
  return pool.members.map(
    ({ service_id, enabled, max_concurrent_requests }) => ({
      service_id,
      enabled,
      max_concurrent_requests,
    }),
  );
}
function settingsDraft(pool: AccountPool) {
  return {
    name: pool.name,
    enabled: pool.enabled,
    sticky: { ...pool.sticky },
    queue: { ...pool.queue },
    proxy: proxyDraft(pool.proxy),
  };
}

export function AccountPoolManager({
  isReady,
  coreSessionKey,
  services,
  onOpenAccount,
  onServiceSaved,
  onDirtyChange,
}: {
  isReady: boolean;
  coreSessionKey: string | null;
  services: Service[];
  onOpenAccount: (id: string) => void;
  onServiceSaved: (service: Service) => void;
  onDirtyChange: (dirty: boolean) => void;
}) {
  const t = useT();
  const [pools, setPools] = useState<AccountPool[]>([]);
  const [loaded, setLoaded] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [revision, setRevision] = useState(0);
  const [query, setQuery] = useState("");
  const [createOpen, setCreateOpen] = useState(false);
  const [name, setName] = useState("");
  const [provider, setProvider] = useState<SubscriptionProvider>("claude_code");
  const [busy, setBusy] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);
  const [pending, setPending] = useState<(() => void) | null>(null);
  const dirty = useRef(false);
  const generation = useRef(0);
  const session = useRef(0);
  useEffect(() => {
    session.current++;
    return () => {
      session.current++;
    };
  }, [coreSessionKey]);
  const refresh = useCallback(
    async (silent = false) => {
      if (!isReady) return;
      const current = ++generation.current;
      try {
        const result = await listAccountPools();
        if (current !== generation.current) return;
        setPools(result);
        setLoaded(true);
        if (!silent) setError(null);
      } catch (e) {
        if (current === generation.current && !silent)
          setError(failure(e, t("pools.loadFailed")));
      }
    },
    [isReady, t],
  );
  useEffect(() => {
    setPools([]);
    setLoaded(false);
    setSelected(null);
    setError(null);
    setCreateOpen(false);
    setBusy(false);
    void refresh();
    const timer = setInterval(() => void refresh(true), 5000);
    return () => {
      generation.current++;
      clearInterval(timer);
    };
  }, [coreSessionKey, refresh]);
  useEffect(() => () => onDirtyChange(false), [onDirtyChange]);
  const changeDirty = useCallback(
    (value: boolean) => {
      dirty.current = value;
      onDirtyChange(value);
    },
    [onDirtyChange],
  );
  const leave = (action: () => void) => {
    if (dirty.current) setPending(() => action);
    else action();
  };
  const upsert = (pool: AccountPool) => {
    generation.current++;
    setError(null);
    setPools((current) => [
      ...current.filter((item) => item.id !== pool.id),
      pool,
    ]);
  };
  const pool = pools.find((item) => item.id === selected);
  const visible = pools.filter((pool) =>
    `${pool.name} ${poolProviderNames[pool.provider]}`
      .toLocaleLowerCase()
      .includes(query.toLocaleLowerCase()),
  );
  const shownName = useExitSnapshot(name, createOpen);
  const shownProvider = useExitSnapshot(provider, createOpen);
  return (
    <section
      className="@container flex min-h-0 flex-1 flex-col"
      data-testid="account-pools"
    >
      {pool && error ? <FormMessage tone="error">{error}</FormMessage> : null}
      {pool ? (
        <PoolEditor
          key={`${coreSessionKey}:${pool.id}:${revision}`}
          pool={pool}
          pools={pools}
          services={services}
          isReady={isReady}
          onDirtyChange={changeDirty}
          onUpdated={upsert}
          onOpenAccount={onOpenAccount}
          onServiceSaved={onServiceSaved}
          onBack={() =>
            leave(() => {
              setSelected(null);
              changeDirty(false);
            })
          }
          onRefresh={() =>
            leave(() => {
              void refresh().then(() => setRevision((value) => value + 1));
            })
          }
          onDeleted={() => {
            setSelected(null);
            changeDirty(false);
            void refresh();
          }}
        />
      ) : (
        <>
          <PageHeader
            title={t("pools.title")}
            actions={
              <Button
                size="sm"
                disabled={!isReady}
                onClick={() => {
                  setName("");
                  setCreateError(null);
                  setCreateOpen(true);
                }}
              >
                <Plus className="size-3.5" />
                {t("pools.create")}
              </Button>
            }
          />
          <ListToolbar
            title={t("pools.members")}
            count={pools.reduce((sum, pool) => sum + pool.members.length, 0)}
            query={query}
            onQueryChange={setQuery}
            searchLabel={t("pools.search")}
            placeholder={t("pools.search")}
            clearLabel={t("common.clearSearch")}
            help={{
              label: t("pools.title"),
              content: <p>{t("pools.description")}</p>,
            }}
            actions={
              <Button
                size="icon-sm"
                variant="ghost"
                aria-label={t("common.refresh")}
                disabled={!isReady}
                onClick={() => void refresh()}
              >
                <RefreshCw className="size-4" />
              </Button>
            }
          />
          {error ? (
            <FormMessage tone="error" className="mt-2">
              {error}
            </FormMessage>
          ) : null}
          <Panel className="mt-3 flex min-h-0 flex-1 flex-col">
            <PanelBody className="p-0" data-testid="pool-primary-region">
              {!loaded ? (
                <p className="p-4 text-sm text-muted-foreground">
                  {t("common.loading")}
                </p>
              ) : visible.length === 0 ? (
                <EmptyState
                  illustration={<Server />}
                  title={t(query ? "pools.noMatches" : "pools.empty")}
                  description={t(
                    query ? "pools.noMatchesHint" : "pools.emptyHint",
                  )}
                />
              ) : (
                visible.map((item) => (
                  <Button
                    key={item.id}
                    variant="ghost"
                    className="h-auto w-full justify-between gap-4 rounded-none border-b p-4 text-left"
                    onClick={() => setSelected(item.id)}
                  >
                    <span className="grid min-w-0 gap-1">
                      <strong className="truncate text-sm">{item.name}</strong>
                      <span className="text-xs text-muted-foreground">
                        {poolProviderNames[item.provider]} ·{" "}
                        {t("pools.memberCount", { count: item.members.length })}
                        {!item.enabled ? ` · ${t("pools.disabled")}` : ""}
                      </span>
                    </span>
                    <span className="flex shrink-0 flex-wrap gap-x-4 gap-y-1 text-xs tabular-nums text-muted-foreground">
                      <span>
                        {t("pools.active")} {item.runtime.active_requests}
                      </span>
                      <span>
                        {t("pools.waiting")} {item.runtime.waiting_requests}
                      </span>
                      <span>
                        {t("pools.available")} {item.runtime.available_accounts}
                      </span>
                    </span>
                  </Button>
                ))
              )}
            </PanelBody>
          </Panel>
        </>
      )}
      <Dialog
        open={createOpen}
        onOpenChange={(open) => {
          if (!busy) setCreateOpen(open);
        }}
      >
        <DialogContent showCloseButton={!busy}>
          <DialogHeader>
            <DialogTitle>{t("pools.create")}</DialogTitle>
            <DialogDescription>{t("pools.emptyHint")}</DialogDescription>
          </DialogHeader>
          <form
            onSubmit={(event) => {
              event.preventDefault();
              if (busy || !isReady || !name.trim()) return;
              setBusy(true);
              setCreateError(null);
              const current = session.current;
              void createAccountPool({ name: name.trim(), provider })
                .then((result) => {
                  if (current !== session.current) return;
                  upsert(result);
                  setSelected(result.id);
                  setCreateOpen(false);
                  notify.success(t("pools.created"));
                })
                .catch((e) => {
                  if (current === session.current)
                    setCreateError(failure(e, t("pools.saveFailed")));
                })
                .finally(() => {
                  if (current === session.current) setBusy(false);
                });
            }}
          >
            <div className="grid gap-4">
              <Field label={t("pools.name")}>
                <Input
                  autoFocus
                  aria-label={t("pools.name")}
                  maxLength={128}
                  value={shownName}
                  onChange={(e) => setName(e.target.value)}
                  disabled={busy}
                />
              </Field>
              <Field label={t("pools.provider")}>
                <Select
                  value={shownProvider}
                  onValueChange={(value) =>
                    setProvider(value as SubscriptionProvider)
                  }
                  disabled={busy}
                >
                  <SelectTrigger
                    aria-label={t("pools.provider")}
                    className="w-full"
                  >
                    <SelectValue />
                  </SelectTrigger>
                  <SelectContent>
                    {Object.entries(poolProviderNames).map(([value, label]) => (
                      <SelectItem key={value} value={value}>
                        {label}
                      </SelectItem>
                    ))}
                  </SelectContent>
                </Select>
              </Field>
              {createError ? (
                <FormMessage tone="error">{createError}</FormMessage>
              ) : null}
            </div>
            <DialogFooter className="mt-5">
              <Button
                type="button"
                variant="outline"
                disabled={busy}
                onClick={() => setCreateOpen(false)}
              >
                {t("common.cancel")}
              </Button>
              <Button type="submit" disabled={busy || !name.trim()}>
                {t("pools.create")}
              </Button>
            </DialogFooter>
          </form>
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={pending !== null}
        title={t("pools.unsavedTitle")}
        description={t("pools.unsavedHint")}
        confirmLabel={t("common.discardAndLeave")}
        onCancel={() => setPending(null)}
        onConfirm={() => {
          const action = pending;
          setPending(null);
          action?.();
        }}
      />
    </section>
  );
}

function PoolEditor({
  pool,
  pools,
  services,
  isReady,
  onDirtyChange,
  onUpdated,
  onBack,
  onRefresh,
  onDeleted,
  onOpenAccount,
  onServiceSaved,
}: {
  pool: AccountPool;
  pools: AccountPool[];
  services: Service[];
  isReady: boolean;
  onDirtyChange: (dirty: boolean) => void;
  onUpdated: (pool: AccountPool) => void;
  onBack: () => void;
  onRefresh: () => void;
  onDeleted: () => void;
  onOpenAccount: (id: string) => void;
  onServiceSaved: (service: Service) => void;
}) {
  const t = useT();
  const [base, setBase] = useState(pool);
  const [draft, setDraft] = useState(() => settingsDraft(pool));
  const [members, setMembers] = useState(() => memberInputs(pool));
  const [query, setQuery] = useState("");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [addOpen, setAddOpen] = useState(false);
  const [newOpen, setNewOpen] = useState(false);
  const [newName, setNewName] = useState("");
  const [candidate, setCandidate] = useState("");
  const [pending, setPending] = useState<
    | { kind: "delete" }
    | { kind: "disablePool" }
    | { kind: "remove" | "probe" | "disableMember"; id: string }
    | null
  >(null);
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);
  const settingsChanged =
    JSON.stringify(draft) !== JSON.stringify(settingsDraft(base));
  const membersChanged =
    JSON.stringify(members) !== JSON.stringify(memberInputs(base));
  const dirty = settingsChanged || membersChanged;
  useEffect(() => {
    onDirtyChange(dirty);
    return () => onDirtyChange(false);
  }, [dirty, onDirtyChange]);
  const accept = (result: AccountPool) => {
    setBase(result);
    onUpdated(result);
  };
  const displayError = (e: unknown) =>
    setError(
      controlErrorCode(e) === "precondition_failed"
        ? t("pools.refreshConflict")
        : failure(e, t("pools.saveFailed")),
    );
  const valid =
    !!draft.name.trim() &&
    Number.isInteger(draft.sticky.ttl_seconds) &&
    draft.sticky.ttl_seconds >= 60 &&
    draft.sticky.ttl_seconds <= 86400 &&
    Number.isInteger(draft.queue.max_waiting_requests) &&
    draft.queue.max_waiting_requests >= 0 &&
    draft.queue.max_waiting_requests <= 128 &&
    Number.isInteger(draft.queue.timeout_ms) &&
    draft.queue.timeout_ms >= 100 &&
    draft.queue.timeout_ms <= 60000 &&
    validProxyDraft(draft.proxy) &&
    members.every(
      (m) =>
        Number.isInteger(m.max_concurrent_requests) &&
        m.max_concurrent_requests >= 1 &&
        m.max_concurrent_requests <= 1024,
    );
  const save = async () => {
    if (!valid || busy || !isReady) return;
    setBusy(true);
    setError(null);
    try {
      let current = base;
      if (settingsChanged) {
        current = await updateAccountPool(
          base.id,
          {
            name: draft.name.trim(),
            enabled: draft.enabled,
            sticky: draft.sticky,
            queue: draft.queue,
            proxy: proxyInput(draft.proxy),
          },
          base.etag,
        );
        if (!mounted.current) return;
        accept(current);
        setDraft(settingsDraft(current));
      }
      if (membersChanged) {
        current = await replaceAccountPoolMembers(
          base.id,
          members,
          current.etag,
        );
        if (!mounted.current) return;
        accept(current);
        setMembers(memberInputs(current));
      }
      notify.success(t("pools.saved"));
    } catch (e) {
      if (mounted.current) displayError(e);
    } finally {
      if (mounted.current) setBusy(false);
    }
  };
  const candidates = services.filter(
    (service) =>
      canJoinPool(service, pool, pools) &&
      !members.some((member) => member.service_id === service.id),
  );
  const visible = members.filter((member) =>
    `${services.find((service) => service.id === member.service_id)?.name ?? ""} ${member.service_id}`
      .toLocaleLowerCase()
      .includes(query.toLocaleLowerCase()),
  );
  const updateMember = (id: string, patch: Partial<AccountPoolMemberInput>) =>
    setMembers((current) =>
      current.map((member) =>
        member.service_id === id ? { ...member, ...patch } : member,
      ),
    );
  const proxyTarget = serviceProxyTestTarget(poolServiceKind(pool.provider));
  const shownPending = useExitSnapshot(pending, pending !== null);
  const shownNewName = useExitSnapshot(newName, newOpen);
  const confirmation = {
    delete: {
      title: "pools.deleteTitle",
      description: "pools.deleteHint",
      confirm: "common.delete",
    },
    remove: {
      title: "pools.removeMemberTitle",
      description: "pools.removeMemberHint",
      confirm: "pools.removeMember",
    },
    probe: {
      title: "pools.quotaProbeTitle",
      description: "pools.quotaProbeHint",
      confirm: "pools.quotaProbe",
    },
    disablePool: {
      title: "pools.disablePoolTitle",
      description: "pools.disablePoolHint",
      confirm: "pools.disableConfirm",
    },
    disableMember: {
      title: "pools.disableMemberTitle",
      description: "pools.disableMemberHint",
      confirm: "pools.disableConfirm",
    },
  }[shownPending?.kind ?? "remove"];

  return (
    <>
      <PageHeader
        title={pool.name}
        back={{
          label: t("pools.back"),
          onClick: () => {
            if (!busy) onBack();
          },
        }}
        titleSuffix={
          <Badge variant="secondary">{poolProviderNames[pool.provider]}</Badge>
        }
        actions={
          <>
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={t("common.refresh")}
              disabled={busy || !isReady}
              onClick={onRefresh}
            >
              <RefreshCw className="size-4" />
            </Button>
            <Button
              size="sm"
              disabled={!dirty || busy || !valid || !isReady}
              onClick={() => void save()}
            >
              {t("common.save")}
            </Button>
            <Button
              size="icon-sm"
              variant="ghost"
              aria-label={t("pools.deleteTitle")}
              disabled={busy || !isReady}
              onClick={() => setPending({ kind: "delete" })}
            >
              <Trash2 className="size-4" />
            </Button>
          </>
        }
      />
      {!valid && dirty ? (
        <FormMessage tone="error" className="mb-2">
          {t("pools.invalidSettings")}
        </FormMessage>
      ) : null}
      {error ? (
        <FormMessage tone="error" className="mb-2">
          {error}
        </FormMessage>
      ) : null}
      <Tabs defaultValue="members" className="min-h-0 flex-1">
        <div className="flex shrink-0 flex-wrap items-center justify-between gap-2">
          <TabsList>
            <TabsTrigger value="members">
              {t("pools.members")} · {members.length}
            </TabsTrigger>
            <TabsTrigger value="settings">{t("pools.settings")}</TabsTrigger>
          </TabsList>
          <span className="text-xs tabular-nums text-muted-foreground">
            {t("pools.active")} {pool.runtime.active_requests} ·{" "}
            {t("pools.waiting")} {pool.runtime.waiting_requests}
          </span>
        </div>
        <TabsContent
          value="members"
          className="flex min-h-0 flex-1 flex-col gap-3 overflow-hidden"
        >
          <ListToolbar
            title={t("pools.members")}
            count={members.length}
            query={query}
            onQueryChange={setQuery}
            searchLabel={t("pools.searchMembers")}
            placeholder={t("pools.searchMembers")}
            clearLabel={t("common.clearSearch")}
            filters={
              <span className="text-xs tabular-nums text-muted-foreground">
                {t("pools.available")} {pool.runtime.available_accounts}
              </span>
            }
            help={{
              label: t("pools.members"),
              content: <p>{t("pools.noEligibleHint")}</p>,
            }}
            actions={
              <>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy || !isReady}
                  onClick={() => {
                    setCandidate("");
                    setAddOpen(true);
                  }}
                >
                  {t("pools.addMember")}
                </Button>
                <Button
                  size="sm"
                  variant="outline"
                  disabled={busy || dirty || !isReady}
                  onClick={() => {
                    setNewName("");
                    setNewOpen(true);
                  }}
                >
                  {t("pools.createAccount")}
                </Button>
              </>
            }
          />
          <Panel className="flex min-h-0 flex-1 flex-col">
            <PanelBody className="p-0" data-testid="pool-primary-region">
              {visible.length === 0 ? (
                <EmptyState
                  illustration={<Server />}
                  title={t(query ? "pools.noMatches" : "pools.emptyMembers")}
                  description={t(
                    query ? "pools.noMatchesHint" : "pools.emptyMembersHint",
                  )}
                />
              ) : (
                visible.map((member) => {
                  const service = services.find(
                    (service) => service.id === member.service_id,
                  );
                  const status = pool.runtime.members.find(
                    (state) => state.service_id === member.service_id,
                  );
                  return (
                    <article
                      key={member.service_id}
                      className="grid min-w-0 gap-3 border-b p-3 @min-[640px]:grid-cols-[minmax(0,1fr)_auto]"
                      data-testid="pool-member"
                    >
                      <div className="grid min-w-0 gap-1">
                        <div className="flex min-w-0 flex-wrap items-center gap-2">
                          <Button
                            variant="link"
                            className="h-auto min-w-0 truncate p-0 text-sm font-semibold"
                            disabled={busy || dirty}
                            onClick={() => onOpenAccount(member.service_id)}
                          >
                            {service?.name ?? member.service_id}
                          </Button>
                          <Badge variant="secondary">
                            {t(`pools.states.${status?.state ?? "unknown"}`)}
                          </Badge>
                          <HelpPopover label={t("pools.quotaSnapshot")}>
                            <p>
                              {status?.usage_observed_at
                                ? t("pools.quotaObserved", {
                                    time: new Date(
                                      status.usage_observed_at,
                                    ).toLocaleString(currentLocale()),
                                  })
                                : t("pools.quotaUnobserved")}
                            </p>
                          </HelpPopover>
                        </div>
                        <span className="text-xs text-muted-foreground">
                          {t("pools.active")} {status?.active_requests ?? 0} ·{" "}
                          {t("pools.proxySource")}:{" "}
                          {t(
                            status?.proxy_source === "account"
                              ? "pools.proxyAccount"
                              : status?.proxy_source === "pool"
                                ? "pools.proxyPool"
                                : "pools.proxyGlobal",
                          )}
                        </span>
                        {status?.quota_cooldown ? (
                          <div className="flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                            <HelpPopover label={t("pools.quota")}>
                              <p>
                                {t(
                                  status.quota_cooldown.source === "usage"
                                    ? "pools.quotaByUsage"
                                    : "pools.quotaByUpstream",
                                )}
                              </p>
                              <p>
                                {t("pools.quotaObserved", {
                                  time: new Date(
                                    status.quota_cooldown.observed_at,
                                  ).toLocaleString(currentLocale()),
                                })}
                              </p>
                              {status.quota_cooldown.windows.map((window) => (
                                <p key={window.id}>
                                  {t(
                                    window.limit_window_seconds === 18000
                                      ? "pools.shortWindow"
                                      : window.limit_window_seconds === 604800
                                        ? "pools.weeklyWindow"
                                        : "pools.accountWindow",
                                  )}{" "}
                                  ·{" "}
                                  {window.reset_at
                                    ? t("pools.quotaReset", {
                                        time: new Date(
                                          window.reset_at,
                                        ).toLocaleString(currentLocale()),
                                      })
                                    : t("pools.quotaUnknown")}
                                </p>
                              ))}
                            </HelpPopover>
                            <Button
                              size="sm"
                              variant="link"
                              className="h-auto p-0 text-xs"
                              disabled={busy || !isReady}
                              onClick={() =>
                                setPending({
                                  kind: "probe",
                                  id: member.service_id,
                                })
                              }
                            >
                              {t("pools.quotaProbe")}
                            </Button>
                          </div>
                        ) : null}
                      </div>
                      <div className="flex flex-wrap items-center gap-3">
                        <Label className="flex items-center gap-2 text-xs">
                          <Checkbox
                            aria-label={`${service?.name ?? member.service_id} · ${t("pools.memberEnabled")}`}
                            checked={member.enabled}
                            disabled={busy}
                            onCheckedChange={(value) =>
                              value === true
                                ? updateMember(member.service_id, {
                                    enabled: true,
                                  })
                                : setPending({
                                    kind: "disableMember",
                                    id: member.service_id,
                                  })
                            }
                          />
                          {t("pools.memberEnabled")}
                        </Label>
                        <Label className="flex items-center gap-2 text-xs">
                          {t("pools.limit")}
                          <Input
                            className="h-8 w-20"
                            type="number"
                            min={1}
                            max={1024}
                            aria-label={`${service?.name ?? member.service_id} · ${t("pools.limit")}`}
                            value={member.max_concurrent_requests}
                            disabled={busy}
                            onChange={(e) =>
                              updateMember(member.service_id, {
                                max_concurrent_requests: Number(e.target.value),
                              })
                            }
                          />
                        </Label>
                        <Button
                          size="icon-sm"
                          variant="ghost"
                          disabled={busy}
                          aria-label={`${t("pools.removeMember")} · ${service?.name ?? member.service_id}`}
                          onClick={() =>
                            setPending({
                              kind: "remove",
                              id: member.service_id,
                            })
                          }
                        >
                          <Trash2 className="size-4" />
                        </Button>
                      </div>
                    </article>
                  );
                })
              )}
            </PanelBody>
          </Panel>
        </TabsContent>
        <TabsContent
          value="settings"
          className="min-h-0 flex-1 overflow-y-auto"
          data-testid="pool-settings"
          data-tab-scroller
        >
          <fieldset
            disabled={busy || !isReady}
            className="grid min-w-0 gap-4 pb-3 @min-[760px]:grid-cols-2"
          >
            <Panel>
              <div className="grid gap-4 p-4">
                <Field label={t("pools.name")}>
                  <Input
                    aria-label={t("pools.name")}
                    maxLength={128}
                    value={draft.name}
                    onChange={(e) =>
                      setDraft({ ...draft, name: e.target.value })
                    }
                  />
                </Field>
                <Label className="flex items-center gap-2 text-sm">
                  <Checkbox
                    checked={draft.enabled}
                    onCheckedChange={(value) =>
                      value === true
                        ? setDraft({ ...draft, enabled: true })
                        : setPending({ kind: "disablePool" })
                    }
                  />
                  {t("pools.enabled")}
                </Label>
                <p className="text-xs text-muted-foreground">
                  {t("pools.poolEnabledHint")}
                </p>
              </div>
            </Panel>
            <Panel>
              <div className="grid gap-4 p-4">
                <Label className="flex items-center gap-2 text-sm">
                  <Checkbox
                    checked={draft.sticky.enabled}
                    onCheckedChange={(value) =>
                      setDraft({
                        ...draft,
                        sticky: { ...draft.sticky, enabled: value === true },
                      })
                    }
                  />
                  {t("pools.sticky")}
                </Label>
                <Field
                  label={t("pools.stickyTTL")}
                  hint={t("pools.stickyHint")}
                >
                  <Input
                    type="number"
                    min={60}
                    max={86400}
                    disabled={!draft.sticky.enabled}
                    aria-label={t("pools.stickyTTL")}
                    value={draft.sticky.ttl_seconds}
                    onChange={(e) =>
                      setDraft({
                        ...draft,
                        sticky: {
                          ...draft.sticky,
                          ttl_seconds: Number(e.target.value),
                        },
                      })
                    }
                  />
                </Field>
              </div>
            </Panel>
            <Panel>
              <div className="grid gap-4 p-4">
                <Field label={t("pools.queueLimit")}>
                  <Input
                    type="number"
                    min={0}
                    max={128}
                    aria-label={t("pools.queueLimit")}
                    value={draft.queue.max_waiting_requests}
                    onChange={(e) =>
                      setDraft({
                        ...draft,
                        queue: {
                          ...draft.queue,
                          max_waiting_requests: Number(e.target.value),
                        },
                      })
                    }
                  />
                </Field>
                <Field label={t("pools.queueWait")} hint={t("pools.queueHint")}>
                  <Input
                    type="number"
                    min={100}
                    max={60000}
                    aria-label={t("pools.queueWait")}
                    value={draft.queue.timeout_ms}
                    onChange={(e) =>
                      setDraft({
                        ...draft,
                        queue: {
                          ...draft.queue,
                          timeout_ms: Number(e.target.value),
                        },
                      })
                    }
                  />
                </Field>
              </div>
            </Panel>
            <ServiceProxyFields
              value={draft.proxy}
              onChange={(proxy) => setDraft({ ...draft, proxy })}
              hasCredential={!!base.proxy?.credential_ref}
              testDisabled={busy || !isReady}
              testTarget={proxyTarget}
              title={t("pools.proxyTitle")}
              inheritLabel={t("pools.poolProxyInherit")}
              inheritHint={t("pools.poolProxyHint")}
              onTest={() =>
                probeServiceProxy({
                  pool_id: pool.id,
                  proxy: proxyInput(draft.proxy)!,
                  target_url: proxyTarget,
                })
              }
            />
          </fieldset>
        </TabsContent>
      </Tabs>
      <Dialog open={addOpen} onOpenChange={setAddOpen}>
        <DialogContent>
          <DialogHeader>
            <DialogTitle>{t("pools.addMember")}</DialogTitle>
            <DialogDescription>{t("pools.noEligibleHint")}</DialogDescription>
          </DialogHeader>
          <Field label={t("pools.chooseAccount")}>
            <Select value={candidate} onValueChange={setCandidate}>
              <SelectTrigger
                aria-label={t("pools.chooseAccount")}
                className="w-full"
              >
                <SelectValue placeholder={t("pools.chooseAccount")} />
              </SelectTrigger>
              <SelectContent>
                {candidates.map((service) => (
                  <SelectItem key={service.id} value={service.id}>
                    {service.name}
                  </SelectItem>
                ))}
              </SelectContent>
            </Select>
          </Field>
          {!candidates.length ? (
            <p className="text-sm text-muted-foreground">
              {t("pools.noEligible")}
            </p>
          ) : null}
          <DialogFooter>
            <Button variant="outline" onClick={() => setAddOpen(false)}>
              {t("common.cancel")}
            </Button>
            <Button
              disabled={!candidates.some((service) => service.id === candidate)}
              onClick={() => {
                setMembers([
                  ...members,
                  {
                    service_id: candidate,
                    enabled: true,
                    max_concurrent_requests: 2,
                  },
                ]);
                setAddOpen(false);
              }}
            >
              {t("pools.addMember")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <Dialog
        open={newOpen}
        onOpenChange={(open) => {
          if (!busy) setNewOpen(open);
        }}
      >
        <DialogContent showCloseButton={!busy}>
          <DialogHeader>
            <DialogTitle>{t("pools.createAccount")}</DialogTitle>
            <DialogDescription>{t("pools.noEligibleHint")}</DialogDescription>
          </DialogHeader>
          <Field label={t("pools.accountName")}>
            <Input
              aria-label={t("pools.accountName")}
              value={shownNewName}
              onChange={(e) => setNewName(e.target.value)}
              maxLength={128}
              disabled={busy}
            />
          </Field>
          {error ? <FormMessage tone="error">{error}</FormMessage> : null}
          <DialogFooter>
            <Button
              variant="outline"
              disabled={busy}
              onClick={() => setNewOpen(false)}
            >
              {t("common.cancel")}
            </Button>
            <Button
              disabled={busy || !newName.trim()}
              onClick={() => {
                setBusy(true);
                setError(null);
                void (async () => {
                  const record = await createService({
                    name: newName.trim(),
                    kind: poolServiceKind(pool.provider),
                    ...(pool.provider === "claude_code"
                      ? { subscription_mode: "legacy" as const }
                      : {}),
                  });
                  if (!mounted.current) return;
                  onServiceSaved(record.service);
                  try {
                    const result = await replaceAccountPoolMembers(
                      pool.id,
                      [
                        ...memberInputs(base),
                        {
                          service_id: record.service.id,
                          enabled: true,
                          max_concurrent_requests: 2,
                        },
                      ],
                      base.etag,
                    );
                    if (!mounted.current) return;
                    accept(result);
                    setMembers(memberInputs(result));
                    setNewOpen(false);
                    notify.success(t("pools.accountCreated"));
                    onOpenAccount(record.service.id);
                  } catch {
                    if (mounted.current) setNewOpen(false);
                    throw new Error(t("pools.accountJoinFailed"));
                  }
                })()
                  .catch((e) => {
                    if (mounted.current) displayError(e);
                  })
                  .finally(() => {
                    if (mounted.current) setBusy(false);
                  });
              }}
            >
              {t("pools.createAccount")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
      <ConfirmDialog
        open={pending !== null}
        title={t(confirmation.title)}
        description={t(confirmation.description)}
        destructive={
          shownPending?.kind === "delete" || shownPending?.kind === "remove"
        }
        disabled={busy}
        confirmLabel={t(confirmation.confirm)}
        onCancel={() => {
          if (!busy) setPending(null);
        }}
        onConfirm={() => {
          if (!pending || busy) return;
          if (pending.kind === "disablePool") {
            setDraft({ ...draft, enabled: false });
            setPending(null);
            return;
          }
          if (pending.kind === "disableMember") {
            updateMember(pending.id, { enabled: false });
            setPending(null);
            return;
          }
          if (pending.kind === "remove") {
            setMembers((current) =>
              current.filter((member) => member.service_id !== pending.id),
            );
            setPending(null);
            return;
          }
          setBusy(true);
          setError(null);
          const action =
            pending.kind === "delete"
              ? deleteAccountPool(pool.id, base.etag)
              : probeAccountPoolQuota(pool.id, pending.id);
          void action
            .then(() => {
              if (!mounted.current) return;
              if (pending.kind === "delete") onDeleted();
              else notify.success(t("pools.quotaProbeArmed"));
              setPending(null);
            })
            .catch((e) => {
              setPending(null);
              displayError(e);
            })
            .finally(() => {
              if (mounted.current) setBusy(false);
            });
        }}
      />
    </>
  );
}
