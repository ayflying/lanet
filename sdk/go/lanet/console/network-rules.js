/* Plain browser script: S, $, and esc are supplied by index.html. */
var networkRulesBusy = false;
var networkRulesRevision = 0;

function networkPort(value) {
  const text = String(value).trim();
  if (!/^\d+$/.test(text) || Number(text) < 1 || Number(text) > 65535) {
    throw new Error('端口必须是 1-65535 的整数');
  }
  return Number(text);
}

function networkIP(value) {
  let text = String(value).trim();
  if (text.startsWith('[') && text.endsWith(']')) text = text.slice(1, -1);
  if (/^\d+\.\d+\.\d+\.\d+$/.test(text)) {
    const parts = text.split('.');
    if (parts.some(p => Number(p) > 255 || (p.length > 1 && p[0] === '0'))) throw new Error('IP 地址非法');
    return parts.map(Number).join('.');
  }
  if (text.includes(':') && /^[\da-f:.]+$/i.test(text)) {
    try {
      const host = new URL('http://[' + text + ']/').hostname;
      return host.replace(/^\[|\]$/g, '').toLowerCase();
    } catch (_) { /* Report the same useful validation error below. */ }
  }
  throw new Error('IP 地址非法');
}

function networkSource(value) {
  const text = String(value).trim();
  if (text === '*') return '*';
  const parts = text.split('/');
  if (parts.length > 2) throw new Error('来源必须是 IP、网段或 *');
  const ip = networkIP(parts[0]);
  if (parts.length === 1) return ip;
  const bits = parts[1];
  if (!/^\d+$/.test(bits) || Number(bits) > (ip.includes(':') ? 128 : 32)) throw new Error('来源网段前缀非法');
  return ip + '/' + Number(bits);
}

function parseNetworkRule(source, proto, port) {
  source = networkSource(source);
  proto = String(proto).trim();
  if (!proto.startsWith('/')) proto = proto.toLowerCase();
  if (!['tcp', 'udp', '*'].includes(proto) && !/^\/[^\s,，]+$/.test(proto)) throw new Error('协议必须是 tcp、udp、* 或 / 开头的应用协议');
  if (proto === '*' || proto.startsWith('/')) return { source, proto, port: '*' };
  port = String(port).trim();
  if (port !== '*') {
    const parts = port.split('-');
    if (parts.length > 2) throw new Error('端口范围非法');
    const start = networkPort(parts[0]);
    const end = parts.length === 2 ? networkPort(parts[1]) : start;
    if (start > end) throw new Error('端口范围起点不能大于终点');
    port = parts.length === 2 ? start + '-' + end : String(start);
  }
  return { source, proto, port };
}

function parseNetworkForward(listen, host, port) {
  listen = networkPort(listen);
  host = networkIP(host);
  port = networkPort(port);
  return { listen, target: (host.includes(':') ? '[' + host + ']' : host) + ':' + port };
}

function networkRuleKey(rule) {
  return JSON.stringify([rule.source, rule.proto || 'tcp', String(rule.port)]);
}

function parseNetworkBatch(text, kind, existing) {
  const result = [];
  const seen = new Set((existing || []).map(kind === 'rules' ? networkRuleKey : f => Number(f.listen)));
  String(text).split(/\r?\n/).forEach((line, index) => {
    if (!line.trim()) return;
    try {
      const columns = line.trim().split(/[\s,，]+/);
      if (columns.length !== 3) throw new Error('每行必须包含 3 列');
      const item = kind === 'rules' ? parseNetworkRule(...columns) : parseNetworkForward(...columns);
      const key = kind === 'rules' ? networkRuleKey(item) : item.listen;
      if (seen.has(key)) {
        if (kind === 'rules') return;
        throw new Error('本地监听端口重复：' + item.listen);
      }
      seen.add(key);
      result.push(item);
    } catch (error) {
      throw new Error('第 ' + (index + 1) + ' 行：' + error.message);
    }
  });
  return result;
}

function networkProtocolLabel(proto) {
  if (!proto || proto === 'tcp') return 'TCP';
  if (proto === 'udp') return 'UDP';
  if (proto === '*') return '全部协议';
  return '应用流 ' + proto;
}

function networkRulesError(message) {
  // askConfirm is reserved for destructive confirmations; do not steal its callback.
  let overlay = $('networkRulesError');
  if (!overlay) {
    overlay = document.createElement('div');
    overlay.id = 'networkRulesError';
    overlay.className = 'overlay';
    overlay.style.zIndex = '80';
    overlay.setAttribute('role', 'dialog');
    overlay.setAttribute('aria-modal', 'true');
    overlay.setAttribute('aria-labelledby', 'networkRulesErrorTitle');
    overlay.innerHTML = '<div class="modal" style="max-width:460px"><h2 id="networkRulesErrorTitle">操作失败</h2><div class="modal-body"></div><div class="modal-btns"><button type="button">知道了</button></div></div>';
    overlay.querySelector('button').onclick = () => { overlay.style.display = 'none'; };
    document.body.appendChild(overlay);
  }
  overlay.querySelector('.modal-body').textContent = message;
  overlay.style.display = 'flex';
  overlay.querySelector('button').focus();
}

function networkControls() {
  return Array.from(document.querySelectorAll('#pFirewall input, #pFirewall select, #pFirewall button, #pFirewall textarea, #pForwards input, #pForwards select, #pForwards button, #pForwards textarea'));
}

async function networkRulesUpdate(kind, build, clear) {
  // One mutex covers BOTH panels, including programmatic calls, from snapshot/build
  // through response commit. No optimistic S mutation and no follow-up GET.
  // fetchState must capture networkRulesRevision BEFORE its GET and preserve the
  // current S.mode/rules/forwards if busy OR that revision changed on response.
  if (networkRulesBusy) return false;
  let body;
  try {
    if (!S) throw new Error('状态尚未加载，请稍后重试');
    body = build();
    if (!body) return false;
  } catch (error) {
    if ($('fwMode') && S) $('fwMode').value = S.mode;
    networkRulesError(error.message);
    return false;
  }
  networkRulesBusy = true;
  const disabled = new Map(networkControls().map(el => [el, el.disabled]));
  disabled.forEach((_, el) => { el.disabled = true; });
  try {
    const response = await fetch(kind === 'rules' ? '/api/firewall' : '/api/forwards', {
      method: 'PUT', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body)
    });
    let state;
    try { state = await response.json(); } catch (_) { throw new Error('服务器返回了无效响应（HTTP ' + response.status + '）'); }
    if (!response.ok) throw new Error(state.error || state.message || '请求失败（HTTP ' + response.status + '）');
    if (!state || (kind === 'rules' ? typeof state.mode !== 'string' || (state.rules != null && !Array.isArray(state.rules)) || !Object.prototype.hasOwnProperty.call(state, 'rules') : !Array.isArray(state.forwards))) {
      throw new Error('服务器返回的网络规则状态不完整');
    }
    if (kind === 'rules') { S.mode = state.mode; S.rules = state.rules || []; }
    else S.forwards = state.forwards;
    // Commit and revision advance are synchronous: polling can never observe a
    // successful write with the old revision. Errors leave both state and draft intact.
    networkRulesRevision++;
    renderRules();
    renderForwards();
    if (clear) clear();
    return true;
  } catch (error) {
    networkRulesError(error.message || String(error));
    return false;
  } finally {
    networkRulesBusy = false;
    if ($('fwMode') && S) $('fwMode').value = S.mode;
    networkControls().forEach(el => { el.disabled = disabled.has(el) ? disabled.get(el) : false; });
  }
}

function renderRules() {
  const rules = (S && S.rules) || [];
  $('bRules').textContent = rules.length;
  $('ruleList').innerHTML = rules.length ? '<table><tr><th>来源</th><th>协议</th><th>端口</th><th></th></tr>' + rules.map((r, i) => '<tr><td>' + esc(r.source) + '</td><td>' + esc(networkProtocolLabel(r.proto)) + '</td><td>' + esc(r.port) + '</td><td><button class="del" onclick="deleteRule(' + i + ')"' + (networkRulesBusy ? ' disabled' : '') + '>删除</button></td></tr>').join('') + '</table>' : '<div class="empty" style="margin:8px 0">暂无放行规则</div>';
}

function renderForwards() {
  const forwards = (S && S.forwards) || [];
  $('bForwards').textContent = forwards.length;
  $('fwdList').innerHTML = forwards.length ? '<table><tr><th>本地端口</th><th>目标 IP</th><th>目标端口</th><th></th></tr>' + forwards.map((f, i) => {
    const split = String(f.target || '').lastIndexOf(':');
    const host = split > 0 ? f.target.slice(0, split) : f.target;
    const port = split > 0 ? f.target.slice(split + 1) : '';
    return '<tr><td>' + esc(f.listen) + '</td><td>' + esc(host) + '</td><td>' + esc(port) + '</td><td><button class="del" onclick="deleteForward(' + i + ')"' + (networkRulesBusy ? ' disabled' : '') + '>删除</button></td></tr>';
  }).join('') + '</table>' : '<div class="empty" style="margin:8px 0">暂无转发映射</div>';
}

function networkClear(ids) { ids.forEach(id => { $(id).value = ''; }); }
function addRule() {
  return networkRulesUpdate('rules', () => {
    const rule = parseNetworkRule($('rSrc').value, $('rProto').value, $('rPort').value);
    if ((S.rules || []).some(r => networkRuleKey(r) === networkRuleKey(rule))) return null;
    return { mode: S.mode, rules: (S.rules || []).concat(rule) };
  }, () => networkClear(['rSrc', 'rPort']));
}
function addRulesBatch() {
  return networkRulesUpdate('rules', () => {
    const rules = parseNetworkBatch($('fwBatch').value, 'rules', S.rules);
    return rules.length ? { mode: S.mode, rules: (S.rules || []).concat(rules) } : null;
  }, () => networkClear(['fwBatch']));
}
function addForward() {
  return networkRulesUpdate('forwards', () => {
    const forward = parseNetworkForward($('fListen').value, $('fTargetHost').value, $('fTargetPort').value);
    if ((S.forwards || []).some(f => Number(f.listen) === forward.listen)) throw new Error('本地监听端口重复：' + forward.listen);
    return { forwards: (S.forwards || []).concat(forward) };
  }, () => networkClear(['fListen', 'fTargetHost', 'fTargetPort']));
}
function addForwardsBatch() {
  return networkRulesUpdate('forwards', () => {
    const forwards = parseNetworkBatch($('fwdBatch').value, 'forwards', S.forwards);
    return forwards.length ? { forwards: (S.forwards || []).concat(forwards) } : null;
  }, () => networkClear(['fwdBatch']));
}
function deleteRule(index) {
  return networkRulesUpdate('rules', () => ({ mode: S.mode, rules: (S.rules || []).filter((_, i) => i !== index) }));
}
function deleteForward(index) {
  return networkRulesUpdate('forwards', () => ({ forwards: (S.forwards || []).filter((_, i) => i !== index) }));
}
function saveFirewall() {
  return networkRulesUpdate('rules', () => {
    const mode = $('fwMode').value;
    if (!['deny-all', 'allow-list', 'allow-all'].includes(mode)) throw new Error('防火墙模式非法');
    return { mode, rules: (S.rules || []).slice() };
  });
}
function saveForwards() {
  return networkRulesUpdate('forwards', () => ({ forwards: (S.forwards || []).slice() }));
}

if (typeof module !== 'undefined' && module.exports) {
  module.exports = { networkPort, networkIP, networkSource, parseNetworkRule, parseNetworkForward, parseNetworkBatch, networkRuleKey, networkProtocolLabel };
}
