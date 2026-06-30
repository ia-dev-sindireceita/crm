package postgres

import (
	"database/sql"
	"testing"
)

// TestPostgresSQLDriverRegistered guards the SIN-66307 regression: whatsmeow's
// sqlstore.New(ctx, "postgres", dsn, ...) goes through database/sql, which
// needs a driver registered under the name "postgres". The app uses pgxpool
// directly and never otherwise links a database/sql driver, so without
// sqldriver.go's init the WhatsApp-session transport fails to boot with
// `sql: unknown driver "postgres" (forgotten import?)`.
func TestPostgresSQLDriverRegistered(t *testing.T) {
	for _, name := range sql.Drivers() {
		if name == "postgres" {
			return
		}
	}
	t.Fatalf(`database/sql driver "postgres" not registered (have %v); whatsmeow sqlstore.New would fail with unknown driver (SIN-66307)`, sql.Drivers())
}
