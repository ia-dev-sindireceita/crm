// Fixture: an annotated override silences the diagnostic when the
// reason is non-empty.
package override

import (
	// forbidwebboundary:ok temporary bridge while PR10 lands
	_ "github.com/pericles-luz/crm/internal/inbox"
)
