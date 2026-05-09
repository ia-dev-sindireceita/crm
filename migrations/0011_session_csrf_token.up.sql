-- 0011_session_csrf_token.up.sql
-- Per-session CSRF token storage (SIN-62375 / FAIL-2 from SIN-62343).
-- ADR 0073 §D1 amendment: store the 32-byte base64 CSPRNG token next to
-- the session row so the CSRF middleware can verify presented values
-- against the durable session record on every state-changing request.
--
-- Storage choice: Postgres column on the existing sessions table. The
-- alternative (Redis adjacent, keyed by session id) was rejected because
-- (a) tenant RLS already gates the column, (b) the lifecycle is exactly
-- the session row's lifecycle (no separate TTL to keep aligned), and
-- (c) it survives a Redis flush (D4 lockout precedent).
--
-- Backward compatibility: TEXT NOT NULL DEFAULT '' so any existing row
-- gets the empty sentinel. internal/iam/csrf.Verify treats an empty
-- session token as ErrSessionTokenMissing (a programmer / migration
-- bug) which the middleware surfaces as 403 csrf.session_token_missing
-- — never silently accepted. Mint-on-login (iam.Service.Login) writes
-- a fresh value on every new session so the empty sentinel only ever
-- appears on rows that pre-date this migration.
--
-- Run as app_admin. Idempotent.

BEGIN;

ALTER TABLE sessions
  ADD COLUMN IF NOT EXISTS csrf_token TEXT NOT NULL DEFAULT '';

COMMIT;
