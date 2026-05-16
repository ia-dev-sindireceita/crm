// Funnel board drag-and-drop.
//
// SIN-62797 / Fase 2 F2-12. Vanilla HTML5 drag/drop wired against the
// server-rendered funnel board (internal/web/funnel). CSP-safe: no
// inline event handlers, no eval — every action is dispatched through
// HTMX's programmatic `htmx.ajax` so the existing CSRF + redirect
// semantics carry through.
//
// The board ships an a11y-first keyboard fallback (← / → buttons
// inside each card) that targets the same /funnel/transitions
// endpoint via standard hx-post attributes, so this script is
// strictly a progressive enhancement.

(function () {
  'use strict';

  var DRAG_TYPE = 'application/x-funnel-card';
  var draggingID = null;

  function isFunnelCard(node) {
    return node && node.classList && node.classList.contains('funnel-card');
  }

  function isFunnelColumn(node) {
    return node && node.classList && node.classList.contains('funnel-column__list');
  }

  function findColumn(target) {
    var n = target;
    while (n && n !== document) {
      if (isFunnelColumn(n)) {
        return n;
      }
      n = n.parentNode;
    }
    return null;
  }

  document.addEventListener('dragstart', function (event) {
    var card = event.target;
    if (!isFunnelCard(card)) {
      return;
    }
    var id = card.getAttribute('data-conversation-id');
    if (!id) {
      return;
    }
    draggingID = id;
    if (event.dataTransfer) {
      try {
        event.dataTransfer.setData(DRAG_TYPE, id);
      } catch (_) {
        // Some browsers reject custom MIME types; fall back to text/plain.
        event.dataTransfer.setData('text/plain', id);
      }
      event.dataTransfer.effectAllowed = 'move';
    }
    card.classList.add('funnel-card--dragging');
  });

  document.addEventListener('dragend', function (event) {
    var card = event.target;
    if (isFunnelCard(card)) {
      card.classList.remove('funnel-card--dragging');
    }
    draggingID = null;
  });

  document.addEventListener('dragover', function (event) {
    var col = findColumn(event.target);
    if (!col) {
      return;
    }
    event.preventDefault();
    if (event.dataTransfer) {
      event.dataTransfer.dropEffect = 'move';
    }
    col.classList.add('funnel-column__list--drop-target');
  });

  document.addEventListener('dragleave', function (event) {
    if (isFunnelColumn(event.target)) {
      event.target.classList.remove('funnel-column__list--drop-target');
    }
  });

  document.addEventListener('drop', function (event) {
    var col = findColumn(event.target);
    if (!col) {
      return;
    }
    event.preventDefault();
    col.classList.remove('funnel-column__list--drop-target');

    var conversationID = null;
    if (event.dataTransfer) {
      conversationID = event.dataTransfer.getData(DRAG_TYPE) || event.dataTransfer.getData('text/plain') || draggingID;
    } else {
      conversationID = draggingID;
    }
    if (!conversationID) {
      return;
    }
    var toStageKey = col.getAttribute('data-stage-key');
    if (!toStageKey) {
      return;
    }
    var card = document.getElementById('card-' + conversationID);
    if (!card) {
      return;
    }
    var fromColumn = card.parentNode;
    // Optimistic DOM move so the operator sees instant feedback.
    col.appendChild(card);

    if (!window.htmx || typeof window.htmx.ajax !== 'function') {
      return;
    }
    window.htmx.ajax('POST', '/funnel/transitions', {
      target: '#card-' + conversationID,
      swap: 'outerHTML',
      values: {
        conversation_id: conversationID,
        to_stage_key: toStageKey
      }
    }).catch(function () {
      // Revert the optimistic move on transport error so the board
      // does not lie about the persisted state.
      if (fromColumn && fromColumn.appendChild) {
        fromColumn.appendChild(card);
      }
    });
  });

  // Keyboard fallback: the ← / → buttons rendered next to every card
  // already carry hx-* attributes, so HTMX handles them with zero JS
  // here. We expose a small "mark draggable" helper so the cards stay
  // draggable across HTMX swaps without re-attaching listeners — the
  // attribute is set in the template, and `draggable` defaults to
  // false for <li> so we re-mirror it after every htmx:afterSwap.
  function applyDraggable(root) {
    var nodes = (root || document).querySelectorAll('.funnel-card');
    for (var i = 0; i < nodes.length; i++) {
      nodes[i].setAttribute('draggable', 'true');
    }
  }

  document.addEventListener('DOMContentLoaded', function () {
    applyDraggable(document);
  });

  document.addEventListener('htmx:afterSwap', function (event) {
    applyDraggable(event.target);
  });
})();
