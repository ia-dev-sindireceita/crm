# CRM

Multi-tenant CRM (fork of `pericles-luz/crm`). Fase 0 bootstrap — see SIN-62192.

## Local bring-up

```bash
git clone https://github.com/pericles-luz/crm.git
cd crm
cp deploy/compose/.env.example deploy/compose/.env  # fill secrets
make up                                              # postgres, redis, nats, minio, caddy, app
curl http://localhost:8080/health                    # → 200 {"status":"ok"}
make down                                            # tear down (volumes preserved)
```

## Layout

- `cmd/server/` — HTTP entrypoint (`/health` only in PR1; expanded in PR4–PR7).
- `internal/` — pure bounded contexts (iam, tenancy, …) added in later PRs.
- `adapters/` — postgres, redis, nats, channels (later PRs).
- `migrations/`, `docs/adr/`, `deploy/{compose,caddy}/` — infra.

## Tests

```bash
make test   # go test ./... -race -cover
make lint   # go vet ./...
```

## Transactional email

Provider wiring selects an adapter at boot via `EMAIL_PROVIDER`
(`mailgun` | `recorder` | `noop`). See
[docs/ops/email-provider.md](docs/ops/email-provider.md) for env vars,
provider switch procedure, and the opt-in live Mailgun smoke test.
Architecture decision: [ADR 0096](docs/adr/0096-email-mailgun-adapter.md).
