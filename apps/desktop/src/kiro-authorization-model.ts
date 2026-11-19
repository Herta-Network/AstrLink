import type {
  AuthorizationFlow,
  KiroAuthorization,
} from "./subscription-model";

export interface KiroLoginDraft extends KiroAuthorization {
  manual: boolean;
  device: boolean;
}

export function defaultKiroLoginDraft(): KiroLoginDraft {
  return {
    method: "builder_id",
    region: "us-east-1",
    start_url: "",
    manual: false,
    device: true,
  };
}

export function kiroLoginFlow(value: KiroLoginDraft): AuthorizationFlow {
  if (value.method === "builder_id") return "device_code";
  if (value.manual) return "authorization_code";
  return value.method === "iam_sso" && value.device ? "device_code" : "browser";
}

export function kiroLoginOptions(
  value: KiroLoginDraft,
): KiroAuthorization | undefined {
  if (value.method === "builder_id") return undefined;
  return value.method === "social"
    ? { method: "social" }
    : {
        method: "iam_sso",
        region: value.region?.trim() || "us-east-1",
        start_url: value.start_url?.trim() || "",
      };
}

export function kiroLoginValid(value: KiroLoginDraft): boolean {
  if (value.method !== "iam_sso") return true;
  if (!/^[a-z]{2}(?:-[a-z0-9]+)+-[0-9]+$/.test(value.region || ""))
    return false;
  try {
    const url = new URL(value.start_url || "");
    return (
      url.protocol === "https:" &&
      !!url.hostname &&
      !url.username &&
      !url.password &&
      !url.search &&
      !url.hash
    );
  } catch {
    return false;
  }
}
