// Fixture: a clean web/inbox package that only imports the use case
// path. The analyzer must not emit any diagnostic.
package good

import (
	_ "github.com/pericles-luz/crm/internal/inbox/usecase"
)
