package branding_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	memstoreadapter "github.com/pericles-luz/crm/internal/adapter/branding/memstore"
	"github.com/pericles-luz/crm/internal/branding"
	"github.com/pericles-luz/crm/internal/tenancy"
	webbranding "github.com/pericles-luz/crm/internal/web/branding"
)

// fakeExtractor is the in-test stand-in for the SIN-63079 mediancut
// adapter. Tests configure either the palette returned on success or
// the sentinel-wrapped error to drive a particular branch.
type fakeExtractor struct {
	pal branding.Palette
	err error
}

func (f *fakeExtractor) Extract(_ context.Context, _ io.Reader, _ branding.Hint) (branding.Palette, error) {
	if f.err != nil {
		return branding.Palette{}, f.err
	}
	return f.pal, nil
}

// recordingInvalidator captures Invalidate calls so tests can assert
// the save / revert flows poke the runtime theme cache.
type recordingInvalidator struct {
	mu    sync.Mutex
	calls []uuid.UUID
}

func (r *recordingInvalidator) Invalidate(id uuid.UUID) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, id)
	return true
}

func (r *recordingInvalidator) Calls() []uuid.UUID {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]uuid.UUID, len(r.calls))
	copy(out, r.calls)
	return out
}

func samplePalette() branding.Palette {
	pal := branding.Palette{
		Primary:    branding.RGB{R: 0x1f, G: 0x29, B: 0x37},
		Secondary:  branding.RGB{R: 0x37, G: 0x41, B: 0x51},
		Accent:     branding.RGB{R: 0x2d, G: 0x9c, B: 0xdb},
		Foreground: branding.RGB{R: 0x0f, G: 0x11, B: 0x15},
		Background: branding.RGB{R: 0xff, G: 0xff, B: 0xff},
		Source:     branding.PaletteSourceExtracted,
	}
	pal, _ = branding.EnsureWCAGAA(pal)
	return pal
}

type testRig struct {
	handler *webbranding.Handler
	tenant  *tenancy.Tenant
	store   *memstoreadapter.Store
	cache   *recordingInvalidator
	extr    *fakeExtractor
}

func newTestRig(t *testing.T) *testRig {
	t.Helper()
	return newTestRigWithExtractor(t, &fakeExtractor{pal: samplePalette()})
}

func newTestRigWithExtractor(t *testing.T, ex *fakeExtractor) *testRig {
	t.Helper()
	store := memstoreadapter.New()
	cache := &recordingInvalidator{}
	h, err := webbranding.New(webbranding.Deps{
		Extractor:  ex,
		Store:      store,
		Writer:     store,
		CSRFToken:  func(_ *http.Request) string { return "csrf-tok" },
		ThemeCache: cache,
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testRig{
		handler: h,
		tenant:  &tenancy.Tenant{ID: uuid.New(), Name: "acme", Host: "acme.crm.local"},
		store:   store,
		cache:   cache,
		extr:    ex,
	}
}

func (rig *testRig) mux(t *testing.T) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	rig.handler.Routes(mux)
	return mux
}

// request returns a request scoped to the rig's tenant so the handler's
// tenancy.FromContext call resolves the expected *Tenant.
func (rig *testRig) request(method, target string, body io.Reader) *http.Request {
	r := httptest.NewRequest(method, target, body)
	r = r.WithContext(tenancy.WithContext(r.Context(), rig.tenant))
	return r
}

func TestNew_RequiresPorts(t *testing.T) {
	t.Parallel()
	store := memstoreadapter.New()
	full := webbranding.Deps{
		Extractor: &fakeExtractor{},
		Store:     store,
		Writer:    store,
		CSRFToken: func(_ *http.Request) string { return "t" },
	}
	cases := []struct {
		name string
		mut  func(d *webbranding.Deps)
	}{
		{"nil extractor", func(d *webbranding.Deps) { d.Extractor = nil }},
		{"nil store", func(d *webbranding.Deps) { d.Store = nil }},
		{"nil writer", func(d *webbranding.Deps) { d.Writer = nil }},
		{"nil csrf", func(d *webbranding.Deps) { d.CSRFToken = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := full
			tc.mut(&d)
			if _, err := webbranding.New(d); err == nil {
				t.Fatalf("expected error, got nil")
			}
		})
	}
}

func TestPage_RendersWithDefaultPalette_WhenStoreEmpty(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, rig.request(http.MethodGet, "/branding", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<meta name="csrf-token" content="csrf-tok">`,
		`hx-headers='{"X-CSRF-Token": "csrf-tok"}'`,
		`name="logo"`,
		`name="primary"`,
		`hx-post="/branding/logo"`,
		`Padrão neutro`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\n%s", want, body)
		}
	}
}

func TestPage_UsesStoredPalette(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	pal := samplePalette()
	pal.Source = branding.PaletteSourceManual
	if err := rig.store.SetForTenant(context.Background(), rig.tenant.ID, pal); err != nil {
		t.Fatalf("SetForTenant: %v", err)
	}
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, rig.request(http.MethodGet, "/branding", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `value="`+pal.Primary.Hex()+`"`) {
		t.Fatalf("expected primary swatch %s in body", pal.Primary.Hex())
	}
	if !strings.Contains(rec.Body.String(), `Override manual`) {
		t.Fatalf("expected manual source label in body")
	}
}

func TestPage_NoTenant_Returns500(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branding", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestPage_EmptyCSRFToken_Returns500(t *testing.T) {
	t.Parallel()
	store := memstoreadapter.New()
	h, err := webbranding.New(webbranding.Deps{
		Extractor: &fakeExtractor{pal: samplePalette()},
		Store:     store,
		Writer:    store,
		CSRFToken: func(_ *http.Request) string { return "" },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	r := httptest.NewRequest(http.MethodGet, "/branding", nil)
	r = r.WithContext(tenancy.WithContext(r.Context(), &tenancy.Tenant{ID: uuid.New(), Name: "acme"}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

// buildMultipart returns a multipart body + content-type for a single
// "logo" file with the supplied payload and declared content type.
func buildMultipart(t *testing.T, declaredCT string, payload []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	hdr := make(textproto.MIMEHeader)
	hdr.Set("Content-Disposition", `form-data; name="logo"; filename="logo.png"`)
	hdr.Set("Content-Type", declaredCT)
	part, err := mw.CreatePart(hdr)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("part.Write: %v", err)
	}
	if err := mw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return body, mw.FormDataContentType()
}

// pngBytes is a minimal valid 1×1 PNG. It is enough for
// http.DetectContentType to identify "image/png"; the fake extractor
// never decodes the payload.
var pngBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
	0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
	0x42, 0x60, 0x82,
}

func TestUpload_ValidPNG_ReturnsPalettePreview(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	body, ct := buildMultipart(t, "image/png", pngBytes)
	r := rig.request(http.MethodPost, "/branding/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `id="branding-preview"`) {
		t.Fatalf("expected preview fragment, got\n%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), rig.extr.pal.Primary.Hex()) {
		t.Fatalf("expected extracted primary %s in body", rig.extr.pal.Primary.Hex())
	}
}

func TestUpload_ExceedsMaxSize_Returns413(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	// Build a payload bigger than MaxLogoBytes by padding the valid PNG.
	huge := bytes.Repeat([]byte{0x00}, webbranding.MaxLogoBytes+1024)
	body, ct := buildMultipart(t, "image/png", append(append([]byte{}, pngBytes...), huge...))
	r := rig.request(http.MethodPost, "/branding/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d, want 413; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpload_DeclaredContentTypeBlocked_Returns415(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	body, ct := buildMultipart(t, "image/svg+xml", []byte("<svg></svg>"))
	r := rig.request(http.MethodPost, "/branding/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d, want 415", rec.Code)
	}
}

func TestUpload_SniffedTypeMismatchesDeclared_Returns415(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	body, ct := buildMultipart(t, "image/png", []byte("not an image at all"))
	r := rig.request(http.MethodPost, "/branding/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d, want 415", rec.Code)
	}
}

func TestUpload_MissingFile_Returns400(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	body := &bytes.Buffer{}
	mw := multipart.NewWriter(body)
	_ = mw.WriteField("other", "value")
	_ = mw.Close()
	r := rig.request(http.MethodPost, "/branding/logo", body)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestUpload_ExtractorReportsInvalidImage_Returns415(t *testing.T) {
	t.Parallel()
	rig := newTestRigWithExtractor(t, &fakeExtractor{err: branding.ErrInvalidImage})
	body, ct := buildMultipart(t, "image/png", pngBytes)
	r := rig.request(http.MethodPost, "/branding/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("status=%d, want 415", rec.Code)
	}
}

func TestUpload_ExtractorTransientError_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	rig := newTestRigWithExtractor(t, &fakeExtractor{err: branding.ErrUnavailable})
	body, ct := buildMultipart(t, "image/png", pngBytes)
	r := rig.request(http.MethodPost, "/branding/logo", body)
	r.Header.Set("Content-Type", ct)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), branding.DefaultPalette.Primary.Hex()) {
		t.Fatalf("expected default primary %s in body", branding.DefaultPalette.Primary.Hex())
	}
}

// formForPalette returns a url.Values populated with each slot's hex.
// Used by the override/save tests.
func formForPalette(p branding.Palette) url.Values {
	v := url.Values{}
	v.Set("primary", p.Primary.Hex())
	v.Set("secondary", p.Secondary.Hex())
	v.Set("accent", p.Accent.Hex())
	v.Set("foreground", p.Foreground.Hex())
	v.Set("background", p.Background.Hex())
	v.Set("text_on_primary", p.TextOnPrimary.Hex())
	return v
}

func TestOverride_ValidHex_UpdatesSlot(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	pal := samplePalette()
	values := formForPalette(pal)
	values.Set("slot", "accent")
	values.Set("accent", "#2d9cdb")
	r := rig.request(http.MethodPost, "/branding/palette/override",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `value="#2d9cdb"`) {
		t.Fatalf("expected #2d9cdb in body\n%s", rec.Body.String())
	}
}

func TestOverride_InvalidHex_Returns422(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	values := formForPalette(samplePalette())
	values.Set("slot", "primary")
	values.Set("primary", "notahex")
	r := rig.request(http.MethodPost, "/branding/palette/override",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "primária") {
		t.Fatalf("expected slot-specific error in body\n%s", rec.Body.String())
	}
}

func TestOverride_InvalidSlot_Returns400(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	values := formForPalette(samplePalette())
	values.Set("slot", "neon")
	r := rig.request(http.MethodPost, "/branding/palette/override",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", rec.Code)
	}
}

func TestOverride_FailsWCAG_Returns422(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	// Sample a low-contrast pair: light grey text on white background.
	values := formForPalette(samplePalette())
	values.Set("slot", "foreground")
	values.Set("foreground", "#eeeeee")
	values.Set("background", "#ffffff")
	r := rig.request(http.MethodPost, "/branding/palette/override",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Contraste insuficiente") {
		t.Fatalf("expected contrast error message; body=%s", rec.Body.String())
	}
}

func TestSave_PersistsAndEmitsOOBSwap(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	pal := samplePalette()
	values := formForPalette(pal)
	r := rig.request(http.MethodPost, "/branding/palette/save",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	got, err := rig.store.GetByTenantID(context.Background(), rig.tenant.ID)
	if err != nil {
		t.Fatalf("expected stored palette: %v", err)
	}
	if got.Primary != pal.Primary {
		t.Fatalf("stored primary=%v, want %v", got.Primary, pal.Primary)
	}
	if got.Source != branding.PaletteSourceManual {
		t.Fatalf("stored source=%v, want Manual", got.Source)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="tenant-theme"`) {
		t.Fatalf("expected OOB <style id=\"tenant-theme\"> in body\n%s", body)
	}
	if !strings.Contains(body, `hx-swap-oob="outerHTML"`) {
		t.Fatalf("expected hx-swap-oob attribute in body\n%s", body)
	}
	if len(rig.cache.Calls()) == 0 {
		t.Fatalf("expected theme cache invalidation; got 0 calls")
	}
}

func TestSave_InvalidHexInForm_Returns422(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	values := formForPalette(samplePalette())
	values.Set("primary", "not-a-hex")
	r := rig.request(http.MethodPost, "/branding/palette/save",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

func TestRevert_DeletesPersistedAndReturnsDefault(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	_ = rig.store.SetForTenant(context.Background(), rig.tenant.ID, samplePalette())
	r := rig.request(http.MethodPost, "/branding/palette/revert", nil)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
	_, err := rig.store.GetByTenantID(context.Background(), rig.tenant.ID)
	if !errors.Is(err, branding.ErrPaletteNotFound) {
		t.Fatalf("after revert err=%v, want ErrPaletteNotFound", err)
	}
	if !strings.Contains(rec.Body.String(), branding.DefaultPalette.Primary.Hex()) {
		t.Fatalf("expected default primary %s in body", branding.DefaultPalette.Primary.Hex())
	}
	if len(rig.cache.Calls()) == 0 {
		t.Fatalf("expected cache invalidation after revert")
	}
}

// failingStore is a PaletteStore whose Get/Set/Delete return a fixed
// non-not-found error. Used by the save / revert failure-path tests.
type failingStore struct {
	getErr    error
	setErr    error
	deleteErr error
}

func (f *failingStore) GetByTenantID(_ context.Context, _ uuid.UUID) (branding.Palette, error) {
	if f.getErr != nil {
		return branding.Palette{}, f.getErr
	}
	return branding.Palette{}, branding.ErrPaletteNotFound
}

func (f *failingStore) SetForTenant(_ context.Context, _ uuid.UUID, _ branding.Palette) error {
	return f.setErr
}

func (f *failingStore) DeleteForTenant(_ context.Context, _ uuid.UUID) error {
	return f.deleteErr
}

func newFailingRig(t *testing.T, fs *failingStore) *testRig {
	t.Helper()
	h, err := webbranding.New(webbranding.Deps{
		Extractor: &fakeExtractor{pal: samplePalette()},
		Store:     fs,
		Writer:    fs,
		CSRFToken: func(_ *http.Request) string { return "csrf-tok" },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &testRig{
		handler: h,
		tenant:  &tenancy.Tenant{ID: uuid.New(), Name: "acme"},
	}
}

func TestSave_WriterError_Returns500(t *testing.T) {
	t.Parallel()
	rig := newFailingRig(t, &failingStore{setErr: errors.New("boom")})
	values := formForPalette(samplePalette())
	r := rig.request(http.MethodPost, "/branding/palette/save",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestRevert_WriterError_Returns500(t *testing.T) {
	t.Parallel()
	rig := newFailingRig(t, &failingStore{deleteErr: errors.New("boom")})
	r := rig.request(http.MethodPost, "/branding/palette/revert", nil)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want 500", rec.Code)
	}
}

func TestPage_StoreTransientError_FallsBackToDefault(t *testing.T) {
	t.Parallel()
	rig := newFailingRig(t, &failingStore{getErr: errors.New("transient")})
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, rig.request(http.MethodGet, "/branding", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200 fallback", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), branding.DefaultPalette.Primary.Hex()) {
		t.Fatalf("expected default primary in body")
	}
}

func TestSave_WithoutThemeCache_StillSucceeds(t *testing.T) {
	t.Parallel()
	store := memstoreadapter.New()
	h, err := webbranding.New(webbranding.Deps{
		Extractor: &fakeExtractor{pal: samplePalette()},
		Store:     store,
		Writer:    store,
		CSRFToken: func(_ *http.Request) string { return "csrf-tok" },
		Logger:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	h.Routes(mux)
	tenant := &tenancy.Tenant{ID: uuid.New(), Name: "acme"}
	values := formForPalette(samplePalette())
	r := httptest.NewRequest(http.MethodPost, "/branding/palette/save",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	r = r.WithContext(tenancy.WithContext(r.Context(), tenant))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, r)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d", rec.Code)
	}
}

func TestOverride_MalformedForm_Returns422(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	values := url.Values{}
	values.Set("slot", "primary")
	values.Set("primary", "#abcdef")
	// missing other slots
	r := rig.request(http.MethodPost, "/branding/palette/override",
		strings.NewReader(values.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, r)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status=%d, want 422", rec.Code)
	}
}

// end-to-end test asserting the full upload → save → page flow
// reads back the persisted palette on the next render.
func TestFullFlow_UploadSaveRender(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	mux := rig.mux(t)

	// 1. Upload returns extracted palette.
	body, ct := buildMultipart(t, "image/png", pngBytes)
	r1 := rig.request(http.MethodPost, "/branding/logo", body)
	r1.Header.Set("Content-Type", ct)
	rec1 := httptest.NewRecorder()
	mux.ServeHTTP(rec1, r1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("upload status=%d", rec1.Code)
	}

	// 2. Save persists.
	values := formForPalette(rig.extr.pal)
	r2 := rig.request(http.MethodPost, "/branding/palette/save",
		strings.NewReader(values.Encode()))
	r2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, r2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", rec2.Code, rec2.Body.String())
	}

	// 3. GET /branding reads the persisted palette.
	r3 := rig.request(http.MethodGet, "/branding", nil)
	rec3 := httptest.NewRecorder()
	mux.ServeHTTP(rec3, r3)
	if rec3.Code != http.StatusOK {
		t.Fatalf("page status=%d", rec3.Code)
	}
	if !strings.Contains(rec3.Body.String(), `value="`+rig.extr.pal.Primary.Hex()+`"`) {
		t.Fatalf("page did not render persisted primary %s", rig.extr.pal.Primary.Hex())
	}
}

// TestPage_HeadWiresPithoStylesheets pins SIN-65112: the /branding page
// <head> must link the design-system sheets so the page is no longer
// rendered with user-agent defaults, and must keep the load-bearing
// per-tenant theme injection. The canonical order (per app-shell
// layout.html and the C4–C6 Pitho pages) is: inline tenant-theme <style>
// FIRST, then tokens.css → components.css → branding.css.
func TestPage_HeadWiresPithoStylesheets(t *testing.T) {
	t.Parallel()
	rig := newTestRig(t)
	rec := httptest.NewRecorder()
	rig.mux(t).ServeHTTP(rec, rig.request(http.MethodGet, "/branding", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	for _, want := range []string{
		`<style id="tenant-theme" nonce="">`,
		`<link rel="stylesheet" href="/static/css/tokens.css">`,
		`<link rel="stylesheet" href="/static/css/components.css">`,
		`<link rel="stylesheet" href="/static/css/branding.css">`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("page head missing %q\n%s", want, body)
		}
	}

	// The tenant-theme block carries the current palette's :root{…} so the
	// preview reflects the tenant colours over the Pitho defaults, and the
	// save/revert OOB swap has a live target to replace.
	if !strings.Contains(body, ":root{--color-primary:") {
		t.Fatalf("tenant-theme <style> missing :root palette declaration\n%s", body)
	}

	// Cascade order: tenant-theme <style> precedes the token links, which
	// precede the page sheet. A reorder would silently break the override.
	themeAt := strings.Index(body, `id="tenant-theme"`)
	tokensAt := strings.Index(body, "tokens.css")
	componentsAt := strings.Index(body, "components.css")
	brandingAt := strings.Index(body, "branding.css")
	if !(themeAt >= 0 && themeAt < tokensAt && tokensAt < componentsAt && componentsAt < brandingAt) {
		t.Fatalf("head order wrong: theme=%d tokens=%d components=%d branding=%d",
			themeAt, tokensAt, componentsAt, brandingAt)
	}
}
