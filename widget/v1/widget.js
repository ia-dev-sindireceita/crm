// Sindireceita CRM Webchat Widget (SIN-62800 / F2-14).
//
// Vanilla JS, CSP-safe (no eval, no Function, no innerHTML). All
// dynamic text is inserted via textContent so the browser escapes it
// automatically. The widget exposes a single global, window.__sindCRMWidget,
// to host helpers and the manual init entry point.
//
// Endpoint contract (F2-11):
//   POST <base>/widget/v1/session           -> { session_id, csrf_token, expires_at }
//   POST <base>/widget/v1/message           -> 204 No Content
//                                              headers: X-Webchat-Session, X-Webchat-CSRF
//                                              body:    { body, client_msg_id, email?, phone? }
//   GET  <base>/widget/v1/stream?session_id -> text/event-stream (EventSource)
//
// The base URL is derived from the script's own src so the embed
// snippet does not have to repeat it.
(function (global, factory) {
	'use strict';
	if (typeof module === 'object' && module.exports) {
		module.exports = factory();
	} else {
		const widget = factory();
		global.__sindCRMWidget = widget;
		if (typeof document !== 'undefined') {
			if (document.readyState === 'loading') {
				document.addEventListener('DOMContentLoaded', function () {
					widget.autoMount();
				});
			} else {
				widget.autoMount();
			}
		}
	}
})(typeof window !== 'undefined' ? window : globalThis, function () {
	'use strict';

	const STORE_KEY = 'sindCRMWidget.v1';
	const PANEL_ID = 'sindcrm-widget-panel';
	const TOGGLE_ID = 'sindcrm-widget-toggle';

	// ------- Pure helpers (URL + storage; covered by node --test) -------

	function deriveAPIBase(scriptSrc) {
		if (!scriptSrc) return '';
		const u = new URL(scriptSrc);
		return u.protocol + '//' + u.host;
	}

	function buildSessionURL(base) {
		return base + '/widget/v1/session';
	}
	function buildMessageURL(base) {
		return base + '/widget/v1/message';
	}
	function buildStreamURL(base, sessionID) {
		return base + '/widget/v1/stream?session_id=' + encodeURIComponent(sessionID);
	}

	function newClientMsgID() {
		const rnd = Math.random().toString(36).slice(2, 10);
		return Date.now().toString(36) + '-' + rnd;
	}

	function loadSession(storage) {
		try {
			const raw = storage.getItem(STORE_KEY);
			if (!raw) return null;
			const obj = JSON.parse(raw);
			if (!obj || typeof obj !== 'object') return null;
			if (typeof obj.session_id !== 'string' || typeof obj.csrf_token !== 'string') return null;
			return obj;
		} catch (_) {
			return null;
		}
	}

	function saveSession(storage, sess) {
		storage.setItem(STORE_KEY, JSON.stringify({
			session_id: sess.session_id,
			csrf_token: sess.csrf_token,
			expires_at: sess.expires_at,
		}));
	}

	function clearSession(storage) {
		storage.removeItem(STORE_KEY);
	}

	// ------- API client (injectable fetch for tests) -------

	async function startSession(base, fetchImpl) {
		const r = await fetchImpl(buildSessionURL(base), {
			method: 'POST',
			credentials: 'omit',
			headers: { 'Content-Type': 'application/json' },
		});
		if (!r.ok) throw new Error('webchat: session ' + r.status);
		return r.json();
	}

	async function postMessage(base, sess, text, fetchImpl) {
		const r = await fetchImpl(buildMessageURL(base), {
			method: 'POST',
			credentials: 'omit',
			headers: {
				'Content-Type': 'application/json',
				'X-Webchat-Session': sess.session_id,
				'X-Webchat-CSRF': sess.csrf_token,
			},
			body: JSON.stringify({
				body: text,
				client_msg_id: newClientMsgID(),
			}),
		});
		if (!r.ok) throw new Error('webchat: message ' + r.status);
	}

	// ------- DOM (CSP-safe: no innerHTML, only textContent) -------

	function el(doc, tag, attrs, text) {
		const e = doc.createElement(tag);
		if (attrs) {
			for (const k in attrs) {
				e.setAttribute(k, attrs[k]);
			}
		}
		if (text != null) e.textContent = text;
		return e;
	}

	function appendBubble(doc, container, text, from) {
		const row = el(doc, 'div', { 'class': 'sindcrm-row sindcrm-row-' + from });
		const bubble = el(doc, 'div', { 'class': 'sindcrm-bubble' }, text);
		row.appendChild(bubble);
		container.appendChild(row);
		container.scrollTop = container.scrollHeight;
	}

	function buildUI(doc) {
		const root = el(doc, 'div', { 'id': 'sindcrm-widget-root', 'role': 'complementary' });
		const toggle = el(doc, 'button', {
			'id': TOGGLE_ID,
			'type': 'button',
			'aria-label': 'Abrir chat',
			'class': 'sindcrm-toggle',
		}, 'Conversar');

		const panel = el(doc, 'section', {
			'id': PANEL_ID,
			'class': 'sindcrm-panel sindcrm-hidden',
			'aria-label': 'Chat',
			'aria-live': 'polite',
		});
		const header = el(doc, 'header', { 'class': 'sindcrm-panel-header' }, 'Atendimento');
		const close = el(doc, 'button', {
			'type': 'button',
			'aria-label': 'Fechar chat',
			'class': 'sindcrm-close',
		}, '×');
		header.appendChild(close);

		const log = el(doc, 'div', { 'class': 'sindcrm-log', 'role': 'log' });
		const form = el(doc, 'form', { 'class': 'sindcrm-form', 'autocomplete': 'off' });
		const input = el(doc, 'input', {
			'type': 'text',
			'name': 'body',
			'placeholder': 'Sua mensagem',
			'maxlength': '2000',
			'aria-label': 'Mensagem',
			'class': 'sindcrm-input',
		});
		const send = el(doc, 'button', { 'type': 'submit', 'class': 'sindcrm-send' }, 'Enviar');
		form.appendChild(input);
		form.appendChild(send);

		panel.appendChild(header);
		panel.appendChild(log);
		panel.appendChild(form);

		root.appendChild(toggle);
		root.appendChild(panel);
		return { root, toggle, panel, close, log, form, input };
	}

	// ------- Stream wiring -------

	function openStream(base, sessionID, EventSourceImpl, onMessage, onAuthError) {
		const es = new EventSourceImpl(buildStreamURL(base, sessionID));
		es.onmessage = function (ev) {
			onMessage(ev.data);
		};
		es.onerror = function () {
			if (es.readyState === EventSourceImpl.CLOSED) {
				onAuthError();
			}
		};
		return es;
	}

	// ------- Mount -------

	function mount(opts) {
		opts = opts || {};
		const doc = opts.document || document;
		const win = opts.window || window;
		const storage = opts.storage || win.sessionStorage;
		const fetchImpl = opts.fetch || win.fetch.bind(win);
		const EventSourceImpl = opts.EventSource || win.EventSource;
		const base = opts.base || deriveAPIBase(opts.scriptSrc ||
			(typeof doc.currentScript !== 'undefined' && doc.currentScript && doc.currentScript.src));

		if (!base) return null;

		const ui = buildUI(doc);
		doc.body.appendChild(ui.root);

		let sess = loadSession(storage);
		let es = null;

		function ensureSession() {
			if (sess) return Promise.resolve(sess);
			return startSession(base, fetchImpl).then(function (s) {
				sess = s;
				saveSession(storage, s);
				return s;
			});
		}

		function startStream() {
			if (!sess || es) return;
			es = openStream(base, sess.session_id, EventSourceImpl,
				function (data) {
					// Backend publishes JSON payload strings. We accept
					// both {text:"..."} and raw strings; if parsing fails
					// the raw event data is shown as-is.
					let text = data;
					try {
						const parsed = JSON.parse(data);
						if (parsed && typeof parsed.text === 'string') text = parsed.text;
					} catch (_) { /* keep raw */ }
					appendBubble(doc, ui.log, text, 'agent');
				},
				function () {
					// Session expired or invalidated. Clear and let
					// the next send re-establish.
					clearSession(storage);
					sess = null;
					if (es) { es.close(); es = null; }
				});
		}

		ui.toggle.addEventListener('click', function () {
			ui.panel.classList.remove('sindcrm-hidden');
			ui.input.focus();
			ensureSession().then(startStream).catch(function () { /* swallow */ });
		});
		ui.close.addEventListener('click', function () {
			ui.panel.classList.add('sindcrm-hidden');
		});

		ui.form.addEventListener('submit', function (ev) {
			ev.preventDefault();
			const text = String(ui.input.value || '').trim();
			if (!text) return;
			ui.input.value = '';
			appendBubble(doc, ui.log, text, 'visitor');
			ensureSession()
				.then(function (s) { return postMessage(base, s, text, fetchImpl); })
				.then(startStream)
				.catch(function () {
					appendBubble(doc, ui.log,
						'Não foi possível enviar. Tente novamente em instantes.', 'system');
				});
		});

		return { ui, base };
	}

	function autoMount() {
		if (typeof document === 'undefined' || !document.body) return null;
		return mount();
	}

	return {
		// Pure helpers (exported for node --test).
		deriveAPIBase: deriveAPIBase,
		buildSessionURL: buildSessionURL,
		buildMessageURL: buildMessageURL,
		buildStreamURL: buildStreamURL,
		newClientMsgID: newClientMsgID,
		loadSession: loadSession,
		saveSession: saveSession,
		clearSession: clearSession,
		startSession: startSession,
		postMessage: postMessage,
		// Mount + lifecycle.
		mount: mount,
		autoMount: autoMount,
		STORE_KEY: STORE_KEY,
	};
});
