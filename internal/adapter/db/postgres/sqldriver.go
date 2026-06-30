// Package postgres also registers a database/sql driver under the name
// "postgres" so third-party libraries that go through database/sql can reach
// Postgres through the same pgx stack the app uses everywhere else.
//
// The app's own data access never touches database/sql — it uses pgxpool
// directly (see New). The one exception is the whatsmeow session store
// (internal/wasession/whatsmeowdev): its sqlstore.New hard-codes the
// "postgres" dialect, which database/sql resolves to a registered driver of
// the same name. Without a registered "postgres" driver the WhatsApp-session
// transport fails to boot with `sql: unknown driver "postgres" (forgotten
// import?)` (SIN-66307). pgx's stdlib shim registers itself as "pgx"; we add
// the "postgres" alias here, inside the postgres adapter package (the only
// place the forbidimport guard, SIN-62216, permits importing a SQL driver),
// so the registration is in the binary whenever the adapter is linked — which
// cmd/server always does.
package postgres

import (
	"database/sql"

	"github.com/jackc/pgx/v5/stdlib"
)

func init() {
	// stdlib's own init already registers "pgx"; alias "postgres" to the same
	// driver. Guard against a double registration (sql.Register panics on a
	// duplicate name) in case another driver claims "postgres" first.
	for _, name := range sql.Drivers() {
		if name == "postgres" {
			return
		}
	}
	sql.Register("postgres", stdlib.GetDefaultDriver())
}
