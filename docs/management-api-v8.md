# v8 Management API

The base path is `/v8/management`. Configuration endpoints follow the structure in
[config.example.yaml](../config.example.yaml); operational endpoints are grouped
by function. The existing `/v0/management` API remains supported. Its reference is
available in the [management API guide](https://help.router-for.me/management/api).

## Access

The v8 endpoints use the same management key and remote-access policy as v0.
Send `Authorization: Bearer <management-key>` or `X-Management-Key: <management-key>`.
OAuth callbacks validate a pending login state and do not require a management-key
header. Home mode disables local management endpoints for both versions.

## Configuration endpoints

All paths below are relative to `/v8/management`.

| Path | Methods | Description |
| --- | --- | --- |
| `/config` | GET, PUT, PATCH | Read, replace, or merge the complete configuration as JSON. |
| `/config.yaml` | GET, PUT | Read or replace the complete configuration as YAML. |
| `/config/<section>/<field>` | GET, PUT, PATCH, DELETE | Read or modify a section or field using its path in the v8 tree. |

GET renders the persisted configuration in the v8 layout without migrating the
file or adding omitted runtime defaults. Successful v8 configuration writes save
the latest layout; reads and rejected writes leave the file unchanged.
Historical v8 paths and request bodies remain accepted as aliases.

JSON writes accept the value directly, without a `{ "value": ... }` envelope.
PUT replaces its target. PATCH merges objects and replaces lists and scalars.
Scalar field updates retain existing comments; complete replacements use the submitted document.
DELETE removes a field. Paths identify mapping keys, not array indexes; replace
an entire list to change its entries. Optional per-key `null` overrides inherit
the corresponding group value. Legacy field names are rejected by v8 writes.

| Example path | Value or purpose |
| --- | --- |
| `/config/access/api-keys` | Client authentication keys, for example `["client-key"]`. |
| `/config/api-keys` | All upstream provider groups. |
| `/config/api-keys/codex` | Codex upstream groups. |
| `/config/client/codex/optimize-multi-agent-v2` | Boolean, default `false`; applies to Codex clients across OAuth and API-key routes. |
| `/config/observability/logs/debug` | A boolean, for example `true`. |
| `/config/routing/retry/request-retry` | A number, for example `0`. |
| `/config/plugins/configs/<id>` | A plugin configuration object. |

For example, `PATCH /v8/management/config` with this body disables additional
request retries while preserving other settings:

```json
{
  "routing": {
    "retry": {
      "request-retry": 0
    }
  }
}
```

JSON reads omit TURN usernames and credentials under
`oauth.providers.codex.live-media-relay.ice-servers`; YAML reads include them.
The Home-owned revision fields
`credentials.concurrency.lifecycle-config-revision`,
`credentials.concurrency.observation-barrier-revision`, and `plugins.auth-revision`
cannot be changed through these endpoints.

### Codex multi-agent configuration migration

`client.codex.optimize-multi-agent-v2` is the sole runtime setting. Loading older
YAML accepts `providers.codex.optimize-multi-agent-v2`,
`oauth.providers.codex.optimize-multi-agent-v2`, and the flat
`codex.optimize-multi-agent-v2` path. The client path always wins conflicts,
including explicit `false` or `null`. If it is absent, the historical OAuth path
wins over `providers`, which wins over the flat path.

These aliases now have client-wide semantics, not OAuth-only scope. Loading does
not rewrite legacy-only settings; normalization removes aliases conflicting with
the client path. A successful v8 write migrates the aliases while preserving their
comments. Historical API paths remain usable. Home configuration publishers
need matching client schema support before emitting the new path; older YAML
payloads remain readable.

## Operational endpoints

All paths below are relative to `/v8/management`. Request and response bodies
retain the corresponding business operation's fields.

| Path | Methods | Description |
| --- | --- | --- |
| `/config/upstream/<provider>` | GET, PUT, PATCH, DELETE | Manage shared provider settings. |
| `/providers` | GET | List configured upstream providers. |
| `/providers/<id>/usage` | GET | Get normalized subscription usage for one provider. |
| `/models` | GET | List the exposed model catalog, including model routes. |
| `/server/latest-version` | GET | Get latest release information. |
| `/requests/api-call` | POST | Make an authenticated upstream call. |
| `/routing/cooldown/reset` | POST | Clear credential cooldown. |
| `/routing/model-definitions/<channel>` | GET | Get model definitions. |
| `/observability/logs` | GET, DELETE | Read or clear application logs. |
| `/observability/logs/errors` | GET | List error-log files. |
| `/observability/logs/errors/<name>` | GET | Download an error-log file. |
| `/observability/logs/requests/<id>` | GET | Get a request log. |
| `/observability/usage/api-keys` | GET | Get API-key usage. |
| `/observability/usage/queue` | GET | Get queued usage events. |
| `/credentials` | GET, POST, DELETE | List, upload, or delete credential files. |
| `/credentials/models` | GET | Get credential models. |
| `/credentials/download` | GET | Download a credential file. |
| `/credentials/status` | PATCH | Change credential status. |
| `/credentials/fields` | PATCH | Change credential fields. |
| `/credentials/refresh` | POST | Refresh credentials. |
| `/oauth/import?provider=vertex` | POST | Import a Vertex service account using a multipart `file` upload. |
| `/oauth/auth-url?provider=<provider>` | GET | Start built-in or plugin OAuth. |
| `/oauth/status?state=<state>` | GET | Get login status. |
| `/oauth/session?state=<state>` | DELETE | Cancel a login session. |
| `/oauth/callback` | GET, POST | Submit an OAuth callback. |
| `/plugins` | GET | List installed plugins. |
| `/plugins/<id>` | DELETE | Delete a plugin. |
| `/plugins/store` | GET | List the plugin store. |
| `/plugins/store/<id>/install` | POST | Install or update a plugin. |
| `/plugins/<id>/quota` | GET, POST, DELETE | Read, fetch, or reset plugin quota. |

## Providers, usage, and models

`GET /v8/management/providers` lists configured OpenAI-compatible providers and
every built-in or plugin provider with registered credentials. Credentials are
never returned.

```json
{"providers":[{"id":"mistral","name":"mistral","disabled":false,"model_ids":["deepseek-v4.1-flash-1017"],"usage_supported":true}]}
```

`id` is the stable provider id: the lower-cased `openai-compatibility` name or the
built-in provider key (for example `codex`). It is also the `provider` value of a
model-route target. `model_ids` are exposed proxy model ids served by the provider,
including model-route aliases with a target on it. `usage_supported` means a usage
adapter exists, not that the credential is valid.

`GET /v8/management/providers/<id>/usage` reports each credential separately.
Unknown providers return 404.

```json
{"provider_id":"mistral","accounts":[{"id":"1a2b3c4d5e6f7a8b","status":"ok","plan":"Pro","checked_at":"2026-11-14T03:00:00Z","windows":[{"id":"api","label":"API","used_percent":25,"used":3.75,"limit":15,"remaining":11.25,"unit":"usd","resets_at":"2026-12-01T00:00:00Z"}]}]}
```

Account `status` is `ok`, `unsupported`, `unavailable`, or `error`; failed or
unsupported accounts have empty `windows` and a short `error`. One account's
failure does not affect the others. Window values the vendor does not report are
omitted rather than shown as zero or unlimited. Currency amounts are money in
`unit`, not tokens.

A window with `used_percent` or a positive `limit` is an allowance. A window with
only `used` and/or `remaining` in a currency `unit` is a plain money amount with no
allowance, such as OpenRouter key spend when the key has no limit. OpenRouter
`credits` is a prepaid balance: `used` is total spend and `remaining` is the
balance still available, with no `limit` or `used_percent`. For an OpenRouter key
with a limit, `used` is the spend counted against that limit
(`limit - limit_remaining`), because a key limit may reset periodically.

| Provider | Source | Windows |
| --- | --- | --- |
| Mistral (`api.mistral.ai`) | `<base-url>/billing/budget`, `/billing/subscription` | `vibe`, `api` |
| OpenCode Go (`opencode.ai/zen/go`) | `<base-url>/usage` | `rolling`, `weekly`, `monthly` |
| OpenRouter (`openrouter.ai`) | `<base-url>/key`, `/credits` when the key may read it | `key`, `key-daily`, `key-weekly`, `key-monthly`, `credits` |
| Codex, Claude | Passive quota headers from earlier responses; no polling | `five-hour`, `weekly` |

OpenAI-compatible adapters are selected by base-url host, or by the provider names
`mistral`, `opencode-go`, and `openrouter` for proxied endpoints. They only call
the provider's own configured base URL with its own key. Other providers report
`unsupported`. Subscription usage is separate from proxy token accounting under
`/observability/usage`.

`GET /v8/management/models` returns the same catalog as `/v1/models` in OpenAI
list form, without requiring a client API key.

### Model routes

`model-routes` (see [config.example.yaml](../config.example.yaml)) exposes an alias
backed by an ordered list of provider/model targets, which may be unrelated models.
Targets are tried in order with each provider's own credentials, retries, and
cooldowns. Credential, quota, availability, and upstream errors move to the next
target; caller request errors and cancellation stop. A streamed response that has
produced output is never switched. Responses, usage records, and payload rules keep
the alias as the requested model, and the alias appears in model catalogs while one
of its targets is available. Target `params` are applied before `requests.payload`
rules on OpenAI-compatible providers. Aliases have no special suffixes:
`model:fast` is an independent route.

## OAuth

The login URL is shared by all providers. Set the required `provider` query
parameter to `claude`, `codex`, `antigravity`, `kimi`, `kimi-ai`, `xai`, `devin`,
`meta`, or a registered plugin provider ID. For example:

```http
GET /v8/management/oauth/auth-url?provider=codex
Authorization: Bearer <management-key>
```

Keep any provider-specific login parameters in the query string. The login
response includes the authorization URL and a session `state`. Use that state
for status queries and cancellation.

Callbacks accept `provider`, `state`, `code`, and `error` as GET query parameters
or POST JSON fields. POST also accepts `redirect_url` containing the complete
callback URL. If `provider` is omitted, it is inferred from the pending state;
an explicit provider must match that state. Poll until the status is `ok` or
`error`; `wait` means the login is still pending. Callback acceptance alone does
not mean credential exchange and persistence have completed.

Import uses `POST /v8/management/oauth/import?provider=vertex`. The `provider`
query parameter is required; Vertex is currently the supported import provider.

## v0 compatibility

The `/v0/management` endpoints keep their original paths and payloads, including
provider-specific OAuth login URLs, callbacks, status queries, and cancellation.
For example, `GET /v0/management/codex-auth-url` and
`GET /v8/management/oauth/auth-url?provider=codex` start the same login flow.

Legacy flat configuration endpoints such as `/debug`, `/request-retry`, and
`/codex-api-key` exist under `/v0/management` only.
`/v0/management/api-keys` continues to manage client authentication keys; the v8
equivalent is `/v8/management/config/access/api-keys`.

Legacy-only configuration files keep their layout until a successful v8
configuration write. When both layouts specify a field, the new field takes
precedence and its legacy equivalent is removed. V0 setters keep legacy-only
files in their original layout; existing v8 files are saved in the latest v8
layout. `PUT /v0/management/config.yaml` still replaces the complete file and
accepts legacy, new, or mixed layouts, normalizing v8 documents on save.

Plugin OAuth uses the shared v8 login endpoint. Other plugin-defined HTTP
extensions retain their declared `/v0/management` routes.
