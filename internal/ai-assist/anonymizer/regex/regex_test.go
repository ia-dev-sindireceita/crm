package regex_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pericles-luz/crm/internal/ai-assist/anonymizer"
	"github.com/pericles-luz/crm/internal/ai-assist/anonymizer/regex"
)

// must wraps Anonymize, failing the test on a non-nil error. It
// keeps the table-driven loops below readable. Go's multi-return
// spread doesn't apply when the call has extra args, so the helper
// runs Anonymize itself rather than accepting (got, err).
func must(t *testing.T, a anonymizer.Anonymizer, in string) string {
	t.Helper()
	got, err := a.Anonymize(context.Background(), in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	return got
}

func TestAnonymizerVersion(t *testing.T) {
	if regex.AnonymizerVersion != "v1" {
		t.Fatalf("AnonymizerVersion = %q, want %q", regex.AnonymizerVersion, "v1")
	}
}

func TestAnonymize_EmptyInput(t *testing.T) {
	a := regex.New()
	got, err := a.Anonymize(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Fatalf("Anonymize(\"\") = %q, want empty string", got)
	}
}

func TestAnonymize_Phones(t *testing.T) {
	a := regex.New()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"mobile parens space hyphen", "(11) 98765-4321", "[PII:PHONE]"},
		{"mobile space hyphen", "11 98765-4321", "[PII:PHONE]"},
		{"mobile parens no space", "(11)98765-4321", "[PII:PHONE]"},
		{"mobile plus 55 compact", "+5511987654321", "[PII:PHONE]"},
		{"mobile plus 55 spaced", "+55 11 98765-4321", "[PII:PHONE]"},
		{"mobile 55 no plus compact", "5511987654321", "[PII:PHONE]"},
		{"fixed parens space hyphen", "(13) 9876-5432", "[PII:PHONE]"},
		{"fixed parens 14 ddd", "(14) 9876-5432", "[PII:PHONE]"},
		{"fixed dd hyphen", "11-3456-7890", "[PII:PHONE]"},
		{"fixed plus 55 spaced", "+55 11 3456-7890", "[PII:PHONE]"},
		{"embedded in sentence", "Ligue para (11) 98765-4321 hoje.", "Ligue para [PII:PHONE] hoje."},
		{"two phones in row", "(11) 98765-4321 e (12) 3456-7890", "[PII:PHONE] e [PII:PHONE]"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := must(t, a, tc.in); got != tc.want {
				t.Fatalf("Anonymize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAnonymize_Emails(t *testing.T) {
	a := regex.New()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"simple", "foo@bar.com", "[PII:EMAIL]"},
		{"subdomain", "a@b.c.d", "[PII:EMAIL]"},
		{"plus alias", "foo+bar@a.b.c", "[PII:EMAIL]"},
		{"with dot in local", "first.last@example.org", "[PII:EMAIL]"},
		{"with underscore", "first_last@example.io", "[PII:EMAIL]"},
		{"embedded", "Contato: alice@example.com, qualquer hora.", "Contato: [PII:EMAIL], qualquer hora."},
		{"two emails", "a@b.com e c@d.org", "[PII:EMAIL] e [PII:EMAIL]"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := must(t, a, tc.in); got != tc.want {
				t.Fatalf("Anonymize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAnonymize_CPF_ChecksumOff(t *testing.T) {
	a := regex.New() // default: checksum off
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"masked valid", "123.456.789-09", "[PII:CPF]"},
		{"masked invalid (checksum off matches anyway)", "111.222.333-44", "[PII:CPF]"},
		{"raw 11 digits", "12345678909", "[PII:CPF]"},
		{"raw 11 digits invalid (checksum off matches anyway)", "11122233344", "[PII:CPF]"},
		{"embedded", "CPF do cliente: 123.456.789-09.", "CPF do cliente: [PII:CPF]."},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := must(t, a, tc.in); got != tc.want {
				t.Fatalf("Anonymize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAnonymize_CPF_ChecksumOn(t *testing.T) {
	a := regex.New(regex.WithCPFChecksum(true))
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"masked valid", "123.456.789-09", "[PII:CPF]"},
		{"masked invalid stays intact", "111.222.333-44", "111.222.333-44"},
		{"raw valid", "12345678909", "[PII:CPF]"},
		{"raw invalid stays intact", "11122233344", "11122233344"},
		// Second well-known valid CPF (digits 529.982.247-25 are
		// the Receita Federal "Joao da Silva" test value).
		{"masked valid alt", "529.982.247-25", "[PII:CPF]"},
		{"raw valid alt", "52998224725", "[PII:CPF]"},
		// Invalid first-digit failure path (d1 mismatch only).
		{"raw first digit wrong", "12345678919", "12345678919"},
		// Exercises the d1==10 → 0 branch of validCPF: digits chosen
		// so the weighted sum yields 10 mod 11, which the algorithm
		// must clamp to a literal 0 check digit.
		{"raw d1 mod-11 clamps 10 to 0", "10000000108", "[PII:CPF]"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := must(t, a, tc.in)
			if got != tc.want {
				t.Fatalf("Anonymize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestAnonymize_CPF_ChecksumD2ClampsTenToZero exercises the
// d2 == 10 → 0 branch of validCPF. The CPF 987.654.321-00 is
// constructed so the weighted check-digit sum yields 10, which the
// algorithm must clamp to the literal wire digit 0.
func TestAnonymize_CPF_ChecksumD2ClampsTenToZero(t *testing.T) {
	a := regex.New(regex.WithCPFChecksum(true))
	got, err := a.Anonymize(context.Background(), "987.654.321-00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != anonymizer.TokenCPF {
		t.Fatalf("expected token, got %q", got)
	}
}

func TestAnonymize_CNPJ(t *testing.T) {
	a := regex.New()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"masked", "12.345.678/0001-95", "[PII:CNPJ]"},
		{"raw 14 digits", "12345678000195", "[PII:CNPJ]"},
		{"embedded masked", "Fornecedor 12.345.678/0001-95 ativo.", "Fornecedor [PII:CNPJ] ativo."},
		{"embedded raw", "Fornecedor 12345678000195 ativo.", "Fornecedor [PII:CNPJ] ativo."},
		{"two cnpjs", "12.345.678/0001-95 e 98.765.432/0001-12", "[PII:CNPJ] e [PII:CNPJ]"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := must(t, a, tc.in); got != tc.want {
				t.Fatalf("Anonymize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAnonymize_FalsePositives(t *testing.T) {
	a := regex.New()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"bare 8 digits not a phone", "12345678", "12345678"},
		{"bare 9 digits not a phone", "123456789", "123456789"},
		{"bare 10 digits not a phone", "1234567890", "1234567890"},
		{"bare 12 digits not pii", "123456789012", "123456789012"},
		{"short number in text", "Comprei 5 unidades por R$ 200,00.", "Comprei 5 unidades por R$ 200,00."},
		{"date with hyphens not phone", "2024-01-01", "2024-01-01"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := must(t, a, tc.in); got != tc.want {
				t.Fatalf("Anonymize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestAnonymize_MixedPII(t *testing.T) {
	a := regex.New()
	in := "Cliente Ana, CPF 123.456.789-09, email ana+marketing@empresa.com.br, telefone (11) 98765-4321, CNPJ 12.345.678/0001-95."
	want := "Cliente Ana, CPF [PII:CPF], email [PII:EMAIL], telefone [PII:PHONE], CNPJ [PII:CNPJ]."
	got := must(t, a, in)
	if got != want {
		t.Fatalf("got:\n  %q\nwant:\n  %q", got, want)
	}
	if strings.Contains(got, "Ana") == false {
		t.Fatalf("proper name 'Ana' must NOT be redacted, got %q", got)
	}
}

func TestAnonymize_Idempotent(t *testing.T) {
	a := regex.New()
	inputs := []string{
		"",
		"foo@bar.com",
		"(11) 98765-4321",
		"123.456.789-09",
		"12345678909",
		"12.345.678/0001-95",
		"Misto: 123.456.789-09 / a@b.c / (11) 98765-4321 / 12345678000195",
	}
	for _, in := range inputs {
		first := must(t, a, in)
		second := must(t, a, first)
		if first != second {
			t.Fatalf("not idempotent for input %q: first=%q second=%q", in, first, second)
		}
	}
}

func TestAnonymize_IdempotentWithChecksumOn(t *testing.T) {
	a := regex.New(regex.WithCPFChecksum(true))
	in := "CPF 123.456.789-09 e raw 52998224725 e fake 111.222.333-44"
	first := must(t, a, in)
	second := must(t, a, first)
	if first != second {
		t.Fatalf("not idempotent (checksum on): first=%q second=%q", first, second)
	}
	// Invalid CPF MUST survive both passes intact.
	if !strings.Contains(first, "111.222.333-44") {
		t.Fatalf("invalid CPF must survive first pass: %q", first)
	}
}

func TestAnonymize_ContextCancelled(t *testing.T) {
	a := regex.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := a.Anonymize(ctx, "foo@bar.com")
	if err == nil {
		t.Fatalf("expected error on cancelled context, got nil; out=%q", got)
	}
	if !errors.Is(err, anonymizer.ErrAnonymize) {
		t.Fatalf("error must wrap ErrAnonymize, got %v", err)
	}
	if got != "" {
		t.Fatalf("on error, output must be empty, got %q", got)
	}
}

func TestAnonymize_NoLeakOfRegexImpl(t *testing.T) {
	// Smoke: the port must be usable through the interface without
	// importing the regex sub-package directly into the call site.
	var iface anonymizer.Anonymizer = regex.New()
	out, err := iface.Anonymize(context.Background(), "ping foo@bar.com")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(out, anonymizer.TokenEmail) {
		t.Fatalf("expected token in %q", out)
	}
}

func TestAnonymize_OnlyDigitsHelperViaCPFFormatVariants(t *testing.T) {
	// validCPF strips non-digits internally; this exercises both
	// the masked and the raw code paths against the same logical
	// number to ensure they converge.
	a := regex.New(regex.WithCPFChecksum(true))
	masked := must(t, a, "529.982.247-25")
	raw := must(t, a, "52998224725")
	if masked != anonymizer.TokenCPF || raw != anonymizer.TokenCPF {
		t.Fatalf("masked=%q raw=%q", masked, raw)
	}
}

func TestAnonymize_LongTextLinearTime(t *testing.T) {
	// Defence in depth against accidental ReDoS regressions: a
	// large but non-pathological input must complete quickly. We
	// don't measure wall-clock here (CI variance) — we just assert
	// the call returns deterministically without panicking. If a
	// future contributor introduces a nested quantifier, this test
	// will simply hang and time out under `go test`.
	a := regex.New()
	in := strings.Repeat("texto sem pii e mais texto ", 5000) + " foo@bar.com"
	got := must(t, a, in)
	if !strings.Contains(got, anonymizer.TokenEmail) {
		t.Fatalf("token missing from long input")
	}
}

func TestTokenConstants(t *testing.T) {
	// Lock the public constants. If a future change renames a
	// token, every downstream prompt template breaks — make that
	// explicit at the test boundary.
	cases := map[string]string{
		"phone": anonymizer.TokenPhone,
		"email": anonymizer.TokenEmail,
		"cpf":   anonymizer.TokenCPF,
		"cnpj":  anonymizer.TokenCNPJ,
	}
	want := map[string]string{
		"phone": "[PII:PHONE]",
		"email": "[PII:EMAIL]",
		"cpf":   "[PII:CPF]",
		"cnpj":  "[PII:CNPJ]",
	}
	for k, v := range cases {
		if v != want[k] {
			t.Fatalf("Token %s = %q, want %q", k, v, want[k])
		}
	}
}

func TestErrAnonymizeIsExported(t *testing.T) {
	if anonymizer.ErrAnonymize == nil {
		t.Fatal("ErrAnonymize must be a non-nil sentinel")
	}
	if !strings.Contains(anonymizer.ErrAnonymize.Error(), "anonymizer") {
		t.Fatalf("ErrAnonymize message should mention package, got %q", anonymizer.ErrAnonymize.Error())
	}
}
