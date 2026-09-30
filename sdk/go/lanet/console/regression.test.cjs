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
console.log('PASS: full script syntax, dual-stack rendering, IPv6 search, IPv4 fallback');
