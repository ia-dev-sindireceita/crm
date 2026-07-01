package backup

// AC proof: this undefined symbol is invisible to go build ./cmd/server
// but caught by go build ./... — demonstrating the gap this workflow closes.
var _ = ThisSymbolDoesNotExist
