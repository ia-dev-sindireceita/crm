// Package lgpd implements the data-protection-officer surface for
// Brazil's Lei Geral de Proteção de Dados (LGPD): contact-scoped data
// export (art. 18 II "acesso aos dados") and contact-scoped deletion
// with the fiscal-retention exception (art. 18 VI "eliminação dos
// dados pessoais tratados com o consentimento do titular").
//
// The package is hexagonal: this file declares the domain types and
// ports. Concrete adapters live in
// internal/adapter/db/postgres/lgpd_repository.go (storage) and
// internal/web/lgpd/handlers.go (HTTP). The worker that finalises a
// deletion request after its retention window expires lives in
// internal/worker/lgpd_retention; its binary is
// cmd/lgpd-retention-purge-worker.
//
// SIN-63186 / Fase 6 PR3.
package lgpd
