const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const path = require('node:path');
const html = fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8');
const script = html.match(/<script>([\s\S]*?)<\/script>/)[1];
new vm.Script(script); // Parse the entire embedded script, not just the exercised functions.
function extract(start, end) { return script.slice(script.indexOf(start), script.indexOf(end, script.indexOf(start))); }
const elements = {};
const el = id => elements[id] ||= { value: '', style: {}, textContent: '', innerHTML: '' };
const ctx = vm.createContext({ $: el, esc: s => String(s).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;'), relTime: () => '刚刚', mdLite: s => s, Date });
vm.runInContext('let S; let updInfo;\n' + extract('const M_PAGE_SIZE', 'function switchTab') + extract('function fmtCheckedAt', 'async function onUpdateClick'), ctx);
vm.runInContext(`S = {members: [{peer_id:'peer1', virtual_ip:'10.7.1.2', virtual_ipv6:'fd00:6c61:6e65::1234', online:true}]}; renderMembers();`, ctx);
assert.match(el('members').innerHTML, /10\.7\.1\.2/);
assert.match(el('members').innerHTML, /fd00:6c61:6e65::1234/);
assert.match(el('members').innerHTML, /class="m-ipv6"/);
el('mSearch').value = 'FD00:6C61';
assert.equal(vm.runInContext('filteredMembers().length', ctx), 1);
el('mSearch').value = '';
vm.runInContext('delete S.members[0].virtual_ipv6; renderMembers();', ctx);
assert.doesNotMatch(el('members').innerHTML, /undefined|class="m-ipv6"/);
vm.runInContext(extract('function seedSpecCheck', '// bootstrapPasteHint') + extract('function copySeedMine', '// ---- 连接种子') + extract('function renderSeed', 'let cfg'), ctx);
const seeds = ['/ip4/43.136.124.167/tcp/4001/p2p/12D3KooWD1RmFbKp7sEmmeRepvQnfpZcLRxQXXGabin21k5zm8Kf', '/ip4/43.136.124.167/udp/4001/quic-v1/p2p/12D3KooWD1RmFbKp7sEmmeRepvQnfpZcLRxQXXGabin21k5zm8Kf'];
ctx.autoGrow = () => {}; ctx.flash = () => {}; let copied;
ctx.copyText = text => { copied = text; };
ctx.seeds = seeds;
vm.runInContext('S.seed_addrs = seeds; renderSeed(); copySeedMine();', ctx);
assert.equal(copied, el('seedMine').value);
assert.match(html, /<textarea id="cBootstrap"/);
for (const separator of ['\n', '\r\n', ' ', '\t', ',']) {
  el('cBootstrap').value = seeds.join(separator);
  assert.equal(vm.runInContext("seedSpecCheck($('cBootstrap').value)", ctx), '');
  const submission = script.match(/bootstrap: ([^\n]+),/)[1];
  assert.equal(vm.runInContext(submission, ctx), seeds.join(','));
}
console.log('PASS: script syntax, IPv6 rendering/search, seed display-copy-paste-submit');
