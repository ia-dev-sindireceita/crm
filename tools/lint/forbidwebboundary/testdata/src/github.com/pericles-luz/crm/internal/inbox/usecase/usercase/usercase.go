// Fixture: a non-web package that imports the inbox root. The
// analyzer's scope must NOT fire here (it only checks internal/web/...).
package usercase

import (
	_ "github.com/pericles-luz/crm/internal/inbox"
)
