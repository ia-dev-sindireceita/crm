package postgres_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
	"github.com/pericles-luz/crm/internal/customdomain/management"
)

// stubRowsConn extends stubConn (defined in store_test.go) with Query
// support so the custom-domain List path is testable without a real DB.
// We can't embed stubConn (private struct from another file in the same
// _test package), so we redefine the minimal surface here.
type stubRowsConn struct {
	queryRow func(sql string, args ...any) pgx.Row
	exec     func(sql string, args ...any) (pgconn.CommandTag, error)
	query    func(sql string, args ...any) (pgx.Rows, error)
}

func (s stubRowsConn) QueryRow(_ context.Context, sql string, args ...any) pgx.Row {
	return s.queryRow(sql, args...)
}
func (s stubRowsConn) Exec(_ context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if s.exec == nil {
		return pgconn.CommandTag{}, nil
	}
	return s.exec(sql, args...)
}
func (s stubRowsConn) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	return s.query(sql, args...)
}

// fakeRows scripts pgx.Rows for List tests. It walks `values` one row at
// a time; each entry mirrors what scanCustomDomainRow expects.
type fakeRows struct {
	values []customDomainRow
	pos    int
	err    error
	closed bool
}

type customDomainRow struct {
	id, tenant     [16]byte
	host, token    string
	verifiedAt     *time.Time
	dnssec         bool
	pausedAt       *time.Time
	deletedAt      *time.Time
	dnsLogID       *[16]byte
	tokenIssuedAt  time.Time
	createdAt, upd time.Time
	scanErr        error
}

func (f *fakeRows) Close()                                       { f.closed = true }
func (f *fakeRows) Err() error                                   { return f.err }
func (f *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (f *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (f *fakeRows) RawValues() [][]byte                          { return nil }
func (f *fakeRows) Values() ([]any, error)                       { return nil, nil }
func (f *fakeRows) Conn() *pgx.Conn                              { return nil }
func (f *fakeRows) Next() bool {
	if f.pos >= len(f.values) {
		return false
	}
	f.pos++
	return true
}
func (f *fakeRows) Scan(dest ...any) error {
	if f.pos == 0 || f.pos > len(f.values) {
		return errors.New("fakeRows: scan before next")
	}
	r := f.values[f.pos-1]
	if r.scanErr != nil {
		return r.scanErr
	}
	return scanIntoCustomDomainDest(dest, r)
}

// scanIntoCustomDomainDest implements the same Scan semantics
// scanCustomDomainRow expects from a *pgx.Row. It is duplicated for
// fakeRows; the QueryRow path uses fakeRow from store_test.go which only
// covers the limited shapes we already exercise. Custom-domain scans
// pull a wider set of types (uuid pointers, *time.Time pointers).
func scanIntoCustomDomainDest(dest []any, r customDomainRow) error {
	if len(dest) != 12 {
		return errors.New("scan: arity")
	}
	mapping := []any{
		r.id, r.tenant, r.host, r.token, r.verifiedAt, r.dnssec,
		r.pausedAt, r.deletedAt, r.dnsLogID, r.tokenIssuedAt, r.createdAt, r.upd,
	}
	for i, src := range mapping {
		switch p := dest[i].(type) {
		case *[16]byte:
			v, ok := src.([16]byte)
			if !ok {
				return errors.New("scan: bad [16]byte")
			}
			*p = v
		case **[16]byte:
			v, ok := src.(*[16]byte)
			if !ok && src != nil {
				return errors.New("scan: bad **[16]byte")
			}
			*p = v
		case *string:
			v, ok := src.(string)
			if !ok {
				return errors.New("scan: bad string")
			}
			*p = v
		case *bool:
			v, ok := src.(bool)
			if !ok {
				return errors.New("scan: bad bool")
			}
			*p = v
		case **time.Time:
			v, ok := src.(*time.Time)
			if !ok && src != nil {
				return errors.New("scan: bad **time.Time")
			}
			*p = v
		case *time.Time:
			v, ok := src.(time.Time)
			if !ok {
				return errors.New("scan: bad time.Time")
			}
			*p = v
		default:
			return errors.New("scan: unsupported dest type")
		}
	}
	return nil
}

// pgxRowAdapter wraps a single customDomainRow into a pgx.Row so the
// QueryRow paths share the scan logic without redefining fakeRow.
type pgxRowAdapter struct {
	row customDomainRow
	err error
}

func (p pgxRowAdapter) Scan(dest ...any) error {
	if p.err != nil {
		return p.err
	}
	return scanIntoCustomDomainDest(dest, p.row)
}

func mkRow() customDomainRow {
	return customDomainRow{
		id:            [16]byte{0xaa},
		tenant:        [16]byte{0xbb},
		host:          "shop.example.com",
		token:         "tok",
		tokenIssuedAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		createdAt:     time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		upd:           time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
	}
}

func TestCustomDomainStore_ListReturnsRows(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 6, 13, 0, 0, 0, time.UTC)
	r1 := mkRow()
	r2 := mkRow()
	r2.host = "another.example.com"
	r2.verifiedAt = &verified
	r2.dnssec = true
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) {
			return &fakeRows{values: []customDomainRow{r1, r2}}, nil
		},
	}
	store := pgstore.NewCustomDomainStore(conn)
	out, err := store.List(context.Background(), uuid.UUID(r1.tenant))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d rows", len(out))
	}
	if out[0].Host != "shop.example.com" || out[1].VerifiedAt == nil || !out[1].VerifiedWithDNSSEC {
		t.Fatalf("unexpected rows: %+v", out)
	}
}

func TestCustomDomainStore_ListQueryError(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) { return nil, errors.New("conn lost") },
	}
	if _, err := pgstore.NewCustomDomainStore(conn).List(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCustomDomainStore_ListScanError(t *testing.T) {
	t.Parallel()
	r := mkRow()
	r.scanErr = errors.New("driver lost")
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) { return &fakeRows{values: []customDomainRow{r}}, nil },
	}
	if _, err := pgstore.NewCustomDomainStore(conn).List(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestCustomDomainStore_ListRowsErr(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) {
			return &fakeRows{err: errors.New("row error")}, nil
		},
	}
	if _, err := pgstore.NewCustomDomainStore(conn).List(context.Background(), uuid.New()); err == nil {
		t.Fatal("expected rows.Err propagation")
	}
}

func TestCustomDomainStore_GetByID_Found(t *testing.T) {
	t.Parallel()
	r := mkRow()
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{row: r} },
	}
	d, err := pgstore.NewCustomDomainStore(conn).GetByID(context.Background(), uuid.UUID(r.id))
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if d.Host != r.host {
		t.Fatalf("host = %q", d.Host)
	}
}

func TestCustomDomainStore_GetByID_NotFound(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{err: pgx.ErrNoRows} },
	}
	_, err := pgstore.NewCustomDomainStore(conn).GetByID(context.Background(), uuid.New())
	if !errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v, want ErrStoreNotFound", err)
	}
}

func TestCustomDomainStore_GetByID_DriverError(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{err: errors.New("driver")} },
	}
	_, err := pgstore.NewCustomDomainStore(conn).GetByID(context.Background(), uuid.New())
	if err == nil || errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v", err)
	}
}

func TestCustomDomainStore_Insert(t *testing.T) {
	t.Parallel()
	r := mkRow()
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{row: r} },
	}
	d, err := pgstore.NewCustomDomainStore(conn).Insert(context.Background(), management.Domain{
		ID: uuid.UUID(r.id), TenantID: uuid.UUID(r.tenant), Host: r.host,
		VerificationToken: r.token, CreatedAt: r.createdAt,
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if d.Host != r.host {
		t.Fatalf("host = %q", d.Host)
	}
}

func TestCustomDomainStore_MarkVerified(t *testing.T) {
	t.Parallel()
	verified := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	logID := uuid.New()
	r := mkRow()
	r.verifiedAt = &verified
	r.dnssec = true
	r.dnsLogID = (*[16]byte)(&logID)
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{row: r} },
	}
	d, err := pgstore.NewCustomDomainStore(conn).MarkVerified(context.Background(), uuid.UUID(r.id), r.token, verified, true, &logID)
	if err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
	if d.VerifiedAt == nil || !d.VerifiedWithDNSSEC {
		t.Fatalf("verified flags missing: %+v", d)
	}
	if d.DNSResolutionLogID == nil || *d.DNSResolutionLogID != logID {
		t.Fatalf("logID = %v", d.DNSResolutionLogID)
	}
}

func TestCustomDomainStore_MarkVerified_NilLogID(t *testing.T) {
	t.Parallel()
	r := mkRow()
	conn := stubRowsConn{queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{row: r} }}
	if _, err := pgstore.NewCustomDomainStore(conn).MarkVerified(context.Background(), uuid.UUID(r.id), r.token, time.Now(), false, nil); err != nil {
		t.Fatalf("MarkVerified: %v", err)
	}
}

func TestCustomDomainStore_SetPaused_PauseAndResume(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 16, 0, 0, 0, time.UTC)
	r := mkRow()
	r.pausedAt = &now
	conn := stubRowsConn{queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{row: r} }}
	store := pgstore.NewCustomDomainStore(conn)
	if _, err := store.SetPaused(context.Background(), uuid.UUID(r.id), &now); err != nil {
		t.Fatalf("pause: %v", err)
	}
	r.pausedAt = nil
	if _, err := store.SetPaused(context.Background(), uuid.UUID(r.id), nil); err != nil {
		t.Fatalf("resume: %v", err)
	}
}

func TestCustomDomainStore_SoftDelete(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 5, 6, 17, 0, 0, 0, time.UTC)
	r := mkRow()
	r.deletedAt = &now
	conn := stubRowsConn{queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{row: r} }}
	d, err := pgstore.NewCustomDomainStore(conn).SoftDelete(context.Background(), uuid.UUID(r.id), now)
	if err != nil {
		t.Fatalf("SoftDelete: %v", err)
	}
	if d.DeletedAt == nil || !d.DeletedAt.Equal(now) {
		t.Fatalf("deletedAt = %v", d.DeletedAt)
	}
}

func TestCustomDomainStore_SoftDelete_NotFound(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{err: pgx.ErrNoRows} }}
	_, err := pgstore.NewCustomDomainStore(conn).SoftDelete(context.Background(), uuid.New(), time.Now())
	if !errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// probeRow scripts a [16]byte / int / *time.Time SELECT probe used by
// the MarkVerified CAS discriminator. Distinct from pgxRowAdapter, which
// only handles the full custom-domain row shape.
type probeRow struct {
	probeVal int
	err      error
}

func (p probeRow) Scan(dest ...any) error {
	if p.err != nil {
		return p.err
	}
	if len(dest) != 1 {
		return errors.New("probeRow: arity")
	}
	switch d := dest[0].(type) {
	case *int:
		*d = p.probeVal
	default:
		return errors.New("probeRow: unsupported dest")
	}
	return nil
}

// TestCustomDomainStore_MarkVerified_DriverError — SIN-63104. Non-
// ErrNoRows errors from the CAS UPDATE must propagate unchanged
// (neither ErrTokenRotated nor ErrStoreNotFound).
func TestCustomDomainStore_MarkVerified_DriverError(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row { return pgxRowAdapter{err: errors.New("driver boom")} },
	}
	_, err := pgstore.NewCustomDomainStore(conn).MarkVerified(context.Background(), uuid.New(), "tok", time.Now(), false, nil)
	if err == nil {
		t.Fatal("expected driver error")
	}
	if errors.Is(err, management.ErrTokenRotated) || errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("unexpected typed error: %v", err)
	}
}

// TestCustomDomainStore_MarkVerified_TokenRotated_ProbeHit — SIN-63104.
// CAS UPDATE matches zero rows AND probe SELECT finds the row →
// ErrTokenRotated.
func TestCustomDomainStore_MarkVerified_TokenRotated_ProbeHit(t *testing.T) {
	t.Parallel()
	calls := 0
	conn := stubRowsConn{
		queryRow: func(_ string, _ ...any) pgx.Row {
			calls++
			if calls == 1 {
				return pgxRowAdapter{err: pgx.ErrNoRows}
			}
			return probeRow{probeVal: 1}
		},
	}
	_, err := pgstore.NewCustomDomainStore(conn).MarkVerified(context.Background(), uuid.New(), "tok", time.Now(), false, nil)
	if !errors.Is(err, management.ErrTokenRotated) {
		t.Fatalf("err = %v, want ErrTokenRotated", err)
	}
}

// TestCustomDomainStore_MarkVerified_NotFound_ProbeMiss — SIN-63104.
// CAS UPDATE matches zero rows AND probe SELECT also returns ErrNoRows
// → ErrStoreNotFound.
func TestCustomDomainStore_MarkVerified_NotFound_ProbeMiss(t *testing.T) {
	t.Parallel()
	calls := 0
	conn := stubRowsConn{
		queryRow: func(_ string, _ ...any) pgx.Row {
			calls++
			if calls == 1 {
				return pgxRowAdapter{err: pgx.ErrNoRows}
			}
			return probeRow{err: pgx.ErrNoRows}
		},
	}
	_, err := pgstore.NewCustomDomainStore(conn).MarkVerified(context.Background(), uuid.New(), "tok", time.Now(), false, nil)
	if !errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("err = %v, want ErrStoreNotFound", err)
	}
}

// TestCustomDomainStore_MarkVerified_ProbeDriverError — SIN-63104.
// CAS UPDATE matches zero rows AND probe SELECT errors with a non-
// ErrNoRows driver error → wrapped error, neither typed sentinel.
func TestCustomDomainStore_MarkVerified_ProbeDriverError(t *testing.T) {
	t.Parallel()
	calls := 0
	conn := stubRowsConn{
		queryRow: func(_ string, _ ...any) pgx.Row {
			calls++
			if calls == 1 {
				return pgxRowAdapter{err: pgx.ErrNoRows}
			}
			return probeRow{err: errors.New("probe boom")}
		},
	}
	_, err := pgstore.NewCustomDomainStore(conn).MarkVerified(context.Background(), uuid.New(), "tok", time.Now(), false, nil)
	if err == nil {
		t.Fatal("expected probe driver error")
	}
	if errors.Is(err, management.ErrTokenRotated) || errors.Is(err, management.ErrStoreNotFound) {
		t.Fatalf("unexpected typed error: %v", err)
	}
}
