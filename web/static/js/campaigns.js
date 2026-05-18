// Campaigns dashboard helpers.
//
// SIN-62962 / Fase 4 web/campaigns. The only client-side behaviour is
// the "copy link" button on the dashboard rows and on the detail
// header. Everything else (table refresh, form submit, navigation)
// goes through HTMX attributes server-side.
//
// CSP-safe: no inline event handlers, no eval. The click is bound
// once at document level so HTMX swap replacements keep working
// without re-binding.

(function () {
  'use strict';

  function copyToClipboard(text) {
    if (!text) {
      return Promise.resolve(false);
    }
    if (navigator.clipboard && navigator.clipboard.writeText) {
      return navigator.clipboard.writeText(text).then(function () { return true; }).catch(function () { return false; });
    }
    // Fallback for older browsers that do not expose the async API:
    // build a hidden textarea, select, and execCommand('copy').
    try {
      var area = document.createElement('textarea');
      area.value = text;
      area.setAttribute('readonly', '');
      area.style.position = 'absolute';
      area.style.left = '-9999px';
      document.body.appendChild(area);
      area.select();
      var ok = document.execCommand && document.execCommand('copy');
      document.body.removeChild(area);
      return Promise.resolve(!!ok);
    } catch (_) {
      return Promise.resolve(false);
    }
  }

  function flashFeedback(button, ok) {
    var original = button.textContent;
    button.textContent = ok ? 'copiado!' : 'falhou';
    button.classList.add(ok ? 'campaign-copy--ok' : 'campaign-copy--err');
    window.setTimeout(function () {
      button.textContent = original;
      button.classList.remove('campaign-copy--ok');
      button.classList.remove('campaign-copy--err');
    }, 1500);
  }

  document.addEventListener('click', function (event) {
    var target = event.target;
    if (!target || !target.classList || !target.classList.contains('campaign-copy')) {
      return;
    }
    event.preventDefault();
    var link = target.getAttribute('data-link') || '';
    copyToClipboard(link).then(function (ok) { flashFeedback(target, ok); });
  });
})();
