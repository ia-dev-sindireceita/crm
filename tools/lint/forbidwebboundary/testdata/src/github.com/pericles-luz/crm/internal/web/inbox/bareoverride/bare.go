// Fixture: a bare override marker (no justification) does NOT silence
// the rule. The diagnostic must still fire so override review surfaces
// the missing reason.
package bareoverride

import (
	// forbidwebboundary:ok
	_ "github.com/pericles-luz/crm/internal/inbox" // want `forbidden import "github.com/pericles-luz/crm/internal/inbox" from internal/web/...`
)
