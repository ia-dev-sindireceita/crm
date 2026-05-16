package openrouter

import "errors"

// ErrUpstream5xx signals that the OpenRouter API returned a 5xx status
// after the configured retry budget was exhausted. Callers should
// degrade gracefully (cache fallback, "try again later" UI) rather than
// retrying again — the adapter has already burned its retry budget.
var ErrUpstream5xx = errors.New("openrouter: upstream 5xx after retries")

// ErrRateLimited signals an HTTP 429 from OpenRouter after the
// configured retry budget was exhausted. The wallet usecase
// (internal/aiassist) is expected to roll back any reservation and
// surface this as a transient failure to the caller.
var ErrRateLimited = errors.New("openrouter: rate limited after retries")

// ErrTimeout signals that the request did not complete within the
// per-call deadline (default 8s p99 per ADR-0040). Wrapping context
// errors lets callers branch with errors.Is(err, openrouter.ErrTimeout)
// without depending on context.DeadlineExceeded directly.
var ErrTimeout = errors.New("openrouter: request timed out")

// ErrBadRequest signals a 4xx (non-429) response. These are NOT retried
// — they indicate a malformed prompt, invalid model, or auth failure
// that retrying cannot fix.
var ErrBadRequest = errors.New("openrouter: bad request")

// ErrInvalidResponse signals that OpenRouter returned a 200 OK with a
// payload the adapter could not decode (missing choices, empty content,
// usage fields absent). It is non-retryable: a malformed response
// surfaces a provider-side regression that ops should investigate.
var ErrInvalidResponse = errors.New("openrouter: invalid response payload")
