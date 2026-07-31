import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { fetchApps, fetchOAuthApps, fetchOAuthGrants, revokeOAuthGrant } from "../api.js";
import { InlineError, Spinner } from "@bleephub/ui-core/components";
import { PageTitle, Button, Box, CodeBlock, ErrorBanner } from "../components/ui.js";
import { confirmAction } from "../components/confirmAction.js";

interface DeviceCodeResponse {
  device_code: string;
  user_code?: string;
  verification_uri?: string;
  verification_uri_complete?: string;
  expires_in?: number;
  interval?: number;
}

function isDeviceCodeResponse(value: unknown): value is DeviceCodeResponse {
  return (
    typeof value === "object" &&
    value !== null &&
    typeof (value as Record<string, unknown>).device_code === "string"
  );
}

function safeOAuthResult(value: unknown): string {
  if (typeof value !== "object" || value === null) return JSON.stringify(value, null, 2);
  const redacted = { ...(value as Record<string, unknown>) };
  for (const key of ["access_token", "refresh_token", "device_code"]) {
    if (key in redacted) redacted[key] = "[redacted]";
  }
  return JSON.stringify(redacted, null, 2);
}

export function OAuthPage() {
  return (
    <div>
      <PageTitle
        title="OAuth flows"
        meta="Device flow and web flow controls through GitHub OAuth endpoints."
      />

      <FlowSimulator />
      <AuthorizedApplications />
    </div>
  );
}

function AuthorizedApplications() {
  const queryClient = useQueryClient();
  const grants = useQuery({ queryKey: ["oauth-grants"], queryFn: fetchOAuthGrants });
  const revoke = useMutation({
    mutationFn: revokeOAuthGrant,
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["oauth-grants"] }),
  });
  return (
    <Box header={<span style={{ fontWeight: 600 }}>Authorized applications</span>}>
      <div style={{ padding: "1rem" }}>
        {grants.isLoading ? <Spinner label="loading authorized applications" /> : grants.isError ? (
          <InlineError title="Failed to load authorized applications" detail={String(grants.error)} />
        ) : grants.data?.length === 0 ? (
          <p style={{ color: "var(--color-fg-muted)" }}>You have not authorized any applications.</p>
        ) : (
          <div className="grid gap-3">
            {grants.data?.map((grant) => (
              <div key={grant.client_id} className="flex flex-wrap items-center justify-between gap-3 rounded border p-3">
                <div>
                  <b>{grant.name || grant.client_id}</b>
                  <div style={{ color: "var(--color-fg-muted)", fontSize: "0.78rem" }}>
                    {grant.type === "GitHubApp" ? "GitHub App" : "OAuth App"}
                    {grant.scopes.length > 0 ? ` · ${grant.scopes.join(", ")}` : ""}
                  </div>
                </div>
                <Button
                  size="sm"
                  variant="danger"
                  disabled={revoke.isPending}
                  onClick={async () => {
                    if (await confirmAction(`Revoke authorization for ${grant.name || grant.client_id}?`)) {
                      revoke.mutate(grant.client_id);
                    }
                  }}
                >
                  Revoke
                </Button>
              </div>
            ))}
          </div>
        )}
        {revoke.error && <div className="mt-3"><ErrorBanner>{String(revoke.error)}</ErrorBanner></div>}
      </div>
    </Box>
  );
}

function FlowSimulator() {
  const apps = useQuery({
    queryKey: ["oauth-flow-clients"],
    queryFn: async () => {
      const [githubApps, oauthApps] = await Promise.all([fetchApps(), fetchOAuthApps()]);
      return [
        ...oauthApps.map((app) => ({
          id: app.clientId,
          label: `${app.name} (OAuth App)`,
          callback: app.callbackUrl,
        })),
        ...githubApps.map((app) => ({
          id: app.clientId,
          label: `${app.name} (GitHub App)`,
          callback: app.callbackUrl,
        })),
      ];
    },
  });
  const [clientID, setClientID] = useState("");
  const [redirectURI, setRedirectURI] = useState("http://localhost:8080/callback");
  const [scope, setScope] = useState("repo read:org");
  const [state, setState] = useState("STATE-1");
  const [deviceCode, setDeviceCode] = useState("");
  const [result, setResult] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);

  function startWebFlow() {
    setError(null);
    setResult(null);
    const url =
      `/login/oauth/authorize?client_id=${encodeURIComponent(clientID)}` +
      `&redirect_uri=${encodeURIComponent(redirectURI)}` +
      `&scope=${encodeURIComponent(scope)}` +
      `&state=${encodeURIComponent(state)}`;
    const popup = window.open(url, "_blank", "noopener");
    if (!popup) {
      setError("Your browser blocked the OAuth window. Allow pop-ups for this site and try again.");
      return;
    }
    setResult(`Opened ${url} in a new tab.`);
  }

  async function startDeviceFlow() {
    setError(null);
    setResult(null);
    try {
      const body = new URLSearchParams();
      body.set("client_id", clientID);
      body.set("scope", scope);
      const res = await fetch("/login/device/code", {
        method: "POST",
        headers: { "Content-Type": "application/x-www-form-urlencoded" },
        body,
      });
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      const json: unknown = await res.json();
      if (!isDeviceCodeResponse(json)) {
        throw new Error("Device flow response did not include a device_code");
      }
      setDeviceCode(json.device_code);
      setResult(safeOAuthResult(json));
    } catch (e) {
      setError(String(e));
    }
  }

  async function pollDeviceToken() {
    setError(null);
    setResult(null);
    try {
      const body = new URLSearchParams();
      body.set("client_id", clientID);
      body.set("grant_type", "urn:ietf:params:oauth:grant-type:device_code");
      body.set("device_code", deviceCode);
      const res = await fetch("/login/oauth/access_token", {
        method: "POST",
        headers: {
          Accept: "application/json",
          "Content-Type": "application/x-www-form-urlencoded",
        },
        body,
      });
      if (!res.ok) throw new Error(`${res.status} ${res.statusText}`);
      setResult(safeOAuthResult(await res.json()));
    } catch (e) {
      setError(String(e));
    }
  }

  return (
    <Box className="mb-6" header={<span style={{ fontWeight: 600, color: "var(--color-fg)" }}>OAuth flow controls</span>}>
      <div style={{ padding: "1rem" }}>
        {apps.isLoading ? <Spinner label="loading OAuth clients" /> : apps.isError ? (
          <InlineError title="Failed to load OAuth clients" detail={String(apps.error)} />
        ) : (
          <div className="mb-4">
            <label htmlFor="oauth-registered-client" className="mb-1 block" style={{ fontSize: "0.82rem", fontWeight: 600 }}>
              Registered application
            </label>
            <select
              id="oauth-registered-client"
              className="w-full"
              value={clientID}
              onChange={(event) => {
                const selected = apps.data?.find((app) => app.id === event.target.value);
                setClientID(event.target.value);
                if (selected?.callback) setRedirectURI(selected.callback);
              }}
            >
              <option value="">Choose an application…</option>
              {apps.data?.map((app) => <option key={app.id} value={app.id}>{app.label}</option>)}
            </select>
          </div>
        )}
        <div className="mb-4 grid gap-3 md:grid-cols-2">
          <Field label="Client identifier" value={clientID} onChange={setClientID} />
          <Field label="State" value={state} onChange={setState} />
          <Field label="Redirect Uniform Resource Identifier" value={redirectURI} onChange={setRedirectURI} />
          <Field label="Scope" value={scope} onChange={setScope} />
          <Field label="Device code" value={deviceCode} onChange={setDeviceCode} />
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="primary" size="sm" onClick={startWebFlow} disabled={!clientID.trim()}>
            Web flow
          </Button>
          <Button variant="secondary" size="sm" onClick={startDeviceFlow} disabled={!clientID.trim()}>
            Device flow
          </Button>
          <Button variant="secondary" size="sm" onClick={pollDeviceToken} disabled={!deviceCode.trim()}>
            Poll device token
          </Button>
        </div>
        {result && (
          <div className="mt-4">
            <CodeBlock>{result}</CodeBlock>
          </div>
        )}
        {error && <div className="mt-4"><ErrorBanner>{error}</ErrorBanner></div>}
      </div>
    </Box>
  );
}

function Field({
  label,
  value,
  onChange,
}: {
  label: string;
  value: string;
  onChange: (v: string) => void;
}) {
  const id = `oauth-${label.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-|-$/g, "")}`;
  return (
    <div>
      <label htmlFor={id} className="mb-1 block" style={{ fontSize: "0.82rem", fontWeight: 600, color: "var(--color-fg)" }}>
        {label}
      </label>
      <input id={id} type="text" value={value} onChange={(e) => onChange(e.target.value)} className="w-full" />
    </div>
  );
}
