import { useId } from "react";
import { ChoiceCard } from "./ChoiceCard";
import { Field } from "./Field";
import { Input } from "./ui/input";
import { RadioGroup } from "./ui/radio-group";
import { Switch } from "./ui/switch";
import { useT } from "../i18n";
import type { KiroAuthorization } from "../subscription-model";
import type { KiroLoginDraft } from "../kiro-authorization-model";

export function KiroAuthorizationEditor({
  value,
  onChange,
}: {
  value: KiroLoginDraft;
  onChange: (value: KiroLoginDraft) => void;
}) {
  const t = useT();
  const id = useId();
  return (
    <div className="grid min-w-0 gap-3">
      <RadioGroup
        aria-label={t("services.kiroLoginMethod")}
        className="grid gap-2"
        value={value.method}
        onValueChange={(method) =>
          onChange({
            ...value,
            method: method as KiroAuthorization["method"],
            manual: false,
          })
        }
      >
        <ChoiceCard
          value="builder_id"
          label="AWS Builder ID"
          description={t("services.kiroBuilderHint")}
          selected={value.method === "builder_id"}
        />
        <ChoiceCard
          value="social"
          label="Google / GitHub"
          description={t("services.kiroSocialHint")}
          selected={value.method === "social"}
        />
        <ChoiceCard
          value="iam_sso"
          label="AWS IAM Identity Center"
          description={t("services.kiroSSOHint")}
          selected={value.method === "iam_sso"}
        />
      </RadioGroup>
      {value.method === "iam_sso" ? (
        <>
          <Field htmlFor={`${id}-start`} label={t("services.kiroStartURL")}>
            <Input
              id={`${id}-start`}
              value={value.start_url || ""}
              placeholder="https://your-organization.awsapps.com/start"
              maxLength={2048}
              onChange={(event) =>
                onChange({ ...value, start_url: event.target.value })
              }
            />
          </Field>
          <Field htmlFor={`${id}-region`} label={t("services.kiroRegion")}>
            <Input
              id={`${id}-region`}
              value={value.region || ""}
              maxLength={40}
              placeholder="us-east-1"
              onChange={(event) =>
                onChange({ ...value, region: event.target.value })
              }
            />
          </Field>
          <Field
            htmlFor={`${id}-device`}
            label={t("services.kiroSSODevice")}
            hint={t("services.kiroSSODeviceHint")}
          >
            <Switch
              id={`${id}-device`}
              checked={value.device}
              onCheckedChange={(device) =>
                onChange({ ...value, device, manual: false })
              }
            />
          </Field>
        </>
      ) : null}
      {value.method === "social" ||
      (value.method === "iam_sso" && !value.device) ? (
        <Field
          htmlFor={`${id}-manual`}
          label={t("services.kiroManualCallback")}
          hint={t("services.kiroManualHint")}
        >
          <Switch
            id={`${id}-manual`}
            checked={value.manual}
            onCheckedChange={(manual) => onChange({ ...value, manual })}
          />
        </Field>
      ) : null}
    </div>
  );
}
