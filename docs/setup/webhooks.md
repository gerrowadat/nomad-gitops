# Webhooks

Configuring a webhook removes the latency between a push to the repo and the
next drift check — instead of waiting for `--poll-interval`, nomad-botherer
fetches immediately on push. Webhooks are optional; polling always runs too.

## GitHub setup

1. Go to your repo → **Settings** → **Webhooks** → **Add webhook**
2. Set **Payload URL** to `https://your-host:8080/webhook` (or your
   `--webhook-path`)
3. Set **Content type** to `application/json`
4. Set **Secret** to the same value as `--webhook-secret` / `WEBHOOK_SECRET`
5. Under **Which events would you like to trigger this webhook?** choose
   **Just the push event**
6. Click **Add webhook**

The service handles `push` events (triggers a fetch + diff) and `ping` events
(acknowledged, no action). All other event types are silently ignored with a
`200 OK`.

## Security

- If `--webhook-secret` is empty, signature verification is skipped and any
  caller can trigger a git fetch. **In production, always set a secret** — the
  service then rejects deliveries whose `X-Hub-Signature-256` HMAC does not
  match.
- Webhook request bodies are capped at 25 MB (GitHub's own payload limit);
  larger requests are rejected with `400 Bad Request` before being read into
  memory.

The webhook path and secret are set via `--webhook-path` / `WEBHOOK_PATH` and
`--webhook-secret` / `WEBHOOK_SECRET` (see [Configuration](../configuration.md)).
To test a webhook locally, see
[Simulating a webhook](../development.md#simulating-a-webhook).
