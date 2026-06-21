/*
 * inbox.js — auto-scroll the conversation thread to the latest message
 * (SIN-65454).
 *
 * The CRM runs under a strict CSP that bans inline `on*=`/`hx-on:*` handlers,
 * so the scroll logic lives in this external, nonce-loaded file wired from the
 * inbox shell's `head_extra` (internal/web/inbox/templates.go) and binds to
 * htmx swap events on `document` — no inline JS, nothing per-element.
 *
 * Thread-affecting swap seams (see internal/web/inbox/templates.go):
 *   - open / switch conversation: GET /inbox/conversations/{id}
 *       → hx-swap="innerHTML" into #inbox-conversation-pane (renders the
 *         conversation view containing #conversation-thread)
 *   - send: POST /inbox/conversations/{id}/messages
 *       → hx-swap="beforeend" into #conversation-thread (trigger = compose form)
 *   - inbound poll: GET /inbox/conversations/{id}/messages/since
 *       → OOB <ol hx-swap-oob="beforeend:#conversation-thread"> append
 *         (trigger element = #thread-live-poll)
 *
 * Behaviour: opening a conversation and sending a message always pin to the
 * newest bubble. An inbound poll only scrolls when the operator is already
 * at/near the bottom, so scrolling up to read history is never interrupted.
 */
(function () {
  'use strict';

  var THREAD_ID = 'conversation-thread';
  var PANE_ID = 'inbox-conversation-pane';
  var POLL_ID = 'thread-live-poll';
  // Treat "within 100px of the bottom" as pinned, so a near-bottom operator
  // still follows new inbound messages without an exact-pixel match.
  var NEAR_BOTTOM_PX = 100;

  // Decision is computed at htmx:beforeSwap (while the DOM still holds the
  // pre-swap scroll position) and applied at htmx:afterSwap / afterSettle,
  // once the new bubble — including any out-of-band poll append — has landed.
  var scrollPending = false;

  function thread() {
    return document.getElementById(THREAD_ID);
  }

  function nearBottom(el) {
    return (el.scrollHeight - el.scrollTop - el.clientHeight) <= NEAR_BOTTOM_PX;
  }

  function scrollToBottom(el) {
    el.scrollTop = el.scrollHeight;
  }

  // The live poll's request is fired by the #thread-live-poll sentinel; every
  // other thread swap is operator-driven (open, send, reset).
  function isPollTrigger(evt) {
    var elt = evt.detail && evt.detail.elt;
    return !!(elt && elt.id === POLL_ID);
  }

  document.addEventListener('htmx:beforeSwap', function (evt) {
    var el = thread();
    if (isPollTrigger(evt)) {
      // Inbound message: preserve the operator's position unless they're
      // already at/near the bottom. No thread yet → nothing to follow.
      scrollPending = el ? nearBottom(el) : false;
      return;
    }
    // Only act when the swap actually targets the conversation pane or the
    // thread; list-filter / customer-pane swaps must not yank the thread.
    var target = evt.detail && evt.detail.target;
    var id = target && target.id;
    scrollPending = (id === PANE_ID || id === THREAD_ID);
  });

  function applyScroll() {
    if (!scrollPending) return;
    var el = thread();
    if (el) scrollToBottom(el);
    // Leave scrollPending set so afterSettle can re-pin once OOB appends have
    // landed; it is reset on the next htmx:beforeSwap.
  }

  document.addEventListener('htmx:afterSwap', applyScroll);
  document.addEventListener('htmx:afterSettle', applyScroll);

  // Defensive: if a thread is ever present on the initial full-page render,
  // land at the latest message.
  document.addEventListener('DOMContentLoaded', function () {
    var el = thread();
    if (el) scrollToBottom(el);
  });
})();
