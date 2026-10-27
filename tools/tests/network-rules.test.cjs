'use strict';
const test = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const filename = path.resolve(__dirname, '../../sdk/go/lanet/console/network-rules.js');
const helpers = require(filename);
const htmlFilename = path.resolve(__dirname, '../../sdk/go/lanet/console/index.html');
const html = fs.readFileSync(htmlFilename, 'utf8');

test('strict ports, firewall ranges, sources and application protocols', () => {
  for (const port of ['0', '65536', '-1', '1.5', '80x', '1e2', '0x50', '', '*']) assert.throws(() => helpers.networkPort(port));
  assert.equal(helpers.networkPort('65535'), 65535);
  assert.deepEqual(helpers.parseNetworkRule('10.7.0.0/16', 'UDP', '5000-5010'), { source: '10.7.0.0/16', proto: 'udp', port: '5000-5010' });
  assert.equal(helpers.parseNetworkRule('*', 'tcp', '*').port, '*');
  assert.equal(helpers.parseNetworkRule('*', '*', 'ignored').port, '*');
  assert.equal(helpers.parseNetworkRule('::1', '/pvn/Tunnel/1.0', 'invalid').port, '*');
  for (const source of ['999.1.1.1', '1.2.3', 'localhost', 'http://1.2.3.4', '1.2.3.4/33', '::1/129', '1.2.3.4/-1', '1.2.3.4/2/3', '01.2.3.4']) assert.throws(() => helpers.parseNetworkRule(source, 'tcp', '80'));
  for (const port of ['100-99', '1-2-3', '1.0', '1x', '0-10']) assert.throws(() => helpers.parseNetworkRule('*', 'tcp', port));
  for (const proto of ['http', '/', '/foo bar']) assert.throws(() => helpers.parseNetworkRule('*', proto, '80'));
});

test('batch parses whitespace and English/Chinese commas, preserves physical error line', () => {
  assert.equal(helpers.parseNetworkBatch('\n* tcp 3389\n10.7.0.0/16，udp，5000-5010\n* , /pvn/tunnel/1.0.0 , *\n', 'rules').length, 3);
  assert.throws(() => helpers.parseNetworkBatch('\n* tcp 80\n\n* udp 99999', 'rules'), /第 4 行/);
  assert.throws(() => helpers.parseNetworkBatch('* tcp\n', 'rules'), /第 1 行.*3 列/);
  assert.deepEqual(helpers.parseNetworkBatch(' \n\t', 'rules'), []);
});

test('IPv6 forwarding is bracketed; invalid hosts and duplicate listeners are rejected', () => {
  assert.deepEqual(helpers.parseNetworkForward('8080', '2001:db8::1', '80'), { listen: 8080, target: '[2001:db8::1]:80' });
  assert.equal(helpers.parseNetworkForward('8080', '[::1]', '80').target, '[::1]:80');
  for (const host of ['example.com', '256.0.0.1', '::xyz', '::1:80:wrong', '127.1', '1.2.3.4/path']) assert.throws(() => helpers.parseNetworkForward('80', host, '80'));
  assert.throws(() => helpers.parseNetworkBatch('80 1.2.3.4 90\n80 ::1 99', 'forwards'), /第 2 行.*重复/);
  assert.throws(() => helpers.parseNetworkBatch('80 ::1 90', 'forwards', [{ listen: 80, target: '1.2.3.4:90' }]), /第 1 行.*重复/);
});

test('rules are deduplicated against batch and stored rules, protocol labels stay distinct', () => {
  const existing = [{ source: '*', proto: 'tcp', port: '80' }];
  const result = helpers.parseNetworkBatch('* TCP 80\n* udp 80\n* udp 80\n* /app invalid\n* /app *', 'rules', existing);
  assert.equal(result.length, 2);
  assert.deepEqual(result.map(r => helpers.networkProtocolLabel(r.proto)), ['UDP', '应用流 /app']);
  assert.equal(helpers.networkProtocolLabel('*'), '全部协议');
  assert.equal(helpers.networkProtocolLabel(), 'TCP');
});

function harness(fetchImpl) {
  const elements = new Map();
  const makeElement = () => ({ value: '', textContent: '', innerHTML: '', disabled: false, style: {}, children: {}, setAttribute() {}, focus() {}, querySelector(selector) { return this.children[selector] ||= makeElement(); } });
  for (const id of ['rSrc', 'rProto', 'rPort', 'fwMode', 'fwBatch', 'fListen', 'fTargetHost', 'fTargetPort', 'fwdBatch', 'ruleList', 'fwdList', 'bRules', 'bForwards', 'button']) elements.set(id, makeElement());
  const controls = ['rSrc', 'rProto', 'rPort', 'fwMode', 'fwBatch', 'fListen', 'fTargetHost', 'fTargetPort', 'fwdBatch', 'button'].map(id => elements.get(id));
  const requests = [];
  const context = vm.createContext({ URL, console,
    S: { mode: 'allow-list', rules: [], forwards: [], info: { name: 'unchanged' } },
    $: id => elements.get(id), esc: value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('>', '&gt;').replaceAll('"', '&quot;'),
    document: { querySelectorAll: () => controls, createElement: makeElement, body: { appendChild(el) { elements.set(el.id, el); } } },
    fetch: async (url, options = {}) => { const body = options.body === undefined ? undefined : JSON.parse(options.body); requests.push({ url, options, body }); return fetchImpl ? fetchImpl(url, body, options) : { ok: true, json: async () => body }; }
  });
  vm.runInContext(fs.readFileSync(filename, 'utf8'), context, { filename });
  elements.get('fwMode').value = 'allow-list';
  elements.get('rProto').value = 'tcp';
  return { context, elements, controls, requests };
}

test('invalid batches send no request and keep all drafts; valid batch sends one atomic PUT', async () => {
  const h = harness();
  const draft = '* tcp 80\n* udp 99999';
  h.elements.get('fwBatch').value = draft;
  assert.equal(await h.context.addRulesBatch(), false);
  assert.equal(h.requests.length, 0);
  assert.equal(h.elements.get('fwBatch').value, draft);
  assert.match(h.elements.get('networkRulesError').querySelector('.modal-body').textContent, /第 2 行/);
  h.elements.get('fwBatch').value = '* tcp 80\n* udp 90\n* tcp 80';
  assert.equal(await h.context.addRulesBatch(), true);
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].options.method, 'PUT');
  assert.equal(h.requests[0].body.rules.length, 2);
  assert.equal(h.context.S.rules.length, 2);
  assert.equal(h.elements.get('bRules').textContent, 2);
  assert.equal(h.elements.get('fwBatch').value, '');
  assert.match(h.elements.get('ruleList').innerHTML, /TCP.*UDP/);
  assert.equal(h.context.networkRulesRevision, 1);
});

test('server error retains input and original S, restores controls and shows centered modal', async () => {
  const h = harness(() => ({ ok: false, status: 400, json: async () => ({ error: '<backend refused>' }) }));
  h.elements.get('fwdBatch').value = '8080 ::1 80';
  h.elements.get('rPort').disabled = true;
  const old = JSON.stringify(h.context.S);
  assert.equal(await h.context.addForwardsBatch(), false);
  assert.equal(JSON.stringify(h.context.S), old);
  assert.equal(h.elements.get('fwdBatch').value, '8080 ::1 80');
  assert.equal(h.context.networkRulesRevision, 0);
  assert.equal(h.context.networkRulesBusy, false);
  assert.equal(h.elements.get('rPort').disabled, true);
  assert.equal(h.elements.get('fListen').disabled, false);
  const modal = h.elements.get('networkRulesError');
  assert.equal(modal.className, 'overlay');
  assert.equal(modal.style.display, 'flex');
  assert.equal(modal.querySelector('.modal-body').textContent, '<backend refused>');
});

test('mutex guards both panels until response; state is server-authoritative and revision advances at commit', async () => {
  let resolve;
  const pending = new Promise(r => { resolve = r; });
  const h = harness(() => pending);
  h.elements.get('rSrc').value = '*';
  h.elements.get('rPort').value = '80';
  const write = h.context.addRule();
  assert.equal(h.context.networkRulesBusy, true);
  assert.equal(h.context.S.rules.length, 0);
  assert.equal(h.context.networkRulesRevision, 0);
  assert.ok(h.controls.every(el => el.disabled));
  assert.equal(await h.context.addForward(), false);
  assert.equal(await h.context.deleteRule(0), false);
  assert.equal(await h.context.saveFirewall(), false);
  assert.equal(h.requests.length, 1);
  resolve({ ok: true, json: async () => ({ mode: 'allow-all', rules: [{ source: '10.0.0.1', proto: 'udp', port: '90' }] }) });
  assert.equal(await write, true);
  assert.equal(h.context.S.mode, 'allow-all');
  assert.equal(h.context.S.rules[0].source, '10.0.0.1');
  assert.equal(h.context.S.info.name, 'unchanged');
  assert.equal(h.elements.get('rSrc').value, '');
  assert.equal(h.elements.get('rPort').value, '');
  assert.equal(h.context.networkRulesRevision, 1);
  assert.equal(h.context.networkRulesBusy, false);
  assert.ok(h.controls.every(el => !el.disabled));
});

test('deletion and mode changes PUT immediately, failed mode switch restores selector', async () => {
  const h = harness();
  h.context.S.rules = [{ source: '*', proto: 'tcp', port: '80' }];
  assert.equal(await h.context.deleteRule(0), true);
  h.elements.get('fwMode').value = 'deny-all';
  assert.equal(await h.context.saveFirewall(), true);
  assert.equal(h.context.S.mode, 'deny-all');
  h.context.S.forwards = [{ listen: 80, target: '[::1]:80' }];
  assert.equal(await h.context.deleteForward(0), true);
  assert.deepEqual(h.requests.map(r => r.url), ['/api/firewall', '/api/firewall', '/api/forwards']);
  const failed = harness(() => { throw new Error('offline'); });
  failed.elements.get('fwMode').value = 'deny-all';
  assert.equal(await failed.context.saveFirewall(), false);
  assert.equal(failed.elements.get('fwMode').value, 'allow-list');
});

function pollingHarness(fetchImpl) {
  const h = harness(fetchImpl);
  for (const id of ['nodeTitle', 'addrLine', 'myPeerID', 'peerLine', 'members']) {
    h.elements.set(id, { textContent: '', innerHTML: '', style: {} });
  }
  // Only unrelated panels are stubbed; network renderers and PUTs remain real.
  for (const name of ['renderMembers', 'renderConnect', 'renderSeed', 'renderPublicDHT', 'renderNearby']) h.context[name] = () => {};
  const start = html.indexOf('async function fetchState(');
  const end = html.indexOf('// ----', start);
  assert.ok(start >= 0 && end > start, 'extract actual fetchState through the next section marker');
  vm.runInContext(html.slice(start, end), h.context, { filename: htmlFilename });
  return h;
}

function pollState() {
  return { mode: 'deny-all', rules: [], forwards: [], info: { name: 'new info', peer_id: 'peer-id', virtual_ip: '10.0.0.2' } };
}

function deferred() {
  let resolve;
  const promise = new Promise(r => { resolve = r; });
  return { promise, resolve };
}

for (const kind of ['rules', 'forwards']) {
  test(`actual fetchState preserves ${kind} when GET starts before a successful PUT`, async () => {
    const get = deferred();
    const h = pollingHarness((url, body) => url === '/api/state' ? get.promise : { ok: true, json: async () => body });
    const signal = new AbortController().signal;
    const poll = h.context.fetchState(signal);
    assert.equal(h.requests[0].url, '/api/state');
    assert.equal(h.requests[0].options.signal, signal);
    assert.equal(h.requests[0].body, undefined);
    if (kind === 'rules') h.elements.get('fwBatch').value = '* tcp 80';
    else h.elements.get('fwdBatch').value = '8080 ::1 80';
    assert.equal(await h.context[kind === 'rules' ? 'addRulesBatch' : 'addForwardsBatch'](), true);
    assert.equal(h.requests[1].options.method, 'PUT');
    assert.equal(h.context.networkRulesBusy, false);
    assert.equal(h.context.networkRulesRevision, 1);
    const committed = ['mode', 'rules', 'forwards'].map(key => h.context.S[key]);
    get.resolve({ ok: true, json: async () => pollState() });
    await poll;
    for (const [index, key] of ['mode', 'rules', 'forwards'].entries()) assert.equal(h.context.S[key], committed[index], key);
    assert.equal(h.context.S[kind].length, 1);
    assert.equal(h.context.S.info.name, 'new info');
    assert.equal(h.elements.get('nodeTitle').textContent, 'new info');
    assert.equal(h.elements.get(kind === 'rules' ? 'bRules' : 'bForwards').textContent, 1);
    assert.match(h.elements.get(kind === 'rules' ? 'ruleList' : 'fwdList').innerHTML, kind === 'rules' ? /TCP/ : /8080/);
  });
}

test('actual fetchState while PUT is busy preserves network state without rebuilding disabled buttons', async () => {
  const put = deferred();
  const h = pollingHarness(url => url === '/api/state' ? { ok: true, json: async () => pollState() } : put.promise);
  h.context.S.rules = [{ source: '*', proto: 'tcp', port: '80' }];
  h.context.S.forwards = [{ listen: 8080, target: '[::1]:80' }];
  h.context.renderRules();
  h.context.renderForwards();
  const listWrites = { ruleList: 0, fwdList: 0 };
  for (const id of Object.keys(listWrites)) {
    const el = h.elements.get(id);
    let markup = el.innerHTML;
    Object.defineProperty(el, 'innerHTML', { get: () => markup, set(value) { listWrites[id]++; markup = value; } });
  }
  const button = h.elements.get('button');
  h.elements.get('fwBatch').value = '* udp 90';
  h.elements.get('fwdBatch').value = '9090 ::1 90';
  const before = ['mode', 'rules', 'forwards'].map(key => h.context.S[key]);
  const write = h.context.addRulesBatch();
  assert.equal(h.context.networkRulesBusy, true);
  assert.equal(button.disabled, true);
  await h.context.fetchState();
  for (const [index, key] of ['mode', 'rules', 'forwards'].entries()) assert.equal(h.context.S[key], before[index], key);
  assert.equal(h.context.S.info.name, 'new info');
  assert.deepEqual(listWrites, { ruleList: 0, fwdList: 0 });
  assert.equal(h.elements.get('button'), button);
  assert.ok(h.controls.every(el => el.disabled));
  assert.equal(h.elements.get('fwBatch').value, '* udp 90');
  assert.equal(h.elements.get('fwdBatch').value, '9090 ::1 90');
  put.resolve({ ok: true, json: async () => ({ mode: 'allow-list', rules: [{ source: '*', proto: 'udp', port: '90' }] }) });
  assert.equal(await write, true);
  assert.equal(h.context.networkRulesBusy, false);
  assert.equal(button.disabled, false);
});

test('actual network panels have immediate mode/batch bindings and no save or refresh buttons', () => {
  const panels = {};
  for (const id of ['pFirewall', 'pForwards']) {
    const match = html.match(new RegExp(`<div class="panel" id="${id}">([\\s\\S]*?)(?=<div class="panel"|<script)`));
    assert.ok(match, `${id} panel exists`);
    panels[id] = match[1];
    const buttons = [...match[1].matchAll(/<button\b[^>]*>[\s\S]*?<\/button>/g)].map(m => m[0]);
    assert.equal(buttons.length, 2, `${id} has only single and batch add buttons`);
    for (const button of buttons) assert.doesNotMatch(button, /保存|刷新|saveFirewall|saveForwards|loadState|fetchState/i);
  }
  assert.match(panels.pFirewall, /<select\b[^>]*id="fwMode"[^>]*onchange="saveFirewall\(\)"/);
  assert.match(panels.pFirewall, /<textarea\b[^>]*id="fwBatch"/);
  assert.match(panels.pFirewall, /<button\b[^>]*onclick="addRulesBatch\(\)"[^>]*>批量添加<\/button>/);
  assert.match(panels.pForwards, /<textarea\b[^>]*id="fwdBatch"/);
  assert.match(panels.pForwards, /<button\b[^>]*onclick="addForwardsBatch\(\)"[^>]*>批量添加<\/button>/);
});

test('network rejection retains single drafts; duplicate rules are filtered without PUT', async () => {
  const h = harness(() => { throw new Error('connection lost'); });
  h.elements.get('rSrc').value = '*';
  h.elements.get('rPort').value = '80';
  assert.equal(await h.context.addRule(), false);
  assert.equal(h.elements.get('rSrc').value, '*');
  assert.equal(h.elements.get('rPort').value, '80');
  assert.equal(h.context.S.rules.length, 0);
  assert.equal(h.context.networkRulesBusy, false);
  const duplicate = harness();
  duplicate.context.S.rules = [{ source: '*', proto: 'tcp', port: '80' }];
  duplicate.elements.get('rSrc').value = '*';
  duplicate.elements.get('rPort').value = '80';
  assert.equal(await duplicate.context.addRule(), false);
  assert.equal(duplicate.requests.length, 0);
  duplicate.elements.get('fwBatch').value = '* tcp 80\n* TCP 80';
  assert.equal(await duplicate.context.addRulesBatch(), false);
  assert.equal(duplicate.requests.length, 0);
});

test('single additions reject duplicate listeners and preserve drafts on malformed success', async () => {
  const h = harness();
  h.context.S.forwards = [{ listen: 80, target: '1.2.3.4:90' }];
  h.elements.get('fListen').value = '80';
  h.elements.get('fTargetHost').value = '::1';
  h.elements.get('fTargetPort').value = '90';
  assert.equal(await h.context.addForward(), false);
  assert.equal(h.requests.length, 0);
  h.elements.get('fListen').value = '81';
  assert.equal(await h.context.addForward(), true);
  assert.equal(h.requests[0].body.forwards[1].target, '[::1]:90');
  assert.equal(h.elements.get('fListen').value, '');
  const malformed = harness(() => ({ ok: true, json: async () => ({}) }));
  malformed.elements.get('fwBatch').value = '* tcp 80';
  assert.equal(await malformed.context.addRulesBatch(), false);
  assert.equal(malformed.elements.get('fwBatch').value, '* tcp 80');
  assert.equal(malformed.context.networkRulesRevision, 0);
});
