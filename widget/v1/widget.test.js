// node --test for widget/v1/widget.js — pure helpers + a static CSP
// rule check on the source file. Runs without npm install.
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const widget = require('./widget.js');

test('deriveAPIBase strips path and keeps protocol+host', () => {
	assert.equal(
		widget.deriveAPIBase('https://acme.crm.sindireceita.org.br/widget.js'),
		'https://acme.crm.sindireceita.org.br',
	);
	assert.equal(
		widget.deriveAPIBase('http://localhost:8080/static/widget/widget.js'),
		'http://localhost:8080',
	);
	assert.equal(widget.deriveAPIBase(''), '');
	assert.equal(widget.deriveAPIBase(null), '');
});

test('builds endpoint URLs', () => {
	const b = 'https://x.example';
	assert.equal(widget.buildSessionURL(b), 'https://x.example/widget/v1/session');
	assert.equal(widget.buildMessageURL(b), 'https://x.example/widget/v1/message');
	assert.equal(
		widget.buildStreamURL(b, 'sess id/needs encoding'),
		'https://x.example/widget/v1/stream?session_id=sess%20id%2Fneeds%20encoding',
	);
});

test('newClientMsgID is unique-ish and non-empty', () => {
	const a = widget.newClientMsgID();
	const b = widget.newClientMsgID();
	assert.ok(a.length > 0 && b.length > 0);
	assert.notEqual(a, b);
});

test('session round-trips through storage', () => {
	const store = makeStorage();
	assert.equal(widget.loadSession(store), null);
	widget.saveSession(store, {
		session_id: 's1', csrf_token: 't1', expires_at: '2026-05-16T12:00:00Z',
	});
	const got = widget.loadSession(store);
	assert.equal(got.session_id, 's1');
	assert.equal(got.csrf_token, 't1');
	widget.clearSession(store);
	assert.equal(widget.loadSession(store), null);
});

test('loadSession rejects malformed or incomplete payloads', () => {
	const store = makeStorage();
	store.setItem(widget.STORE_KEY, '{not json');
	assert.equal(widget.loadSession(store), null);
	store.setItem(widget.STORE_KEY, JSON.stringify({ session_id: 'only' }));
	assert.equal(widget.loadSession(store), null);
});

test('postMessage sends session/csrf headers and JSON body', async () => {
	const calls = [];
	const fetchImpl = async (url, init) => {
		calls.push({ url, init });
		return { ok: true, status: 204, json: async () => ({}) };
	};
	await widget.postMessage(
		'https://x.example',
		{ session_id: 'sess', csrf_token: 'csrf' },
		'hello',
		fetchImpl,
	);
	assert.equal(calls[0].url, 'https://x.example/widget/v1/message');
	assert.equal(calls[0].init.method, 'POST');
	assert.equal(calls[0].init.headers['X-Webchat-Session'], 'sess');
	assert.equal(calls[0].init.headers['X-Webchat-CSRF'], 'csrf');
	const body = JSON.parse(calls[0].init.body);
	assert.equal(body.body, 'hello');
	assert.ok(typeof body.client_msg_id === 'string' && body.client_msg_id.length > 0);
});

test('postMessage rejects on non-2xx', async () => {
	const fetchImpl = async () => ({ ok: false, status: 401, json: async () => ({}) });
	await assert.rejects(() => widget.postMessage('https://x.example',
		{ session_id: 's', csrf_token: 't' }, 'hi', fetchImpl));
});

test('startSession returns parsed JSON on success', async () => {
	const fetchImpl = async () => ({
		ok: true, status: 200,
		json: async () => ({ session_id: 's', csrf_token: 'c', expires_at: 'e' }),
	});
	const got = await widget.startSession('https://x.example', fetchImpl);
	assert.equal(got.session_id, 's');
	assert.equal(got.csrf_token, 'c');
});

// CSP-rule static check on the source: forbid eval, new Function, and
// innerHTML/outerHTML assignments. textContent is the only sanctioned
// way to inject visitor or agent text into the DOM.
test('widget.js source is CSP-safe (no eval / Function / innerHTML)', () => {
	const src = fs.readFileSync(path.join(__dirname, 'widget.js'), 'utf8');
	const banned = [
		/\beval\s*\(/,
		/\bnew\s+Function\s*\(/,
		/\.innerHTML\s*=/,
		/\.outerHTML\s*=/,
		/document\.write\s*\(/,
	];
	for (const re of banned) {
		assert.equal(re.test(src), false, 'widget.js must not use ' + re);
	}
	assert.ok(/textContent\s*=/.test(src),
		'widget.js must use textContent for dynamic text insertion');
});

function makeStorage() {
	const m = new Map();
	return {
		getItem: (k) => m.has(k) ? m.get(k) : null,
		setItem: (k, v) => m.set(k, String(v)),
		removeItem: (k) => m.delete(k),
	};
}
