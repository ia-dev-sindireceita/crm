// Package factory wires an email.EmailSender from environment
// variables. It is the only place in the codebase that is allowed to
// import every adapter (mailgun, noop, recorder); the rest of the
// codebase consumes the resulting EmailSender through the domain port.
//
// Selection table:
//
//	EMAIL_PROVIDER=mailgun  → mailgun.New (requires MAILGUN_API_KEY,
//	                          MAILGUN_DOMAIN, MAILGUN_REGION)
//	EMAIL_PROVIDER=recorder → recorder.New
//	EMAIL_PROVIDER=noop OR  → noop.New (development default)
//	  unset                   (only when APP_ENV != "production")
//
// Production fail-fast: when APP_ENV=production, an unset
// EMAIL_PROVIDER is rejected so a misconfigured deploy never silently
// drops emails.
package factory

import (
	"errors"
	"fmt"

	"github.com/pericles-luz/crm/internal/adapter/notify/email/mailgun"
	"github.com/pericles-luz/crm/internal/adapter/notify/email/noop"
	"github.com/pericles-luz/crm/internal/adapter/notify/email/recorder"
	"github.com/pericles-luz/crm/internal/notify/email"
)

// Env is the abstract view of the environment the factory needs.
// Implemented in production by an os.Getenv-backed adapter and in
// tests by a map-backed fake — boundary functions stay testable
// without the test process polluting its own real environment.
type Env interface {
	Get(key string) string
}

// MapEnv is a trivial Env implementation backed by a map. Useful for
// tests and for callers that want to override specific keys without
// mutating os environment.
type MapEnv map[string]string

// Get implements Env.
func (m MapEnv) Get(k string) string { return m[k] }

// ErrMissingEnv signals a required env var was empty in a context
// where it was mandatory (e.g. mailgun in production). Boot wiring
// re-wraps it with the offending key so the operator sees which
// variable to set.
var ErrMissingEnv = errors.New("email factory: missing environment variable")

// ErrUnknownProvider signals an unrecognised EMAIL_PROVIDER value.
// The set of valid values is closed; new providers must be added to
// the switch in New.
var ErrUnknownProvider = errors.New("email factory: unknown EMAIL_PROVIDER")

// New returns the EmailSender selected by the environment. Boot code
// must check the returned error and refuse to start when present.
//
// The returned sender is concrete-typed only for the test and
// recorder branches; production code should bind the result to an
// email.EmailSender so the adapter remains swappable.
func New(env Env) (email.EmailSender, error) {
	provider := env.Get("EMAIL_PROVIDER")
	appEnv := env.Get("APP_ENV")

	if provider == "" {
		if appEnv == "production" {
			return nil, fmt.Errorf("%w: EMAIL_PROVIDER", ErrMissingEnv)
		}
		return noop.New(), nil
	}

	switch provider {
	case "mailgun":
		return newMailgun(env)
	case "noop":
		return noop.New(), nil
	case "recorder":
		return recorder.New(), nil
	default:
		return nil, fmt.Errorf("%w: %q", ErrUnknownProvider, provider)
	}
}

func newMailgun(env Env) (email.EmailSender, error) {
	apiKey := env.Get("MAILGUN_API_KEY")
	domain := env.Get("MAILGUN_DOMAIN")
	region := env.Get("MAILGUN_REGION")
	if apiKey == "" {
		return nil, fmt.Errorf("%w: MAILGUN_API_KEY", ErrMissingEnv)
	}
	if domain == "" {
		return nil, fmt.Errorf("%w: MAILGUN_DOMAIN", ErrMissingEnv)
	}
	if region == "" {
		return nil, fmt.Errorf("%w: MAILGUN_REGION", ErrMissingEnv)
	}
	cfg := mailgun.Config{
		APIKey: apiKey,
		Domain: domain,
		Region: mailgun.Region(region),
	}
	s, err := mailgun.New(cfg)
	if err != nil {
		return nil, err
	}
	return s, nil
}
