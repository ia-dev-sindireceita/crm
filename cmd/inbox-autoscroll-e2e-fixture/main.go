// Package main is the inbox auto-scroll E2E fixture binary (SIN-65455).
//
// This is NOT a production server. It exists solely so the Playwright
// browser-smoke suite under tests/e2e/ can drive the real
// web/static/js/inbox.js auto-scroll behaviour against the production
// inbox DOM contract (the #conversation-thread scroll container inside
// #inbox-conversation-pane) and the three htmx swap shapes that move the
// thread:
//
//  1. Conversation open / switch — a conversation row swaps the whole
//     right pane (#inbox-conversation-pane, innerHTML). inbox.js detects
//     the fresh view via evt.detail.target.id === "inbox-conversation-pane"
//     and lands the thread at the bottom.
//  2. Send — POST /inbox/conversations/{id}/messages appends an outbound
//     bubble (hx-swap=beforeend into #conversation-thread). inbox.js detects
//     the send because the swap target id === "conversation-thread" (it is
//     not a poll trigger), so it always pins.
//  3. Inbound — the live-thread poll (SIN-65419) advances its sentinel
//     (#thread-live-poll, outerHTML self-swap) and appends inbound bubbles
//     via an <ol hx-swap-oob="beforeend:#conversation-thread">. inbox.js
//     recognises the inbound moment by the trigger element id
//     (evt.detail.elt.id === "thread-live-poll") and pins to the bottom ONLY
//     when the operator was already at/near the bottom (captured on
//     htmx:beforeSwap), so an inbound reply does not yank the view away from
//     someone reading history.
//
// The fixture reproduces those exact swap shapes (ids, classes, swap
// attributes, request paths) so the browser probe exercises the real
// inbox.js against the real selectors. The seed thread overflows a
// fixed-height scroll viewport (fixture.css) so the scroll assertions are
// deterministic. The inbound moment is fired on demand by clicking the
// #thread-live-poll sentinel itself (its hx-trigger is "click" instead of
// the production "every 3s"), so the trigger element id stays
// "thread-live-poll" — matching production — without racing the 3s timer.
//
// SIN-62320 / SIN-63977: the fixture mounts the production CSP middleware
// (internal/http/middleware/csp) so every response carries the same
// Content-Security-Policy the production hosts emit (script-src 'self'
// 'nonce-{N}', no 'unsafe-inline'/'unsafe-eval'). inbox.js is an external
// nonce'd+defer'd file that hangs behaviour off htmx events — no inline
// JS, no hx-on:* — exactly so it survives this policy. Mounting the real
// CSP here means a regression to inline/eval would fail the browser probe
// instead of silently passing.
//
// Routes (all served on -addr, default 127.0.0.1:8089):
//
//	GET  /                The inbox shell host page: the conversation list
//	                      pane with a row that opens the conversation, an
//	                      (initially empty) #inbox-conversation-pane, and
//	                      the production htmx + inbox.js + inbox.css assets.
//	GET  /static/...      Pass-through to ./web/static/ (real inbox.js,
//	                      inbox.css, htmx bundle).
//	GET  /fixture.css     Fixture-only layout CSS that pins the thread to a
//	                      fixed-height scroll viewport so it overflows
//	                      deterministically. NOT a production stylesheet.
//	GET  /conversation    The conversation view fragment (innerHTML target
//	                      #inbox-conversation-pane): a #conversation-thread
//	                      seeded with enough bubbles to overflow, the
//	                      #thread-live-poll sentinel (which doubles as the
//	                      on-demand inbound trigger), and the compose form.
//	POST /inbox/conversations/{id}/messages
//	                      Returns a single outbound bubble (beforeend
//	                      append) — the send moment.
//	GET  /inbound         Returns the live-thread-update shape: a fresh
//	                      sentinel (outerHTML self-swap) followed by an
//	                      <ol hx-swap-oob="beforeend:#conversation-thread">
//	                      carrying one inbound bubble — the inbound moment.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"html/template"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pericles-luz/crm/internal/http/middleware/csp"
	vendorintegrity "github.com/pericles-luz/crm/internal/web/vendor"
	vendorassets "github.com/pericles-luz/crm/web/static/vendor"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Printf("inbox-autoscroll-e2e-fixture: %v", err)
		os.Exit(1)
	}
}

// run is the testable entry point. It parses flags, builds the mux, and
// runs the server until ctx is cancelled. Exposed so a unit test can
// drive a real bind/serve/shutdown cycle without exec'ing the binary.
func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("inbox-autoscroll-e2e-fixture", flag.ContinueOnError)
	addr := fs.String("addr", "127.0.0.1:8089", "listen address (loopback by default — fixture is not a public server)")
	staticDir := fs.String("static", "./web/static", "path to the web/static asset tree (htmx + inbox.css + inbox.js)")
	seed := fs.Int("seed-messages", 25, "number of seed bubbles in the conversation thread; enough to overflow the scroll viewport")
	if err := fs.Parse(args); err != nil {
		return err
	}

	handler := buildHandler(*staticDir, *seed)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	serveErr := make(chan error, 1)
	go func() {
		log.Printf("inbox-autoscroll-e2e-fixture: listening on http://%s (seed=%d)", *addr, *seed)
		err := srv.ListenAndServe()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case <-ctx.Done():
	case err := <-serveErr:
		return err
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return err
	}
	if err := <-serveErr; err != nil {
		return err
	}
	return nil
}

// buildHandler wraps the fixture mux with the production CSP middleware so
// every response carries the same Content-Security-Policy the prod hosts
// emit. Mirrors `csp.Middleware(mux)` in cmd/server/main.go.
func buildHandler(staticDir string, seed int) http.Handler {
	return csp.Middleware(buildMux(staticDir, seed))
}

func buildMux(staticDir string, seed int) *http.ServeMux {
	mux := http.NewServeMux()

	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.Dir(staticDir))))

	mux.HandleFunc("GET /fixture.css", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(fixtureCSS))
	})

	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_ = hostPageTmpl.Execute(w, hostPageData{Nonce: csp.Nonce(r.Context())})
	})

	mux.HandleFunc("GET /conversation", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = conversationTmpl.Execute(w, conversationData{Bubbles: seedBubbles(seed)})
	})

	// The send moment: POST .../messages appends one outbound bubble. The
	// path MUST end in /messages so inbox.js's isSend (verb=post + path
	// /messages$) recognises it and always pins to the bottom.
	mux.HandleFunc("POST /inbox/conversations/{id}/messages", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		body := r.FormValue("body")
		if body == "" {
			body = "mensagem enviada"
		}
		_ = bubbleTmpl.Execute(w, bubble{ID: nextID(), Direction: "out", Body: body})
	})

	// The inbound moment: the live-thread-update shape (SIN-65419). A
	// fresh sentinel (outerHTML self-swap onto #thread-live-poll) followed
	// by an <ol hx-swap-oob="beforeend:#conversation-thread"> that appends
	// the inbound bubble to the END of the thread regardless of where the
	// sentinel sits — exactly threadLiveUpdateTmpl.
	mux.HandleFunc("GET /inbound", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = inboundTmpl.Execute(w, bubble{ID: nextID(), Direction: "in", Body: "resposta recebida"})
	})

	slog.Info("inbox-autoscroll-e2e-fixture wired", "seed_messages", seed)
	return mux
}

// idSeq hands out stable, monotonically increasing bubble ids so appended
// bubbles get unique #msg-N anchors the Playwright probe can address.
var idSeq atomic.Int64

func nextID() string {
	return "f" + strconv.FormatInt(idSeq.Add(1), 10)
}

func seedBubbles(n int) []bubble {
	if n < 1 {
		n = 1
	}
	out := make([]bubble, 0, n)
	for i := 0; i < n; i++ {
		dir := "in"
		if i%2 == 1 {
			dir = "out"
		}
		out = append(out, bubble{
			ID:        "seed" + strconv.Itoa(i),
			Direction: dir,
			Body:      "mensagem de histórico " + strconv.Itoa(i+1),
		})
	}
	return out
}

// bubble is the fixture's message payload. It mirrors the production
// message-bubble contract inbox.css styles and inbox.js scrolls past:
// id="msg-{ID}", class "message-bubble" plus the direction modifier.
type bubble struct {
	ID        string
	Direction string // "in" | "out"
	Body      string
}

func (b bubble) DirClass() string {
	if b.Direction == "out" {
		return "message-bubble--out"
	}
	return "message-bubble--in"
}

type conversationData struct {
	Bubbles []bubble
}

type hostPageData struct {
	Nonce string
}

// bubbleTmplSrc is the single message bubble — the smallest swap unit, the
// same shape conversationThreadTmpl/messageBubbleTmpl render in
// production: <li id="msg-{ID}" class="message-bubble …">.
const bubbleTmplSrc = `<li id="msg-{{.ID}}" class="message-bubble {{.DirClass}}" data-direction="{{.Direction}}" role="listitem"><p class="message-bubble__body">{{.Body}}</p></li>`

// conversationTmplSrc is the conversation view fragment swapped into
// #inbox-conversation-pane (innerHTML). It carries:
//   - #conversation-thread (the scroll container) seeded to overflow,
//   - #thread-live-poll (the SIN-65419 sentinel, which is ALSO the inbound
//     trigger here), and
//   - the compose form posting to .../messages (beforeend).
//
// Inbound fidelity (SIN-65455 CTO review): the shipped inbox.js detects the
// inbound moment by the TRIGGER element id — isPollTrigger keys off
// evt.detail.elt.id === "thread-live-poll". So the on-demand inbound
// trigger lives on the sentinel itself (hx-get="/inbound" hx-trigger="click"
// hx-target="this" hx-swap="outerHTML"), exactly the production sentinel
// shape (threadLivePollTmpl) except hx-trigger is "click" instead of
// "every 3s" so the probe fires inbound deterministically without racing the
// timer. Firing it from a separate button would make elt.id !=
// "thread-live-poll" and inbox.js would NOT recognise the swap as inbound —
// the probe would then validate a variant, not what ships.
const conversationTmplSrc = `<article class="conversation" aria-label="Conversa">
  <ol id="conversation-thread" class="conversation__thread" role="list">
    {{- range .Bubbles}}
    <li id="msg-{{.ID}}" class="message-bubble {{.DirClass}}" data-direction="{{.Direction}}" role="listitem"><p class="message-bubble__body">{{.Body}}</p></li>
    {{- end}}
  </ol>
  <div id="thread-live-poll" class="thread-live-poll" aria-hidden="true"
       hx-get="/inbound" hx-trigger="click" hx-target="this" hx-swap="outerHTML"></div>
  <form class="conversation__compose"
        hx-post="/inbox/conversations/conv-e2e/messages"
        hx-target="#conversation-thread"
        hx-swap="beforeend">
    <label for="compose-body" class="visually-hidden">Mensagem</label>
    <textarea id="compose-body" name="body" rows="3" placeholder="Escreva sua resposta…"></textarea>
    <button id="compose-submit" type="submit" class="conversation__compose-submit">Enviar</button>
  </form>
</article>`

// inboundTmplSrc is the live-thread-update response (SIN-65419): the fresh
// sentinel (outerHTML self-swap) followed by the OOB append. Mirrors
// threadLiveUpdateTmpl. The fresh sentinel re-arms with the same
// hx-trigger="click" wiring so a repeat inbound can be fired — the same way
// production's response re-arms the every-3s sentinel. Driving inbound via a
// click on the sentinel itself keeps evt.detail.elt.id === "thread-live-poll",
// which is exactly how the shipped inbox.js recognises the inbound moment.
const inboundTmplSrc = `<div id="thread-live-poll" class="thread-live-poll" aria-hidden="true" hx-get="/inbound" hx-trigger="click" hx-target="this" hx-swap="outerHTML"></div><ol hx-swap-oob="beforeend:#conversation-thread"><li id="msg-{{.ID}}" class="message-bubble {{.DirClass}}" data-direction="{{.Direction}}" role="listitem"><p class="message-bubble__body">{{.Body}}</p></li></ol>`

// hostPageTmplSrc is the inbox shell host page. The production assets are
// linked exactly as inboxLayoutTmpl links them: the htmx-config meta
// (includeIndicatorStyles:false), inbox.css, the vendored htmx bundle with
// its SRI attribute pair, and inbox.js as <script src nonce defer>. The
// fixture-only fixture.css is linked last so its fixed-height scroll
// viewport wins the cascade.
const hostPageTmplSrc = `<!doctype html>
<html lang="pt-BR">
<head>
  <meta charset="utf-8">
  <title>Inbox auto-scroll E2E fixture (SIN-65455)</title>
  <meta name="htmx-config" content='{"includeIndicatorStyles":false}'>
  <link rel="stylesheet" href="/static/css/inbox.css">
  <link rel="stylesheet" href="/fixture.css">
  <script src="/static/vendor/htmx/2.0.9/htmx.min.js" {{ vendorSRI "htmx/2.0.9/htmx.min.js" }}></script>
  <script src="/static/js/inbox.js" nonce="{{.Nonce}}" defer></script>
</head>
<body>
  <div class="inbox-shell" data-testid="inbox-shell">
    <nav class="inbox-list-pane" aria-label="Conversas" data-testid="inbox-list-pane">
      <button id="open-conversation" type="button"
              hx-get="/conversation"
              hx-target="#inbox-conversation-pane"
              hx-swap="innerHTML">Abrir conversa</button>
    </nav>
    <section id="inbox-conversation-pane" class="inbox-conversation-pane" aria-live="polite" aria-label="Conversa selecionada" data-testid="inbox-conversation-pane">
      <div class="inbox-empty" role="status">
        <p class="inbox-empty__title">Selecione uma conversa.</p>
      </div>
    </section>
  </div>
</body>
</html>`

// fixtureCSS pins the thread to a fixed-height scroll viewport so the seed
// bubbles overflow it deterministically, independent of the pitho design
// tokens inbox.css references. NOT a production stylesheet — layout only.
const fixtureCSS = `* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; }
.inbox-shell { display: flex; gap: 8px; }
.inbox-list-pane { width: 180px; flex: none; }
#inbox-conversation-pane { flex: 1; }
.conversation { margin: 0; }
#conversation-thread {
  height: 300px;
  overflow-y: auto;
  list-style: none;
  margin: 0;
  padding: 8px;
  display: flex;
  flex-direction: column;
  gap: 8px;
  border: 1px solid #ccc;
}
.message-bubble { padding: 8px 12px; border-radius: 8px; background: #eee; }
.message-bubble--out { align-self: flex-end; background: #cfe3ff; }
.thread-live-poll { display: none; }
`

// fixtureRequiredVendorAssets enumerates every vendored relpath referenced
// by hostPageTmplSrc. mustBuildHostPageTmpl validates each against the
// embedded CHECKSUMS.txt manifest and panics at startup if any are
// missing (the SIN-62535 panic-at-startup contract). Mirrors the aipanel
// fixture — keep in sync when adding vendored bundles.
var fixtureRequiredVendorAssets = []string{
	"htmx/2.0.9/htmx.min.js",
}

var (
	bubbleTmpl       = template.Must(template.New("bubble").Parse(bubbleTmplSrc))
	conversationTmpl = template.Must(template.New("conversation").Parse(conversationTmplSrc))
	inboundTmpl      = template.Must(template.New("inbound").Parse(inboundTmplSrc))
	hostPageTmpl     = mustBuildHostPageTmpl(func() (vendorintegrity.VendorIntegrity, error) {
		return vendorintegrity.NewFromFS(vendorassets.ChecksumsFS, vendorassets.ChecksumsManifestPath)
	})
)

// mustBuildHostPageTmpl returns the parsed host page template wired to a
// vendor-integrity provider produced by newProvider. The closure is the
// unit-test seam: tests swap in a stub provider to exercise the
// missing-asset panic path without touching the embedded manifest.
func mustBuildHostPageTmpl(newProvider func() (vendorintegrity.VendorIntegrity, error)) *template.Template {
	provider, err := newProvider()
	if err != nil {
		panic(fmt.Sprintf("inbox-autoscroll-e2e-fixture: load vendor integrity: %v", err))
	}
	for _, relPath := range fixtureRequiredVendorAssets {
		if _, err := provider.SRIAttribute(relPath); err != nil {
			panic(fmt.Sprintf("inbox-autoscroll-e2e-fixture: vendor manifest missing required asset %q: %v", relPath, err))
		}
	}
	funcs := template.FuncMap{
		"vendorSRI": func(relPath string) (template.HTMLAttr, error) {
			attr, err := provider.SRIAttribute(relPath)
			if err != nil {
				return "", err
			}
			return template.HTMLAttr(attr), nil
		},
	}
	return template.Must(template.New("hostPage").Funcs(funcs).Parse(hostPageTmplSrc))
}
