// Package walletui implements the three read-side ports backing the
// F5 gerente wallet UI (SIN-63942):
//
//   - DashboardReader   → GET /wallet
//   - LedgerReader      → GET /wallet/ledger, GET /wallet/ledger.csv
//   - TopupCatalogReader → GET /wallet/topup
//
// Tenant-scoped reads (DashboardReader, LedgerReader) route through
// postgresadapter.WithTenant so RLS scopes the visible rows to the
// requested tenant; the handler layer adds a defensive cross-tenant
// gate above this adapter. token_packages has no RLS (catalogue), so
// TopupCatalogReader.ListPackages uses the runtime pool directly.
//
// LGPD: ledger views never expose raw conversation_id or message
// bodies. ConversationIDHash is a 16-char hex prefix of
// sha256(metadata->>'conversation_id'); the model id and ai_policy_id
// pass through directly because both are operational metadata, not
// PII.
package walletui

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	postgresadapter "github.com/pericles-luz/crm/internal/adapter/db/postgres"
	"github.com/pericles-luz/crm/internal/wallet"
	"github.com/pericles-luz/crm/internal/web/walletui"
)

var (
	_ walletui.DashboardReader    = (*Store)(nil)
	_ walletui.LedgerReader       = (*Store)(nil)
	_ walletui.TopupCatalogReader = (*Store)(nil)
)

// dashboardLastFive is the number of recent ledger rows surfaced on
// the dashboard preview card.
const dashboardLastFive = 5

// dashboardConsumeWindowDays is the trailing window the dashboard uses
// to project AvgDailyConsume / DaysRemaining (per F5 spec).
const dashboardConsumeWindowDays = 14

// Store implements the three walletui ports against Postgres.
//
// runtimePool connects as app_runtime (RLS enforced). Tenant-scoped
// reads wrap their work in WithTenant; the runtime pool is also used
// directly for the catalog read (token_packages has no RLS).
type Store struct {
	runtimePool *pgxpool.Pool
}

// New constructs a Store. nil pool is rejected so the wire fails fast.
func New(runtimePool *pgxpool.Pool) (*Store, error) {
	if runtimePool == nil {
		return nil, postgresadapter.ErrNilPool
	}
	return &Store{runtimePool: runtimePool}, nil
}

// ---------------------------------------------------------------------------
// DashboardReader
// ---------------------------------------------------------------------------

// Snapshot returns the aggregate /wallet snapshot for tenantID inside
// a single tenant-scoped transaction. The reads see a consistent view
// of token_wallet, the 14-day consumption window, subscription_dunning_states,
// and the most-recent five ledger rows.
func (s *Store) Snapshot(ctx context.Context, tenantID uuid.UUID, now time.Time) (walletui.DashboardSnapshot, error) {
	if tenantID == uuid.Nil {
		return walletui.DashboardSnapshot{}, walletui.ErrTenantNotFound
	}
	var snap walletui.DashboardSnapshot
	err := postgresadapter.WithTenant(ctx, s.runtimePool, tenantID, func(tx pgx.Tx) error {
		balance, reserved, err := scanWalletBalances(ctx, tx, tenantID)
		if err != nil {
			return err
		}
		snap.Balance = balance
		snap.Reserved = reserved
		snap.Available = balance - reserved

		avg, err := scanAvgDailyConsume(ctx, tx, tenantID, now)
		if err != nil {
			return err
		}
		snap.AvgDailyConsume = avg
		if avg > 0 {
			d := int(snap.Available / avg)
			if d < 0 {
				d = 0
			}
			snap.DaysRemaining = &d
		}

		state, override, err := scanDunningRow(ctx, tx, tenantID, now)
		if err != nil {
			return err
		}
		snap.DunningState = state
		snap.DunningOverrideUntil = override

		entries, err := queryLedgerWithTx(ctx, tx, walletui.LedgerFilter{TenantID: tenantID}, time.Time{}, uuid.Nil, dashboardLastFive)
		if err != nil {
			return err
		}
		snap.LastFive = entries
		return nil
	})
	if err != nil {
		return walletui.DashboardSnapshot{}, err
	}
	if len(snap.LastFive) > 0 {
		applyBalanceAfter(snap.LastFive, snap.Balance)
	}
	return snap, nil
}

const selectWalletBalances = `
	SELECT balance, reserved
	  FROM token_wallet
	 WHERE tenant_id = $1
`

func scanWalletBalances(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID) (int64, int64, error) {
	var balance, reserved int64
	err := tx.QueryRow(ctx, selectWalletBalances, tenantID).Scan(&balance, &reserved)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, 0, walletui.ErrTenantNotFound
		}
		return 0, 0, fmt.Errorf("walletui: scan wallet balances: %w", err)
	}
	return balance, reserved, nil
}

// selectAvgDailyConsume sums the absolute commit amounts over the
// trailing 14-day window. The amount column is negative for commits
// (per SignedAmount), so we negate to get a positive "consumption"
// number, then divide by the window in days.
const selectAvgDailyConsume = `
	SELECT COALESCE(SUM(-amount), 0)
	  FROM token_ledger
	 WHERE tenant_id = $1
	   AND kind = 'commit'
	   AND occurred_at >= $2
	   AND occurred_at <  $3
`

func scanAvgDailyConsume(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, now time.Time) (int64, error) {
	from := now.Add(-time.Duration(dashboardConsumeWindowDays) * 24 * time.Hour)
	var total int64
	if err := tx.QueryRow(ctx, selectAvgDailyConsume, tenantID, from, now).Scan(&total); err != nil {
		return 0, fmt.Errorf("walletui: scan avg consume: %w", err)
	}
	if total <= 0 {
		return 0, nil
	}
	return total / int64(dashboardConsumeWindowDays), nil
}

// selectDunningRow surfaces the dunning state for the tenant's active
// subscription. JOIN through subscription so we get the row tied to
// the currently-active plan, not some legacy one.
const selectDunningRow = `
	SELECT d.state, d.override_until
	  FROM subscription s
	  JOIN subscription_dunning_states d ON d.subscription_id = s.id
	 WHERE s.tenant_id = $1
	   AND s.status = 'active'
	 LIMIT 1
`

func scanDunningRow(ctx context.Context, tx pgx.Tx, tenantID uuid.UUID, now time.Time) (string, *time.Time, error) {
	var state string
	var override *time.Time
	err := tx.QueryRow(ctx, selectDunningRow, tenantID).Scan(&state, &override)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", nil, nil
		}
		return "", nil, fmt.Errorf("walletui: scan dunning: %w", err)
	}
	// Override only relevant when it is still in effect.
	if override != nil && !override.After(now) {
		override = nil
	}
	return state, override, nil
}

// ---------------------------------------------------------------------------
// LedgerReader
// ---------------------------------------------------------------------------

// Page returns one cursor-paginated page of ledger rows in (occurred_at,
// id) DESC order. The query fetches PageSize+1 rows so HasMore is known
// without a second round trip; the extra row's cursor becomes the
// NextCursor for the follow-up call.
func (s *Store) Page(ctx context.Context, opts walletui.LedgerPageOptions) (walletui.LedgerPage, error) {
	if opts.Filter.TenantID == uuid.Nil {
		return walletui.LedgerPage{}, walletui.ErrTenantNotFound
	}
	pageSize := opts.PageSize
	if pageSize <= 0 {
		pageSize = 50
	}
	var rows []walletui.LedgerEntryView
	var current int64
	var haveBalance bool
	err := postgresadapter.WithTenant(ctx, s.runtimePool, opts.Filter.TenantID, func(tx pgx.Tx) error {
		got, err := queryLedgerWithTx(ctx, tx, opts.Filter, opts.CursorOccurredAt, opts.CursorID, pageSize+1)
		if err != nil {
			return err
		}
		rows = got
		bal, _, err := scanWalletBalances(ctx, tx, opts.Filter.TenantID)
		if err == nil {
			current = bal
			haveBalance = true
			return nil
		}
		if errors.Is(err, walletui.ErrTenantNotFound) {
			// Best-effort projection — port contract allows zero
			// BalanceAfter when the wallet row is missing.
			return nil
		}
		return err
	})
	if err != nil {
		return walletui.LedgerPage{}, err
	}
	page := walletui.LedgerPage{}
	if len(rows) > pageSize {
		page.Entries = rows[:pageSize]
		page.HasMore = true
		last := page.Entries[len(page.Entries)-1]
		page.NextCursorOccurredAt = last.OccurredAt
		page.NextCursorID = last.ID
	} else {
		page.Entries = rows
	}
	if haveBalance && len(page.Entries) > 0 && opts.CursorOccurredAt.IsZero() {
		applyBalanceAfter(page.Entries, current)
	}
	return page, nil
}

// applyBalanceAfter rolls the balance backwards across the entries
// (which are in DESC order). For the newest row, BalanceAfter ==
// current. For each older row, BalanceAfter == BalanceAfter(newer) -
// amount(newer) because subtracting the newer row's effect
// reconstructs the wallet state immediately after the older row
// settled.
func applyBalanceAfter(entries []walletui.LedgerEntryView, current int64) {
	balance := current
	for i := range entries {
		entries[i].BalanceAfter = balance
		balance -= entries[i].Amount
	}
}

// StreamCSV writes the full filtered result set to w as CSV with a
// header row. The implementation cursors over the rows from Postgres
// and writes each row to the csv.Writer as it is scanned — no whole
// result set is buffered. The csv.Writer's internal bufio buffer is
// flushed at the end (and implicitly when full mid-stream).
func (s *Store) StreamCSV(ctx context.Context, filter walletui.LedgerFilter, w io.Writer) error {
	if filter.TenantID == uuid.Nil {
		return walletui.ErrTenantNotFound
	}
	cw := csv.NewWriter(w)
	if err := cw.Write(csvHeader()); err != nil {
		return fmt.Errorf("walletui: write csv header: %w", err)
	}
	err := postgresadapter.WithTenant(ctx, s.runtimePool, filter.TenantID, func(tx pgx.Tx) error {
		kindsArg, kindsLen := kindsArg(filter.Kinds)
		fromArg, toArg := boundsArgs(filter.FromOccurredAt, filter.ToOccurredAt)
		rows, err := tx.Query(ctx, selectLedgerRows,
			filter.TenantID,
			fromArg, toArg,
			kindsArg, kindsLen,
			nil, nil,
			csvStreamLimit,
		)
		if err != nil {
			return fmt.Errorf("walletui: query csv stream: %w", err)
		}
		defer rows.Close()
		for rows.Next() {
			view, err := scanLedgerRow(rows)
			if err != nil {
				return err
			}
			if err := cw.Write(csvRow(view)); err != nil {
				return fmt.Errorf("walletui: write csv row: %w", err)
			}
		}
		if err := rows.Err(); err != nil {
			return fmt.Errorf("walletui: iterate csv stream: %w", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		return fmt.Errorf("walletui: flush csv: %w", err)
	}
	return nil
}

// csvStreamLimit is an arbitrarily-large LIMIT used by the CSV stream
// to keep the same prepared statement as the paginated path while
// effectively returning every row in the filter window. Real exports
// stay well below this number (the gerente UI is per-tenant); the cap
// exists as defense-in-depth against an unbounded scan.
const csvStreamLimit = 1_000_000

func csvHeader() []string {
	return []string{
		"id",
		"occurred_at",
		"kind",
		"source",
		"amount",
		"conversation_id_hash",
		"model",
		"policy_id",
		"external_ref",
	}
}

func csvRow(v walletui.LedgerEntryView) []string {
	policy := ""
	if v.PolicyID != uuid.Nil {
		policy = v.PolicyID.String()
	}
	return []string{
		v.ID.String(),
		v.OccurredAt.UTC().Format(time.RFC3339),
		string(v.Kind),
		string(v.Source),
		strconv.FormatInt(v.Amount, 10),
		v.ConversationIDHash,
		v.Model,
		policy,
		v.ExternalRef,
	}
}

// ---------------------------------------------------------------------------
// TopupCatalogReader
// ---------------------------------------------------------------------------

const selectTopupPackages = `
	SELECT id, slug, name, tokens, price_cents_brl
	  FROM token_packages
	 WHERE kind = 'tokens'
	 ORDER BY price_cents_brl ASC
`

// ListPackages returns the active token_packages ordered by price ASC.
// PricePerKToken is computed in Go (round-half-up to nearest cent).
//
// token_packages has no RLS, so this read does NOT go through
// WithTenant — the catalogue is identical across tenants.
func (s *Store) ListPackages(ctx context.Context) ([]walletui.TopupPackage, error) {
	rows, err := s.runtimePool.Query(ctx, selectTopupPackages)
	if err != nil {
		return nil, fmt.Errorf("walletui: query packages: %w", err)
	}
	defer rows.Close()
	out := make([]walletui.TopupPackage, 0)
	for rows.Next() {
		var (
			id     uuid.UUID
			slug   string
			name   string
			tokens int64
			price  int
		)
		if err := rows.Scan(&id, &slug, &name, &tokens, &price); err != nil {
			return nil, fmt.Errorf("walletui: scan package: %w", err)
		}
		out = append(out, walletui.TopupPackage{
			ID:             id,
			Slug:           slug,
			Name:           name,
			Tokens:         tokens,
			PriceCentsBRL:  price,
			PricePerKToken: pricePerKToken(price, tokens),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("walletui: iterate packages: %w", err)
	}
	return out, nil
}

// pricePerKToken returns priceCents * 1000 / tokens rounded half-up.
// Tokens == 0 returns 0 (the table CHECK forbids zero tokens, but the
// guard keeps the helper total).
func pricePerKToken(priceCents int, tokens int64) int {
	if tokens <= 0 {
		return 0
	}
	num := int64(priceCents) * 1000
	q := num / tokens
	r := num % tokens
	// Round half-up: if 2*remainder >= divisor, bump.
	if r*2 >= tokens {
		q++
	}
	return int(q)
}

// ---------------------------------------------------------------------------
// Ledger SQL — shared by dashboard last-five, paginated page, and CSV stream
// ---------------------------------------------------------------------------

// selectLedgerRows is the shared read path for every walletui ledger
// view. Parameters:
//
//	$1 tenant_id   — uuid (paranoia filter; WithTenant + RLS already gate)
//	$2 from_at     — timestamptz NULL = no lower bound
//	$3 to_at       — timestamptz NULL = no upper bound
//	$4 kinds       — text[] NULL = all kinds
//	$5 kinds_len   — int NULL = "$4 is NULL" (driver-friendly)
//	$6 cursor_at   — timestamptz NULL = first page / no cursor
//	$7 cursor_id   — uuid (paired with $6)
//	$8 limit       — int LIMIT
//
// metadata is returned as the raw jsonb bytes and parsed in Go (see
// extractLGPDFields). The hash/model/policy_id derivation happens in
// the adapter so the redaction logic stays in one place — adapters
// updating the rule (e.g. switching to sha256[:24] or salting the hash)
// touch one Go helper rather than reasoning about jsonb operators inside
// SQL.
const selectLedgerRows = `
	SELECT id, occurred_at, kind, source, amount,
	       metadata,
	       COALESCE(external_ref, '')
	  FROM token_ledger
	 WHERE tenant_id = $1
	   AND ($2::timestamptz IS NULL OR occurred_at >= $2::timestamptz)
	   AND ($3::timestamptz IS NULL OR occurred_at <  $3::timestamptz)
	   AND ($5::int IS NULL OR kind = ANY($4::text[]))
	   AND ($6::timestamptz IS NULL
	        OR occurred_at < $6::timestamptz
	        OR (occurred_at = $6::timestamptz AND id < $7::uuid))
	 ORDER BY occurred_at DESC, id DESC
	 LIMIT $8
`

// queryLedgerWithTx is the WithTenant-internal worker that runs the
// shared ledger SELECT with the given filter, cursor and limit, and
// decodes each row into a walletui.LedgerEntryView. queryLedgerWithTx
// itself does NOT open a transaction — the caller (Page / Snapshot) is
// responsible for the WithTenant scope.
func queryLedgerWithTx(ctx context.Context, tx pgx.Tx, filter walletui.LedgerFilter, cursorAt time.Time, cursorID uuid.UUID, limit int) ([]walletui.LedgerEntryView, error) {
	kindsArg, kindsLen := kindsArg(filter.Kinds)
	fromArg, toArg := boundsArgs(filter.FromOccurredAt, filter.ToOccurredAt)
	var cursorAtArg any
	var cursorIDArg any
	if !cursorAt.IsZero() {
		cursorAtArg = cursorAt
		cursorIDArg = cursorID
	}
	rows, err := tx.Query(ctx, selectLedgerRows,
		filter.TenantID,
		fromArg, toArg,
		kindsArg, kindsLen,
		cursorAtArg, cursorIDArg,
		limit,
	)
	if err != nil {
		return nil, fmt.Errorf("walletui: query ledger: %w", err)
	}
	defer rows.Close()
	return readLedgerRows(rows, limit)
}

func readLedgerRows(rows pgx.Rows, capHint int) ([]walletui.LedgerEntryView, error) {
	if capHint < 0 {
		capHint = 0
	}
	out := make([]walletui.LedgerEntryView, 0, capHint)
	for rows.Next() {
		view, err := scanLedgerRow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, view)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("walletui: iterate ledger: %w", err)
	}
	return out, nil
}

func scanLedgerRow(rows pgx.Rows) (walletui.LedgerEntryView, error) {
	var (
		id           uuid.UUID
		occurredAt   time.Time
		kind, source string
		amount       int64
		metaRaw      []byte
		externalRef  string
	)
	if err := rows.Scan(&id, &occurredAt, &kind, &source, &amount, &metaRaw, &externalRef); err != nil {
		return walletui.LedgerEntryView{}, fmt.Errorf("walletui: scan ledger row: %w", err)
	}
	view := walletui.LedgerEntryView{
		ID:          id,
		OccurredAt:  occurredAt,
		Kind:        wallet.LedgerKind(kind),
		Source:      wallet.LedgerSource(source),
		Amount:      amount,
		ExternalRef: externalRef,
	}
	hash, model, policy := extractLGPDFields(metaRaw)
	view.ConversationIDHash = hash
	view.Model = model
	view.PolicyID = policy
	return view, nil
}

// extractLGPDFields decodes the LGPD-safe projection of a token_ledger
// row's metadata jsonb. The raw conversation_id is one-way hashed
// (sha256, hex, first 16 chars); model and ai_policy_id pass through
// because they are operational metadata, not PII.
//
// Decode errors collapse to empty fields — a malformed metadata column
// must NOT fail the page; the worst it can do is render "—" in the UI.
func extractLGPDFields(raw []byte) (hash, model string, policyID uuid.UUID) {
	if len(raw) == 0 {
		return "", "", uuid.Nil
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		return "", "", uuid.Nil
	}
	if v, ok := stringField(m, "conversation_id"); ok {
		hash = hashConversationID(v)
	}
	if v, ok := stringField(m, "model"); ok {
		model = v
	}
	if v, ok := stringField(m, "ai_policy_id"); ok {
		if parsed, err := uuid.Parse(v); err == nil {
			policyID = parsed
		}
	}
	return hash, model, policyID
}

// hashConversationID returns hex(sha256(raw))[:16]. Empty input returns
// empty output (handled by the caller, which checks the metadata key
// presence first).
func hashConversationID(raw string) string {
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])[:16]
}

func stringField(m map[string]any, key string) (string, bool) {
	v, ok := m[key]
	if !ok {
		return "", false
	}
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

// kindsArg packages filter.Kinds into the (kinds_text, kinds_len)
// parameter pair expected by selectLedgerRows. nil/empty filter means
// "no kind restriction" — both args are nil.
func kindsArg(kinds []wallet.LedgerKind) (any, any) {
	if len(kinds) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(kinds))
	for _, k := range kinds {
		out = append(out, string(k))
	}
	return out, len(out)
}

// boundsArgs converts zero-value times into nil so the SQL IS NULL
// branch fires (no lower/upper bound). Non-zero times pass through.
func boundsArgs(from, to time.Time) (any, any) {
	var fromArg, toArg any
	if !from.IsZero() {
		fromArg = from
	}
	if !to.IsZero() {
		toArg = to
	}
	return fromArg, toArg
}
