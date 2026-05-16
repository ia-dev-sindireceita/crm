# `internal/ai-assist/anonymizer`

Pure-domain port + regex adapter for redacting Brazilian PII out of
free-form text **before** it is forwarded to an external LLM
(OpenRouter today, anything tomorrow).

Anonymization is a fixed product rule from
[ADR-0041](../../../docs/adr/0041-anonymizer-pii.md) (ratified by the
board in SIN-62203, decisão #8 — Fase 3): every payload that crosses
the bounded-context boundary into the LLM client MUST first pass
through an `Anonymizer`. The choice is opt-OUT only at the product
level — tenants cannot disable it via the `aipolicy` cascade.

## What gets masked

| PII class           | Token         | Examples                                        |
| ------------------- | ------------- | ----------------------------------------------- |
| Brazilian phone     | `[PII:PHONE]` | `(11) 98765-4321`, `+5511987654321`, `11 3456-7890` |
| Email address       | `[PII:EMAIL]` | `foo+bar@a.b.c`, `alice@example.com.br`         |
| CPF                 | `[PII:CPF]`   | `123.456.789-09`, `12345678909`                 |
| CNPJ                | `[PII:CNPJ]`  | `12.345.678/0001-95`, `12345678000195`          |

Tokens are part of the public contract. Downstream prompt templates
and prompt-evaluation suites may pin against them. Changing them is
a deliberate breaking change that MUST coincide with bumping
`regex.AnonymizerVersion` (currently `"v1"`).

## What is NOT masked

- **Proper names** (e.g. "Ana", "João"). Operators need the name to
  hold a useful conversation; any best-effort name detector is more
  likely to over-redact than to protect anything LGPD already covers
  via the identifier classes above. This is a conscious product
  decision recorded in ADR-0041.
- **Free-form numbers below the PII thresholds** (e.g. "8 dígitos
  avulsos não viram telefone"). The phone matcher requires a
  Brazilian-shape signal (DDI `+55`, parenthesised DDD, or an
  explicit separator between DDD and number); a bare 8-digit run is
  left untouched.

## Hexagonal layout

- `anonymizer.go` — port. Pure domain: imports only `context` and
  `errors`. Declares the `Anonymizer` interface, the public
  replacement tokens, and the `ErrAnonymize` sentinel.
- `regex/regex.go` — adapter. Pre-compiles a small set of
  **linear** regexes at package init (no nested quantifiers, no
  super-linear backtracking) and applies them in a fixed order:
  email → CNPJ masked → CPF masked → CNPJ raw → CPF raw → phone.

The OpenRouter chat-completions client takes an
`anonymizer.Anonymizer` by interface; only `cmd/server` wires the
concrete adapter. Tests are pure unit tests (no DB, no network, no
testcontainer).

## Usage

```go
import (
    "context"

    "github.com/pericles-luz/crm/internal/ai-assist/anonymizer"
    anonregex "github.com/pericles-luz/crm/internal/ai-assist/anonymizer/regex"
)

func example(ctx context.Context) error {
    var a anonymizer.Anonymizer = anonregex.New()
    out, err := a.Anonymize(ctx, "Ligue para (11) 98765-4321")
    if err != nil {
        // Fail-closed: ABORT the LLM call. Never forward cleartext.
        return err
    }
    // out == "Ligue para [PII:PHONE]"
    _ = out
    return nil
}
```

### Optional: CPF checksum gate

CPF validation by mod-11 checksum is **off by default**. In
conversational text every 11-digit number is almost certainly PII,
and the strict gate would let an invalid-but-clearly-numerical CPF
slip through. Enable it when the input is structured and the cost
of a false positive (redacting an opaque 11-digit identifier) is
high:

```go
a := anonregex.New(anonregex.WithCPFChecksum(true))
```

With the gate ON, a masked or raw CPF whose mod-11 check digits
disagree is left **intact** in the output.

## Fail-closed contract

Adapters MUST return `anonymizer.ErrAnonymize` (or an error that
wraps it) on any internal failure. The use-case site MUST abort
the LLM call and surface an audible error to the operator. There
is no cleartext fallback.

Concretely, the regex adapter:

- Returns `ErrAnonymize` if the supplied `context.Context` is
  already cancelled.
- Wraps any recovered panic (defensive — the implementation is
  built on `regexp` + pure string ops and should not panic) into
  `ErrAnonymize`.
- Logs neither input nor output. If you must trace a call, log
  only `len(input)` or a hash digest.

## Order-of-operations and idempotency

Patterns run in this order:

1. Email (anchored by `@`).
2. CNPJ masked (anchored by `/` — distinctive vs. CPF).
3. CPF masked.
4. CNPJ raw (14 consecutive digits).
5. CPF raw (11 consecutive digits).
6. Phone (DDI / parens / separator-anchored).

One consequence: a **bare** 11-digit string like `11987654321` is
tagged as `[PII:CPF]` rather than `[PII:PHONE]`. With the +55
prefix or any formatting (`(11) 98765-4321`, `11-98765-4321`,
`+5511987654321`), the phone matcher wins. Both outcomes redact the
PII; this is the deliberate trade-off when raw 11-digit numbers are
ambiguous.

Anonymization is **idempotent**: applying it twice produces the
same result. The replacement tokens contain `[`, `:`, `]`, and
letters only — none of the patterns match them.

## Versioning

`regex.AnonymizerVersion` is a SemVer-shaped string (`"v1"` today).
Bumping it is a breaking change:

- Any change to a token literal.
- Any change to the pattern set (new PII class, retired class,
  widened or narrowed matcher).
- Any change to the order of operations.

When you bump the version, re-baseline every prompt evaluation
suite that pins against tokens and update this README's "What gets
masked" table in the same commit.

## Adding a new PII class

1. Pick a stable token literal of the form `[PII:CATEGORY]`.
2. Add it to `anonymizer.go` next to the existing tokens (this is
   a public-contract change → bump the adapter version).
3. Add a **linear** regex constant in `regex/regex.go` and append a
   replacement step to `Anonymize` in the correct order (most
   specific patterns first).
4. Add table-driven tests in `regex/regex_test.go` for both the
   primary case and the obvious false-positive case.
5. Run `gofmt -l .` and `go test -coverprofile=cover.out
   ./internal/ai-assist/anonymizer/...` — the package-level
   coverage must remain above 95%.
6. Update the "What gets masked" table above.

## ReDoS safety

Every regex in the adapter uses bounded quantifiers (`\d{N}` or
`\d{4,5}`) — there is no nested quantifier and no alternation that
backtracks across the whole input. The patterns are compiled once
at package init via `regexp.MustCompile`, so a malformed pattern
fails immediately at startup rather than at first request. The test
suite includes a long-input smoke (`TestAnonymize_LongTextLinearTime`)
that hangs under `go test`'s default timeout if a future contributor
introduces super-linear backtracking.
