import { Field } from "./Field";
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue,
} from "./ui/select";
import { useT } from "../i18n";
import {
  poolProviderNames,
  type AccessTokenRouting,
  type AccountPool,
} from "../account-pool-model";

export function TokenRoutingField({
  value,
  onChange,
  pools,
  disabled = false,
}: {
  value: AccessTokenRouting;
  onChange: (value: AccessTokenRouting) => void;
  pools: readonly AccountPool[];
  disabled?: boolean;
}) {
  const t = useT();
  const selected = value.routing_scope === "global" ? "global" : value.pool_id;
  return (
    <Field
      label={t("pools.keyScope")}
      hint={t(
        value.routing_scope === "global"
          ? "pools.globalScopeHint"
          : "pools.poolScopeHint",
      )}
    >
      <Select
        value={selected}
        disabled={disabled}
        onValueChange={(next) =>
          onChange(
            next === "global"
              ? { routing_scope: "global" }
              : { routing_scope: "pool", pool_id: next },
          )
        }
      >
        <SelectTrigger aria-label={t("pools.keyScope")} className="w-full">
          <SelectValue />
        </SelectTrigger>
        <SelectContent>
          <SelectItem value="global">{t("pools.globalScope")}</SelectItem>
          {value.routing_scope === "pool" &&
          !pools.some((pool) => pool.id === value.pool_id) ? (
            <SelectItem value={value.pool_id} disabled>
              {value.pool_id}
            </SelectItem>
          ) : null}
          {pools.map((pool) => (
            <SelectItem key={pool.id} value={pool.id}>
              {pool.name} · {poolProviderNames[pool.provider]}
              {!pool.enabled ? ` · ${t("pools.disabled")}` : ""}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
    </Field>
  );
}
