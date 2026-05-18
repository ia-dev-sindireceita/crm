package engine_test

import "encoding/json"

// jsonMarshal is a tiny indirection so the event_test.go fixtures can
// reuse the encoder without re-importing encoding/json across test
// helpers. Kept in a *_test.go file so the production package stays
// stdlib-light.
func jsonMarshal(v any) ([]byte, error) {
	return json.Marshal(v)
}
