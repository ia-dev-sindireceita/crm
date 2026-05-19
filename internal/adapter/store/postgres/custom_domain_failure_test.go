package postgres_test

// SIN-63080 — coverage for ListPendingVerification + MarkFailed, the
// two new methods on CustomDomainStore that back the DNS-poller worker
// (cmd/customdomain-verifier). These tests live in their own file with
// their own row fixture so the existing custom_domain_store_test.go
// fixtures (which scan the original 11-column projection) are not
// touched.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	pgstore "github.com/pericles-luz/crm/internal/adapter/store/postgres"
)

// customDomainFailureRow mirrors the 13-column SELECT projection that
// ListPendingVerification + MarkFailed emit. It is intentionally
// distinct from customDomainRow so the scan-arity contracts cannot
// silently drift.
type customDomainFailureRow struct {
	id, tenant     [16]byte
	host, token    string
	verifiedAt     *time.Time
	dnssec         bool
	pausedAt       *time.Time
	deletedAt      *time.Time
	failedAt       *time.Time
	failureReason  *string
	dnsLogID       *[16]byte
	createdAt, upd time.Time
	scanErr        error
}

type failureRows struct {
	values []customDomainFailureRow
	pos    int
	err    error
	closed bool
}

func (f *failureRows) Close()                                       { f.closed = true }
func (f *failureRows) Err() error                                   { return f.err }
func (f *failureRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (f *failureRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (f *failureRows) RawValues() [][]byte                          { return nil }
func (f *failureRows) Values() ([]any, error)                       { return nil, nil }
func (f *failureRows) Conn() *pgx.Conn                              { return nil }
func (f *failureRows) Next() bool {
	if f.pos >= len(f.values) {
		return false
	}
	f.pos++
	return true
}
func (f *failureRows) Scan(dest ...any) error {
	if f.pos == 0 || f.pos > len(f.values) {
		return errors.New("failureRows: scan before next")
	}
	r := f.values[f.pos-1]
	if r.scanErr != nil {
		return r.scanErr
	}
	return scanIntoCustomDomainFailureDest(dest, r)
}

// scanIntoCustomDomainFailureDest implements Scan for the extended 13-
// column row shape: id, tenant_id, host, verification_token, verified_at,
// verified_with_dnssec, tls_paused_at, deleted_at, failed_at,
// failure_reason, dns_resolution_log_id, created_at, updated_at.
func scanIntoCustomDomainFailureDest(dest []any, r customDomainFailureRow) error {
	if len(dest) != 13 {
		return errors.New("scan: arity")
	}
	mapping := []any{
		r.id, r.tenant, r.host, r.token, r.verifiedAt, r.dnssec,
		r.pausedAt, r.deletedAt, r.failedAt, r.failureReason,
		r.dnsLogID, r.createdAt, r.upd,
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
		case **string:
			v, ok := src.(*string)
			if !ok && src != nil {
				return errors.New("scan: bad **string")
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

// failurePgxRowAdapter wraps one customDomainFailureRow into a pgx.Row
// so the MarkFailed path (QueryRow) shares the scan logic.
type failurePgxRowAdapter struct {
	row customDomainFailureRow
	err error
}

func (p failurePgxRowAdapter) Scan(dest ...any) error {
	if p.err != nil {
		return p.err
	}
	return scanIntoCustomDomainFailureDest(dest, p.row)
}

func mkFailureRow() customDomainFailureRow {
	return customDomainFailureRow{
		id:        [16]byte{0xaa},
		tenant:    [16]byte{0xbb},
		host:      "shop.example.com",
		token:     "tok",
		createdAt: time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
		upd:       time.Date(2026, 5, 6, 12, 0, 0, 0, time.UTC),
	}
}

func TestCustomDomainStore_ListPendingVerification_ReturnsRows(t *testing.T) {
	t.Parallel()
	r1 := mkFailureRow()
	r2 := mkFailureRow()
	r2.host = "other.example.com"
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) {
			return &failureRows{values: []customDomainFailureRow{r1, r2}}, nil
		},
	}
	store := pgstore.NewCustomDomainStore(conn)
	out, err := store.ListPendingVerification(context.Background())
	if err != nil {
		t.Fatalf("ListPendingVerification: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("rows = %d, want 2", len(out))
	}
	if out[0].Host != "shop.example.com" || out[1].Host != "other.example.com" {
		t.Errorf("unexpected order: %+v", out)
	}
}

func TestCustomDomainStore_ListPendingVerification_PopulatesFailureMeta(t *testing.T) {
	t.Parallel()
	failedAt := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	reason := "cap_exceeded"
	r := mkFailureRow()
	r.failedAt = &failedAt
	r.failureReason = &reason
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) {
			return &failureRows{values: []customDomainFailureRow{r}}, nil
		},
	}
	store := pgstore.NewCustomDomainStore(conn)
	out, err := store.ListPendingVerification(context.Background())
	if err != nil {
		t.Fatalf("ListPendingVerification: %v", err)
	}
	if len(out) != 1 {
		t.Fatalf("rows = %d, want 1", len(out))
	}
	if out[0].FailedAt == nil || !out[0].FailedAt.Equal(failedAt) {
		t.Errorf("FailedAt = %v, want %v", out[0].FailedAt, failedAt)
	}
	if out[0].FailureReason != reason {
		t.Errorf("FailureReason = %q, want %q", out[0].FailureReason, reason)
	}
}

func TestCustomDomainStore_ListPendingVerification_QueryError(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) { return nil, errors.New("conn lost") },
	}
	if _, err := pgstore.NewCustomDomainStore(conn).ListPendingVerification(context.Background()); err == nil {
		t.Fatal("expected error")
	}
}

func TestCustomDomainStore_ListPendingVerification_ScanError(t *testing.T) {
	t.Parallel()
	r := mkFailureRow()
	r.scanErr = errors.New("driver lost")
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) {
			return &failureRows{values: []customDomainFailureRow{r}}, nil
		},
	}
	if _, err := pgstore.NewCustomDomainStore(conn).ListPendingVerification(context.Background()); err == nil {
		t.Fatal("expected scan error")
	}
}

func TestCustomDomainStore_ListPendingVerification_RowsErr(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		query: func(string, ...any) (pgx.Rows, error) {
			return &failureRows{err: errors.New("rows broken")}, nil
		},
	}
	if _, err := pgstore.NewCustomDomainStore(conn).ListPendingVerification(context.Background()); err == nil {
		t.Fatal("expected rows error")
	}
}

func TestCustomDomainStore_MarkFailed_ReturnsRow(t *testing.T) {
	t.Parallel()
	failedAt := time.Date(2026, 5, 6, 14, 0, 0, 0, time.UTC)
	reason := "cap_exceeded"
	r := mkFailureRow()
	r.failedAt = &failedAt
	r.failureReason = &reason
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row {
			return failurePgxRowAdapter{row: r}
		},
	}
	store := pgstore.NewCustomDomainStore(conn)
	out, err := store.MarkFailed(context.Background(), uuid.UUID(r.id), failedAt, reason)
	if err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	if out.FailedAt == nil || !out.FailedAt.Equal(failedAt) {
		t.Errorf("FailedAt = %v, want %v", out.FailedAt, failedAt)
	}
	if out.FailureReason != reason {
		t.Errorf("FailureReason = %q, want %q", out.FailureReason, reason)
	}
}

func TestCustomDomainStore_MarkFailed_NotFound(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row {
			return failurePgxRowAdapter{err: pgx.ErrNoRows}
		},
	}
	if _, err := pgstore.NewCustomDomainStore(conn).MarkFailed(context.Background(), uuid.New(), time.Now(), "cap_exceeded"); err == nil {
		t.Fatal("expected not-found error")
	}
}

func TestCustomDomainStore_MarkFailed_ScanError(t *testing.T) {
	t.Parallel()
	conn := stubRowsConn{
		queryRow: func(string, ...any) pgx.Row {
			return failurePgxRowAdapter{err: errors.New("driver fail")}
		},
	}
	if _, err := pgstore.NewCustomDomainStore(conn).MarkFailed(context.Background(), uuid.New(), time.Now(), "cap_exceeded"); err == nil {
		t.Fatal("expected scan error")
	}
}
