# CRM — Agent Workflow

Conventions for AI agents (Claude, Codex, Paperclip workers) contributing to this repo.

## Pre-PR checklist

Antes de `gh pr create`, rodar `./scripts/pre-pr-check.sh`. Se falhar, fixar antes de abrir o PR.

- O script roda `gofmt -l .`, `go vet ./...`, e `go build ./...` — os checks rápidos do CI (`lint` + `build`).
- Não roda `go test ./...` (testcontainers postgres demora minutos) nem `coverage-gate`.
- Atalho: `make precheck` (wrapper do mesmo script).

Se um check falhar, **fixar antes de abrir o PR**, não depois — CI vermelho consome ciclos de revisão e budget.
