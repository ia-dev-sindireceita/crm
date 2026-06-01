// Master grants "kind" toggle.
//
// SIN-63977 / SEC-F1. The grants_panel template
// (internal/web/master/grants_templates.go) renders two radio inputs
// (`input[name="kind"]`) plus two fieldsets carrying
// `data-grant-fields="free_subscription_period|extra_tokens"`. The
// active radio's value selects which fieldset is visible; the others
// are `hidden`.
//
// Previously the template inlined an `onclick="..."` attribute on each
// radio. The strict-CSP middleware
// (internal/http/middleware/csp/csp.go) emits
// `script-src 'self' 'nonce-…'` with no `unsafe-inline`/`unsafe-eval`,
// so inline attribute handlers (and htmx `hx-on:`) are blocked at
// runtime. Loading this file with `<script src="/static/js/…"
// defer>` is covered by `script-src 'self'` without a nonce.
//
// Event delegation on `document` so HTMX swaps of `#grants-panel`
// (the partial returned by POST /master/tenants/{id}/grants and the
// 4-eyes request endpoint) are covered without re-running this
// script. Progressive enhancement only — when JS is disabled the
// fieldsets fall back to their server-rendered `hidden` state derived
// from `data.Kind`.

(function () {
  'use strict';

  function toggleGrantFields(value) {
    var nodes = document.querySelectorAll('[data-grant-fields]');
    for (var i = 0; i < nodes.length; i++) {
      var node = nodes[i];
      node.hidden = node.dataset.grantFields !== value;
    }
  }

  function isKindRadio(node) {
    if (!node || node.tagName !== 'INPUT') {
      return false;
    }
    if (node.getAttribute('name') !== 'kind') {
      return false;
    }
    return node.hasAttribute('data-grant-kind-toggle');
  }

  // `change` covers keyboard and click-driven radio selection. We
  // also bind `click` so re-clicking the already-selected radio
  // still normalises the fieldset visibility (matches the original
  // inline `onclick` semantics that ran on every click).
  document.addEventListener('change', function (event) {
    var target = event.target;
    if (!isKindRadio(target)) {
      return;
    }
    if (target.checked) {
      toggleGrantFields(target.value);
    }
  });

  document.addEventListener('click', function (event) {
    var target = event.target;
    if (!isKindRadio(target)) {
      return;
    }
    if (target.checked) {
      toggleGrantFields(target.value);
    }
  });
})();
