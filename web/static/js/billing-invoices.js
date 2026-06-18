// Billing invoices — progressive enhancement for the PIX copia-e-cola
// button.
//
// SIN-65123 (Peitho · Tranche-D). internal/web/billing/invoices/templates.go
// linked this file but it never existed, so the link 404'd and the
// copy-to-clipboard button on the invoice-detail page silently no-op'd.
//
// The strict-CSP middleware (internal/http/middleware/csp/csp.go) emits
// `script-src 'self' 'nonce-…'` with no `unsafe-inline`/`unsafe-eval`, so
// inline `onclick=` attribute handlers (and htmx `hx-on:`) are blocked at
// runtime. Loading this file with `<script src="/static/js/billing-invoices.js"
// defer>` is covered by `script-src 'self'` without a nonce.
//
// Event delegation on `document` so HTMX swaps that replace the invoice
// body (hx-target="body") keep the button wired without re-running this
// script. Progressive enhancement only — with JS disabled the textarea is
// still selectable and the EMVCo string is fully visible, and the rest of
// the page (dunning banner, status polling, list/detail navigation) is
// driven by plain links + htmx attributes that need no script.

(function () {
  'use strict';

  var COPIED_CLASS = 'is-copied';
  var COPIED_LABEL = 'copiado!';
  var RESET_MS = 2000;

  // Resolve the textarea/input a copy button points at via
  // data-copy-target (a CSS selector). Returns null when the target is
  // missing so a malformed button degrades to a no-op rather than throwing.
  function targetOf(button) {
    var selector = button.getAttribute('data-copy-target');
    if (!selector) {
      return null;
    }
    try {
      return document.querySelector(selector);
    } catch (e) {
      return null;
    }
  }

  function flashCopied(button) {
    if (button.classList.contains(COPIED_CLASS)) {
      return;
    }
    var original = button.textContent;
    button.classList.add(COPIED_CLASS);
    button.textContent = COPIED_LABEL;
    window.setTimeout(function () {
      button.classList.remove(COPIED_CLASS);
      button.textContent = original;
    }, RESET_MS);
  }

  // Copy with the async Clipboard API when available (https / localhost),
  // falling back to selecting the field + execCommand for older/insecure
  // contexts. Either way the user sees the value selected.
  function copyValue(field, button) {
    var value = field.value !== undefined ? field.value : field.textContent;
    field.focus();
    if (typeof field.select === 'function') {
      field.select();
    }

    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      navigator.clipboard.writeText(value).then(
        function () { flashCopied(button); },
        function () { legacyCopy(button); }
      );
      return;
    }
    legacyCopy(button);
  }

  function legacyCopy(button) {
    try {
      if (document.execCommand('copy')) {
        flashCopied(button);
      }
    } catch (e) {
      // Selection is left in place so the user can copy manually.
    }
  }

  function closestCopyButton(node) {
    while (node && node !== document) {
      if (node.nodeType === 1 && node.hasAttribute('data-copy-target')) {
        return node;
      }
      node = node.parentNode;
    }
    return null;
  }

  document.addEventListener('click', function (event) {
    var button = closestCopyButton(event.target);
    if (!button) {
      return;
    }
    var field = targetOf(button);
    if (!field) {
      return;
    }
    event.preventDefault();
    copyValue(field, button);
  });
})();
