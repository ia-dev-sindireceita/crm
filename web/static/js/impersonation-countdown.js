// Impersonation countdown timer.
//
// SIN-65379 / UX-F10. The impersonation banner
// (internal/web/shell/layout.html and
// internal/web/master/impersonation_banner.go) renders an
// `<aside data-impersonation-banner>` carrying `data-expires-at`
// (RFC3339Nano) and `data-server-now`, plus a
// `<span data-impersonation-countdown>` whose only content is a
// `<noscript>` fallback. Until this file existed the span never
// ticked: the layout comment promised "client JS ticks the countdown"
// but the JS was never written, so the master operator saw a blank
// pill instead of the remaining time.
//
// The 15-minute envelope expiry is enforced SERVER-SIDE — the next
// request after expiry 303s to /master/tenants?expired=1 (see the
// master impersonation logout semantics). This script is a UX
// affordance only, never a security control: it shows how much time
// is left and does NOT force navigation when it reaches zero.
//
// Loaded as `<script src="/static/js/impersonation-countdown.js"
// defer>` so it runs after the DOM is parsed; the strict-CSP policy
// (internal/http/middleware/csp/csp.go emits
// `script-src 'self' 'nonce-…'`) covers an external script from
// `'self'` without a nonce. No inline handler, no eval — the
// CSP-safe pattern from SIN-63977.
//
// Server-authoritative clock: we measure the offset between the
// browser clock and the server's `data-server-now` once, then tick the
// remaining time against `(Date.now() - offset)` so a skewed client
// clock cannot inflate or deflate the displayed time.

(function () {
  'use strict';

  function pad2(n) {
    return n < 10 ? '0' + n : String(n);
  }

  function formatRemaining(ms) {
    if (ms <= 0) {
      return '00:00';
    }
    var totalSeconds = Math.floor(ms / 1000);
    var minutes = Math.floor(totalSeconds / 60);
    var seconds = totalSeconds % 60;
    return pad2(minutes) + ':' + pad2(seconds);
  }

  function start() {
    var banner = document.querySelector('[data-impersonation-banner]');
    if (!banner) {
      return; // no active impersonation on this page — nothing to tick.
    }
    var output = banner.querySelector('[data-impersonation-countdown]');
    if (!output) {
      return;
    }

    var expiresAt = Date.parse(banner.getAttribute('data-expires-at'));
    if (isNaN(expiresAt)) {
      return; // malformed/empty — keep the <noscript> fallback intact.
    }
    var serverNow = Date.parse(banner.getAttribute('data-server-now'));

    // offset = browser clock - server clock. `remaining` is measured
    // against the server clock so a wrong local clock cannot mis-render
    // the countdown. A missing/invalid server-now degrades to trusting
    // the local clock (offset 0).
    var offset = isNaN(serverNow) ? 0 : Date.now() - serverNow;

    var timer = null;

    function render() {
      var remaining = expiresAt - (Date.now() - offset);
      if (remaining <= 0) {
        output.textContent = 'Sessão expirada';
        if (timer !== null) {
          clearInterval(timer);
          timer = null;
        }
        return;
      }
      output.textContent = formatRemaining(remaining);
    }

    render();
    timer = setInterval(render, 1000);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', start);
  } else {
    start();
  }
})();
