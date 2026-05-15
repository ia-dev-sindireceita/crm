// Fixture: a web/inbox package that violates the rule by importing the
// domain root directly. The diagnostic must point at the import line.
package bad

import (
	_ "github.com/pericles-luz/crm/internal/inbox" // want `forbidden import "github.com/pericles-luz/crm/internal/inbox" from internal/web/...`
)
