// App-shell top-bar toggles.
//
// SIN-63935 / UX-F1 B-1. The layout (internal/web/shell/layout.html)
// stamps two collapsed UI elements:
//
//   - .app-shell__hamburger controls #app-shell-nav via aria-controls;
//     the nav carries data-collapsed="true" until the user expands it.
//     Below the --bp-sm breakpoint app-shell.css hides the nav unless
//     data-collapsed="false".
//   - .app-shell__user-menu-toggle controls a sibling
//     .app-shell__user-menu-panel that ships with the `hidden`
//     attribute on every viewport.
//
// Without this script both toggles are dead buttons: the hamburger
// never reveals the nav on mobile and the user-menu never opens at
// all. Loaded as `<script src="/static/js/app-shell.js" defer>` so it
// runs after the DOM is parsed; CSP `script-src 'self'` covers it
// without a nonce.
//
// Progressive enhancement only — buttons are native <button> elements,
// so keyboard Enter / Space already triggers `click`. Escape closes the
// user-menu and a click outside the menu collapses it. The hamburger
// stays open until the user clicks it again (matches the existing
// behaviour of every server-rendered surface — no JS state machine).

(function () {
  'use strict';

  function bindHamburger() {
    var button = document.querySelector('.app-shell__hamburger');
    var nav = document.getElementById('app-shell-nav');
    if (!button || !nav) {
      return;
    }
    button.addEventListener('click', function () {
      var expanded = button.getAttribute('aria-expanded') === 'true';
      var next = !expanded;
      button.setAttribute('aria-expanded', next ? 'true' : 'false');
      nav.setAttribute('data-collapsed', next ? 'false' : 'true');
    });
  }

  function bindUserMenu() {
    var toggle = document.querySelector('.app-shell__user-menu-toggle');
    var panel = document.querySelector('.app-shell__user-menu-panel');
    if (!toggle || !panel) {
      return;
    }
    var container = toggle.closest('.app-shell__user-menu') || panel.parentNode;

    function setOpen(open) {
      toggle.setAttribute('aria-expanded', open ? 'true' : 'false');
      if (open) {
        panel.removeAttribute('hidden');
      } else {
        panel.setAttribute('hidden', '');
      }
    }

    toggle.addEventListener('click', function (event) {
      event.stopPropagation();
      var open = toggle.getAttribute('aria-expanded') === 'true';
      setOpen(!open);
    });

    document.addEventListener('click', function (event) {
      if (toggle.getAttribute('aria-expanded') !== 'true') {
        return;
      }
      if (container && container.contains(event.target)) {
        return;
      }
      setOpen(false);
    });

    document.addEventListener('keydown', function (event) {
      if (event.key !== 'Escape') {
        return;
      }
      if (toggle.getAttribute('aria-expanded') !== 'true') {
        return;
      }
      setOpen(false);
      toggle.focus();
    });
  }

  function init() {
    bindHamburger();
    bindUserMenu();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
