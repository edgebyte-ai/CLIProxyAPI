# Decisions proxy for standalone CPA

`POST /v1/decisions` forwards a Decisions request to a single configured upstream. Clients authenticate with their usual CPA key. Only the plugin's bearer key is sent to the upstream. Point the plugin at the full endpoint, not at a base URL. The default is `https://api.openai.com/v1/decisions`. To use OpenAI, supply an OpenAI Platform API key with access to Decisions. This route does not use ChatGPT subscription credentials or fall back to Responses.

## Build and configure

Build the **CPA host and plugin from this revision**. An older CPA binary does not register `/v1/decisions`. From the repository root on Linux:

```sh
go build -o cli-proxy-api ./cmd/server
mkdir -p plugins
(cd examples/plugin/decisions-proxy/go && CGO_ENABLED=1 go build -buildmode=c-shared -o ../../../../plugins/decisions-proxy.so .)
```

Merge this into your CPA configuration. Set `DECISIONS_UPSTREAM_KEY` in the server process environment before starting CPA. If storing the secret in the CPA configuration is appropriate, use `api-key` **instead of** `api-key-env`.

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    decisions-proxy:
      enabled: true
      upstream-url: https://api.openai.com/v1/decisions
      api-key-env: DECISIONS_UPSTREAM_KEY
```

Keep the plugin filename `decisions-proxy.so` (a versioned filename such as `decisions-proxy-v0.1.0.so` also works). The URL must use HTTPS; HTTP is also accepted on loopback for local services. Redirects are not followed. Without an active plugin, authenticated requests to this endpoint return 404. This host route is disabled in Home mode.

## Call it

Set `CPA_API_KEY` to the client key for your CPA instance. Adjust the CPA address as needed:

```sh
curl -sS http://127.0.0.1:8317/v1/decisions \
  -H "Authorization: Bearer $CPA_API_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"model":"gpt-6-luna","input":"My package arrived damaged.","questions":[{"type":"predicate","name":"damaged","instructions":"Does the customer report damage?"}]}'
```

This route runs CPA frontend authentication. It dispatches directly to the plugin, so normal model routing, model-request interceptors and per-model usage reporting do not run. Track billable usage on the configured upstream account.

CPA applies matching [payload rules](../../../config.example.yaml) before forwarding (protocol `decisions`, executor `decisions-proxy`). The upstream determines which models and question types it accepts. The response retains the upstream HTTP status, JSON body, and diagnostic headers such as `X-Request-Id` and `Retry-After`. If an upstream error body echoes the configured key, the plugin replaces that body with a generic error and keeps the status. An upstream transport error or a response body exceeding 16 MiB produces a 502. See the [OpenAI Decisions reference](https://developers.openai.com/api/reference/resources/decisions/methods/create) for the request and response format.
