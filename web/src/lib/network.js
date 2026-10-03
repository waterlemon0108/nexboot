// 页面共用的客户机网段计算：分组地址窗口是否在服务器客户机网段内。
// 与服务端 assets.ClassifyGroupNetwork 一致，表单输入时先判，保存时以服务端为准。

export const ip4ToInt = (s) => {
  const m = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/.exec(String(s || '').trim());
  if (!m) return null;
  const parts = m.slice(1).map(Number);
  if (parts.some(p => p > 255)) return null;
  return ((parts[0] << 24) >>> 0) + (parts[1] << 16) + (parts[2] << 8) + parts[3];
};

export const intToIp4 = (n) => [n >>> 24, (n >>> 16) & 255, (n >>> 8) & 255, n & 255].join('.');

// prefixContains("192.168.50.1/24", "192.168.50.77") → true
export const prefixContains = (prefix, ip) => {
  const [addr, bitsRaw] = String(prefix || '').split('/');
  const a = ip4ToInt(addr), n = ip4ToInt(ip), bits = Number(bitsRaw);
  if (a === null || n === null || !(bits >= 0 && bits <= 32)) return false;
  const mask = bits === 0 ? 0 : (0xffffffff << (32 - bits)) >>> 0;
  return ((a & mask) >>> 0) === ((n & mask) >>> 0);
};

// 分组分配的地址窗口：start .. start+max-1。
export const groupWindow = (startIP, clientMax) => {
  const start = ip4ToInt(startIP), max = Number(clientMax) || 0;
  if (start === null || max <= 0) return null;
  return { start, end: start + max - 1 };
};

// classifyGroupNetwork(form-ish {start_ip, client_max}, networkView) →
// 'same' | 'relay' | 'blocked' | 'unknown'
export const classifyGroupNetwork = ({ start_ip, client_max }, network) => {
  if (!network || !network.known || !(network.client_networks || []).length) return 'unknown';
  const win = groupWindow(start_ip, client_max);
  if (!win) return 'unknown';
  const inside = network.client_networks.some(p => prefixContains(p, intToIp4(win.start)) && prefixContains(p, intToIp4(win.end)));
  if (inside) return 'same';
  return network.allow_cross_subnet ? 'relay' : 'blocked';
};

// 中继部署时交换机侧需要的配置，已填好服务器地址。
export const relaySnippet = (serverAddr) => `请在客户机所在 VLAN 的三层接口上配置 DHCP 中继，指向 ${serverAddr}：
  思科      ip helper-address ${serverAddr}
  华为/H3C  dhcp select relay
            dhcp relay server-ip ${serverAddr}
  Linux     dnsmasq: dhcp-relay=<该网段网关>,${serverAddr}
并保证客户机网段能路由到 ${serverAddr} 的 18080（HTTP）、69（TFTP）、3260（iSCSI）。
分组的「网关」请填该 VLAN 的三层接口地址（即做中继的那台）。`;
