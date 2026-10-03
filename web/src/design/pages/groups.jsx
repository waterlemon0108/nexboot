// 分组绑定系统镜像 + 配置 + 还原点及 DHCP 网络参数；数据盘是分组下的列表（挂载点 + 镜像 + 配置）。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, Select, SearchBox, TableState, Toggle } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, useMutation } from '../../lib/hooks.js';
import { classifyGroupNetwork, groupWindow, intToIp4, relaySnippet } from '../../lib/network.js';

// 新建分组预填的公共 DNS，放在输入框里，有内网 DNS 的现场可直接改。
// 国内的放第一位：Windows 先问 DNS1、超时才问 DNS2，8.8.8.8 在国内常超时。
// 不能留空：dnsmasq 这里 port=0 不做解析，客户机拿不到 DNS 就解析不了任何域名。
const DEFAULT_DNS1 = "114.114.114.114";
const DEFAULT_DNS2 = "223.5.5.5";
const { useState, useEffect } = React;

// 各种网络状态在卡片和表单里的叫法。
const NET_STATUS = {
  same:    { chip: "emerald", text: "同网段" },
  relay:   { chip: "amber",   text: "跨网段·中继" },
  blocked: { chip: "rose",    text: "不通" },
  unknown: { chip: "",        text: "" },
};
const rangeText = (startIP, max) => { const w = groupWindow(startIP, max); return w ? `${intToIp4(w.start)}–${intToIp4(w.end)}` : startIP || "—"; };

const ipRangeEnd = (startIP, max) => {
  const m = /^(.*\.)(\d+)$/.exec(String(startIP || ""));
  if (!m || !max) return startIP || "—";
  return m[1] + (parseInt(m[2], 10) + Math.max(0, max - 1));
};
const asItems = (r) => (Array.isArray(r) ? r : (r && Array.isArray(r.items) ? r.items : []));

function PageGroups() {
  const store = useStore();
  const conf = useConfirm();
  const { route, goto } = store;

  const [search, setSearch] = useState("");
  const [modal, setModal] = useState(null); // add | edit | disks | fixnet
  const [form, setForm] = useState({});
  const [disksGroup, setDisksGroup] = useState(null);
  const [fix, setFix] = useState(null); // { group, target, preview }
  // 服务器的客户机网段：默认值取自它，所有地址区间都对照它判断。单独获取，失败只影响判断，不影响页面。
  const { data: network, reload: reloadNetwork } = useResource(() => api.getNetwork(30).catch(() => null), []);
  // 存储节点下拉用的集群名单；单机或获取失败时整个下拉隐藏。
  const { data: nodesData } = useResource(() => api.listClusterNodes().catch(() => null), []);
  const clusterNodes = (nodesData && nodesData.items) || [];

  // 分开 settle：镜像、客户机信息获取失败不能清空分组列表。
  const { data, loading, error, reload: load } = useResource(async () => {
    const [gr, ir, tr] = await Promise.allSettled([api.listGroups(), api.listImages(), api.listTerminals()]);
    if (gr.status === "rejected") throw gr.reason;
    const groups = (gr.value && gr.value.items) || [];
    // 把配置、还原点的原始 ID（带纳秒后缀，操作者看不懂）换成名称，查不到就显示 ID。
    const imageIds = [...new Set(groups.map(g => g.SystemImageID).filter(Boolean))];
    const configIds = [...new Set(groups.map(g => g.SystemConfigID).filter(Boolean))];
    const [cfgResults, redResults] = await Promise.all([
      Promise.allSettled(imageIds.map(id => api.listConfigs(id))),
      Promise.allSettled(configIds.map(id => api.listReductions(id))),
    ]);
    const names = {};
    for (const r of [...cfgResults, ...redResults]) {
      if (r.status !== "fulfilled") continue;
      for (const item of (r.value && r.value.items) || []) names[item.ID] = item.DisplayName || item.Name;
    }
    return {
      groups,
      images: ir.status === "fulfilled" ? (ir.value && ir.value.items) || [] : [],
      terminals: tr.status === "fulfilled" ? (tr.value && tr.value.items) || [] : [],
      names,
    };
  }, []);
  const groups = (data && data.groups) || [];
  const images = (data && data.images) || [];
  const terminals = (data && data.terminals) || [];
  const nameOf = (id) => (data && data.names && data.names[id]) || id || "—";
  const { busy, run } = useMutation(store.toast);

  const imageById = Object.fromEntries(images.map(i => [i.ID, i]));
  const filtered = groups.filter(g => !search ||
    g.Name.toLowerCase().includes(search.toLowerCase()) || (g.StartIP || "").includes(search));

  const termsOf = (gid) => terminals.filter(t => t.GroupID === gid);
  const onlineOf = (gid) => termsOf(gid).filter(t => t.State === "online").length;

  const openAdd = () => {
    // 默认值取自服务器的客户机网段，不能写死在代码里，否则分组会落到没人服务的网段。
    const sug = network?.suggest;
    setForm({ name: "", start_ip: sug?.start_ip || "", client_max: sug?.client_max || 30, gateway: sug?.gateway || "", netmask: sug?.netmask || "255.255.255.0", dns1: DEFAULT_DNS1, dns2: DEFAULT_DNS2, is_default: false, system_image_id: "", system_config_id: "", storage_server_id: "" });
    setModal("add");
  };
  // fillSuggested 按表单要的台数，在客户机网段内换一个可用区间。
  const fillSuggested = async () => {
    try {
      const view = await api.getNetwork(Number(form.client_max) || 30);
      if (!view?.suggest) { store.toast("服务器网段里放不下这么多台，缩小客户机数再试", "err"); return; }
      setForm(f => ({ ...f, start_ip: view.suggest.start_ip, netmask: view.suggest.netmask, gateway: view.suggest.gateway || "" }));
    } catch (e) { store.toast(e.message || "读取服务器网络失败", "err"); }
  };
  const netStatus = classifyGroupNetwork(form, network);
  const openEdit = (g) => {
    setForm({ id: g.ID, name: g.Name, start_ip: g.StartIP, client_max: g.ClientMax, gateway: g.Gateway, netmask: g.Netmask, dns1: g.DNS1, dns2: g.DNS2, is_default: g.IsDefault, system_image_id: g.SystemImageID, system_config_id: g.SystemConfigID, storage_server_id: g.StorageServerID || "" });
    setModal("edit");
  };
  const save = async () => {
    if (!form.name?.trim()) { store.toast("请输入分组名称", "err"); return; }
    if (netStatus === "blocked") { store.toast("网段不在服务器的客户机网段，先改到服务器网段或打开跨网段分组", "err"); return; }
    if (!form.system_image_id || !form.system_config_id) { store.toast("请选择镜像 / 配置", "err"); return; }
    const body = {
      name: form.name.trim(), is_default: !!form.is_default,
      start_ip: form.start_ip?.trim(), client_max: +form.client_max || 0,
      gateway: form.gateway?.trim(), netmask: form.netmask?.trim(), dns1: form.dns1?.trim() || "", dns2: form.dns2?.trim() || "",
      system_image_id: form.system_image_id, system_config_id: form.system_config_id,
      storage_server_id: form.storage_server_id || "",
    };
    if (modal === "add") {
      if (await run(() => api.createGroup(body), "分组已创建", { reload: load, errMsg: "保存失败" })) setModal(null);
      return;
    }
    // 改网络参数前先预览影响：区间一挪，组内机器地址跟着换，运行中的机器续租时
    // dnsmasq 不再给原地址，Windows 的 IP 一变 iSCSI 就断，要在保存前告诉运维。
    const before = groups.find(g => g.ID === form.id);
    const moved = before && (before.StartIP !== body.start_ip || before.ClientMax !== body.client_max
      || (before.Netmask || "") !== (body.netmask || "") || (before.Gateway || "") !== (body.gateway || ""));
    if (moved) {
      try {
        const preview = await api.previewGroupNetwork(form.id, {
          start_ip: body.start_ip, client_max: body.client_max, netmask: body.netmask, gateway: body.gateway,
        });
        if ((preview.terminals || []).length) {
          setFix({ group: before, target: body, preview, body });
          setModal("fixnet");
          return;
        }
      } catch (e) {
        store.toast(e.message || "无法预览地址变化", "err");
        return;
      }
    }
    if (await run(() => api.updateGroup(form.id, body), "分组已更新", { reload: load, errMsg: "保存失败" })) setModal(null);
  };
  const onDelete = async (g) => {
    const n = termsOf(g.ID).length;
    const message = n > 0
      ? `分组 ${g.Name} 下还有 ${n} 台终端，删除会失败。\n请先把终端移动到其他分组或删除。`
      : `删除分组 ${g.Name}？`;
    const ok = await conf.ask({ title: "删除分组", message, danger: true });
    if (!ok) return;
    await run(() => api.deleteGroup(g.ID), "分组已删除", { reload: load, errMsg: "删除失败" });
  };
  const onSetDefault = (g) => run(() => api.setDefaultGroup(g.ID), `${g.Name} 已设为默认分组`, { reload: load });
  const netStatusOf = (g) => (network?.groups || []).find(x => x.id === g.ID)?.status || classifyGroupNetwork({ start_ip: g.StartIP, client_max: g.ClientMax }, network);
  // openFix 算出分组要挪到的区间及对组内机器的影响，写入前先展示。
  const openFix = async (g) => {
    try {
      const view = await api.getNetwork(g.ClientMax || 30);
      if (!view?.suggest) { store.toast("服务器网段里放不下这么多台，先缩小该组的客户机数", "err"); return; }
      const target = { start_ip: view.suggest.start_ip, client_max: g.ClientMax, netmask: view.suggest.netmask, gateway: view.suggest.gateway || "" };
      const preview = await api.previewGroupNetwork(g.ID, target);
      setFix({ group: g, target, preview });
      setModal("fixnet");
    } catch (e) { store.toast(e.message || "无法预览", "err"); }
  };
  const applyFix = async () => {
    const g = fix.group, t = fix.target;
    // 表单来的改动带完整 body（名称、镜像可能一起改了）；「改到服务器网段」只改网络部分，其余照旧。
    const body = fix.body || { name: g.Name, is_default: !!g.IsDefault, start_ip: t.start_ip, client_max: t.client_max, gateway: t.gateway, netmask: t.netmask, dns1: g.DNS1 || "", dns2: g.DNS2 || "", system_image_id: g.SystemImageID, system_config_id: g.SystemConfigID };
    if (await run(() => api.updateGroup(g.ID, body), `${g.Name} 已改到 ${rangeText(t.start_ip, t.client_max)}`, { reload: () => { load(); reloadNetwork(); }, errMsg: "修改失败" })) { setModal(null); setFix(null); }
  };

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div className="card" style={{ padding: 14, display: "flex", alignItems: "center", gap: 12 }}>
        <SearchBox value={search} onChange={setSearch} placeholder="搜索分组名称 / IP 段" width={280}/>
        <span className="chip">{groups.length} 分组</span>
        <span className="chip emerald">{groups.reduce((s, g) => s + onlineOf(g.ID), 0)} 在线</span>
        <span className="chip">{groups.reduce((s, g) => s + (g.ClientMax || 0), 0)} 终端容量</span>
        <div style={{ marginLeft: "auto", display: "flex", gap: 8 }}>
          <button className="btn ghost icon" onClick={() => load()} title="立即刷新" aria-label="立即刷新"><Icons.Refresh size={13}/></button>
          <button className="btn primary" onClick={openAdd}><Icons.Plus size={12}/> 新建分组</button>
        </div>
      </div>

      {!filtered.length && (
        <div className="card" style={{ padding: 40, textAlign: "center", color: "var(--fg-faint)", fontSize: 13 }}>
          {loading ? "加载中…" : error ? `加载失败：${error}` : "暂无分组"}
        </div>
      )}

      {/* 用 auto-fill 不用 auto-fit：auto-fit 会折叠空轨道，只有一个分组时卡片会撑满整行。 */}
      <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fill, minmax(430px, 1fr))", gap: 14 }}>
        {filtered.map(g => {
          const online = onlineOf(g.ID), cap = g.ClientMax || 0;
          const pct = cap ? (online / cap) * 100 : 0;
          const focused = route.params.groupId === g.ID;
          const img = imageById[g.SystemImageID];
          return (
            <div key={g.ID} className="card" style={{ padding: 0, overflow: "hidden", borderColor: focused ? "var(--cyan)" : undefined }}>
              <div style={{ padding: 16, borderBottom: "1px solid var(--line-soft)", display: "flex", alignItems: "center", gap: 12 }}>
                <div style={{ width: 40, height: 40, borderRadius: 8, background: "var(--cyan-soft)", border: "1px solid oklch(0.55 0.1 200 / 0.4)", display: "grid", placeItems: "center", color: "var(--cyan)", flexShrink: 0 }}>
                  <Icons.Group size={20}/>
                </div>
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
                    <span style={{ fontSize: 14, fontWeight: 600 }}>{g.Name}</span>
                    {g.IsDefault && <span className="chip cyan">默认</span>}
                    {/* 不显示分组 ID：机器生成，操作者用不上，名称已能区分。 */}
                  </div>
                  {/* 单行显示，过长截断不换行，避免在词中间折行。 */}
                  <div className="mono meta" style={{ marginTop: 3, whiteSpace: "nowrap", overflow: "hidden", textOverflow: "ellipsis", display: "flex", alignItems: "center", gap: 6 }}>
                    <span>{g.StartIP} → {ipRangeEnd(g.StartIP, g.ClientMax)} · 最多 {g.ClientMax} 台</span>
                    {(() => { const st = NET_STATUS[netStatusOf(g)]; return st?.text ? <span className={`chip ${st.chip}`} title={st.chip === "rose" ? "不在服务器客户机网卡的网段内，这组机器拿不到地址" : ""}>{st.text}</span> : null; })()}
                  </div>
                </div>
                <div style={{ textAlign: "right" }}>
                  <div className="mono" style={{ fontSize: 18, fontWeight: 600 }}>
                    <span style={{ color: pct > 90 ? "var(--emerald)" : "var(--cyan)" }}>{online}</span>
                    <span style={{ color: "var(--fg-faint)", fontSize: 14 }}> / {cap}</span>
                  </div>
                  <div className="bar emerald" style={{ width: 80, marginTop: 4 }}><span style={{ width: `${pct}%` }}/></div>
                </div>
              </div>

              <div style={{ padding: 16, display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(180px, 1fr))", gap: 14 }}>
                <div>
                  <div className="card-title" style={{ marginBottom: 10 }}><Icons.Network size={12}/> 网络</div>
                  <KV2 k="网关" v={g.Gateway}/><KV2 k="子网掩码" v={g.Netmask}/><KV2 k="DNS" v={[g.DNS1, g.DNS2].filter(Boolean).join(" / ") || "—"}/>
                </div>
                <div>
                  <div className="card-title" style={{ marginBottom: 10 }}><Icons.Layers size={12}/> 系统盘</div>
                  <KV2 k="镜像" v={img?.Name || "—"}/><KV2 k="配置" v={nameOf(g.SystemConfigID)}/><KV2 k="应用还原点" v={nameOf(g.SystemReductionID)}/>
                </div>
              </div>

              <div style={{ padding: "10px 16px", borderTop: "1px solid var(--line-soft)", display: "flex", gap: 8, justifyContent: "flex-end", flexWrap: "wrap" }}>
                <button className="btn" onClick={() => goto("terminals", { groupId: g.ID })}><Icons.Eye size={12}/> {termsOf(g.ID).length} 台终端</button>
                <button className="btn" onClick={() => { setDisksGroup(g); setModal("disks"); }}><Icons.Disk size={12}/> 数据盘</button>
                {netStatusOf(g) === "blocked" && <button className="btn primary" onClick={() => openFix(g)}>改到服务器网段</button>}
                {!g.IsDefault && <button className="btn" onClick={() => onSetDefault(g)}>设为默认</button>}
                <button className="btn" onClick={() => openEdit(g)}><Icons.Cog size={12}/> 编辑</button>
                <button className="btn ghost icon danger" onClick={() => onDelete(g)} title="删除分组" aria-label={`删除分组 ${g.Name}`}><Icons.Trash size={12}/></button>
              </div>
            </div>
          );
        })}
      </div>

      {/* 新建 / 编辑 */}
      <Modal open={modal === "add" || modal === "edit"} onClose={() => !busy && setModal(null)} size="md"
        title={modal === "add" ? "新建终端分组" : "编辑分组"}
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy || netStatus === "blocked"} title={netStatus === "blocked" ? "网段不在服务器的客户机网段，先改到服务器网段或打开跨网段分组" : ""} onClick={save}>{busy ? "保存中…" : "保存"}</button></>}>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 12 }}>
          <Field label="分组名称" required><input className="input" aria-label="分组名称" style={{ width: "100%" }} value={form.name || ""} onChange={e => setForm({ ...form, name: e.target.value })}/></Field>
          <Field label="设为默认分组"><Toggle checked={!!form.is_default} onChange={v => setForm({ ...form, is_default: v })} onLabel="是" offLabel="否"/></Field>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 12 }}>
          <Field label="起始 IP" required><input className="input mono" aria-label="起始 IP" style={{ width: "100%" }} value={form.start_ip || ""} onChange={e => setForm({ ...form, start_ip: e.target.value })}/></Field>
          <Field label="最大客户机数" required><input className="input mono" type="number" aria-label="最大客户机数" style={{ width: "100%" }} value={form.client_max || 0} onChange={e => setForm({ ...form, client_max: +e.target.value })}/></Field>
        </div>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 12 }}>
          <Field label={netStatus === "relay" ? "网关（中继地址）" : "网关"} required={netStatus === "relay"}
            help={netStatus === "relay" ? "该网段在交换机上的三层接口地址，也就是做 DHCP 中继的那台" : "客户机上网用的路由器地址；服务器自己做路由时填服务器；没有路由器可留空"}>
            <input className="input mono" aria-label={netStatus === "relay" ? "网关（中继地址）" : "网关"} style={{ width: "100%" }} value={form.gateway || ""} onChange={e => setForm({ ...form, gateway: e.target.value })}/>
          </Field>
          <Field label="子网掩码" required><input className="input mono" aria-label="子网掩码" style={{ width: "100%" }} value={form.netmask || ""} onChange={e => setForm({ ...form, netmask: e.target.value })}/></Field>
        </div>
        <NetworkStatusLine status={netStatus} form={form} network={network} onFill={fillSuggested} toast={store.toast}/>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(200px, 1fr))", gap: 12 }}>
          <Field label="DNS1"><input className="input mono" aria-label="DNS1" style={{ width: "100%" }} value={form.dns1 || ""} onChange={e => setForm({ ...form, dns1: e.target.value })}/></Field>
          <Field label="DNS2"><input className="input mono" aria-label="DNS2" style={{ width: "100%" }} value={form.dns2 || ""} onChange={e => setForm({ ...form, dns2: e.target.value })}/></Field>
        </div>
        <ImageBindingPicker images={images.filter(i => i.Purpose !== "data")} imageLabel="系统镜像"
          configHelp="客户机开机使用该配置的应用还原点；超管机使用该配置的最新还原点。镜像 / 配置与网络的改动，对组内终端于下次开机生效；改了网段，组内终端的地址会自动换到新网段。"
          imageId={form.system_image_id} configId={form.system_config_id}
          onChange={({ image_id, config_id }) => setForm({ ...form, system_image_id: image_id, system_config_id: config_id })}/>
        {clusterNodes.length > 1 && (
          <Field label="存储节点" help="该组客户机的磁盘由哪台服务器提供。默认自动均衡：开机时挑当前最闲的健康节点，机器会尽量回到上次那台（克隆还在、缓存是热的）。固定到某台适合让某个班独占一台机器。改动对组内终端于下次开机生效；节点故障时自动回退到主机。">
            <select className="input" aria-label="存储节点" style={{ width: "100%" }} value={form.storage_server_id || ""}
              onChange={e => setForm({ ...form, storage_server_id: e.target.value })}>
              <option value="">自动均衡（推荐）</option>
              {clusterNodes.map(n => (
                <option key={n.id} value={n.id} disabled={!n.online}>
                  固定到 {n.ip || n.name || n.id}{n.ha_state === "active" ? "（主机）" : ""}{n.online ? "" : "（离线）"}
                </option>
              ))}
            </select>
          </Field>
        )}
      </Modal>

      {disksGroup && <GroupDisksModal open={modal === "disks"} group={disksGroup} images={images} onClose={() => { setModal(null); setDisksGroup(null); }}/>}

      {/* 把落在网段外的分组挪回客户机网段 */}
      <Modal open={modal === "fixnet" && !!fix} onClose={() => !busy && setModal(null)} size="md" title={fix ? `${fix.group.Name} · 改到服务器网段` : ""}
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={applyFix}>{busy ? "修改中…" : "确认修改"}</button></>}>
        {fix && <>
          <div style={{ display: "grid", gridTemplateColumns: "max-content 1fr", gap: "4px 12px", fontSize: 14, marginBottom: 10 }}>
            <span className="meta">现在</span><span className="mono">{rangeText(fix.group.StartIP, fix.group.ClientMax)} · 网关 {fix.group.Gateway || "无"}</span>
            <span className="meta">改为</span><span className="mono">{rangeText(fix.target.start_ip, fix.target.client_max)} · 掩码 {fix.target.netmask} · 网关 {fix.target.gateway || "无"}</span>
          </div>
          {fix.preview.terminals.length
            ? <>
                <div className="hint" style={{ marginBottom: 6 }}>组内 {fix.preview.terminals.length} 台终端的地址跟着换（保持相对位置），旧租约会释放。</div>
                {/* 运行中的机器续租时才换地址，Windows 的 IP 一变 iSCSI 就断，所以要单独提示。 */}
                {fix.preview.online_count > 0 && (
                  <div className="hint" style={{ marginBottom: 6, color: "var(--amber)" }}>
                    其中 {fix.preview.online_count} 台正在运行。它们不会立刻换地址，而是在租约续期时换——那一刻会掉线并重启。
                    建议等这些机器关机后再改。
                  </div>
                )}
                <div style={{ overflowX: "auto" }}><table className="t">
                  <thead><tr><th>终端</th><th>MAC</th><th>现在</th><th>改为</th><th>状态</th></tr></thead>
                  <tbody>{fix.preview.terminals.map(t => <tr key={t.id}><td>{t.name || t.id}</td><td className="mono">{t.mac}</td><td className="mono">{t.old_ip}</td><td className="mono">{t.new_ip}</td>
                    <td>{t.online ? <span className="chip amber">运行中</span> : <span className="meta">关机</span>}</td></tr>)}</tbody>
                </table></div>
              </>
            : <div className="hint">组内没有需要换地址的终端。</div>}
        </>}
      </Modal>

      {conf.node}
    </div>
  );
}

// 镜像 → 配置级联选择，每一级按需获取。
function ImageBindingPicker({ images, imageId, configId, onChange, imageLabel = "系统镜像", configLabel = "启动配置", configHelp }) {
  const [configs, setConfigs] = useState([]);
  // <select> 的值不匹配任何选项时仍显示第一项，下一级却按空值查成「无可用配置」，
  // 所以把值落到实际显示的镜像上，与配置那一级的做法一致。
  useEffect(() => {
    if (imageId || !images.length) return;
    onChange({ image_id: images[0].ID, config_id: "" });
  }, [imageId, images, onChange]);
  useEffect(() => {
    if (!imageId) { setConfigs([]); return; }
    let c = false;
    api.listConfigs(imageId).then(r => { if (!c) setConfigs(asItems(r)); }).catch(() => { if (!c) setConfigs([]); });
    return () => { c = true; };
  }, [imageId]);
  useEffect(() => {
    if (!imageId || !configs.length) return;
    if (configs.some(c => c.ID === configId)) return;
    onChange({ image_id: imageId, config_id: configs[0].ID });
  }, [imageId, configId, configs, onChange]);
  return (
    <>
      <Field label={imageLabel} required>
        <Select value={imageId || ""} aria-label={imageLabel} onChange={v => onChange({ image_id: v, config_id: "" })}
          options={images.length ? images.map(i => ({ value: i.ID, label: `${i.Name} · ${i.OSType}` })) : [{ value: "", label: "无可用镜像" }]}/>
      </Field>
      <Field label={configLabel} required help={configHelp}>
        <Select value={configId || ""} onChange={v => onChange({ image_id: imageId, config_id: v })}
          options={configs.length ? configs.map(c => ({ value: c.ID, label: c.Name })) : [{ value: "", label: "无可用配置" }]}/>
      </Field>
    </>
  );
}

// 分组的数据盘（额外挂载的镜像 + 配置），例如 D: 游戏盘。
function GroupDisksModal({ open, group, images, onClose }) {
  const store = useStore();
  const conf = useConfirm();
  const [form, setForm] = useState({ mount_target: "", image_id: "", config_id: "" });
  const [adding, setAdding] = useState(false);

  const { data, loading, reload: load } = useResource(
    () => (open ? api.listGroupDisks(group.ID) : Promise.resolve(null)), [open, group.ID]);
  const disks = (data && data.items) || [];
  const { busy, run } = useMutation(store.toast);
  useEffect(() => { if (open) { setForm({ mount_target: "", image_id: "", config_id: "" }); setAdding(false); } }, [open]);

  const imageName = (id) => images.find(i => i.ID === id)?.Name || id || "—";

  const add = async () => {
    if (!form.mount_target?.trim()) { store.toast("请输入挂载盘符/目标", "err"); return; }
    if (!form.image_id || !form.config_id) { store.toast("请选择镜像与配置", "err"); return; }
    const ok = await run(() => api.createGroupDisk(group.ID, { mount_target: form.mount_target.trim(), image_id: form.image_id, config_id: form.config_id }),
      "数据盘已添加", { reload: load, errMsg: "添加失败" });
    if (ok) { setForm({ mount_target: "", image_id: "", config_id: "" }); setAdding(false); }
  };
  const del = async (d) => {
    const ok = await conf.ask({ title: "移除数据盘", message: `移除 ${group.Name} 的数据盘 ${d.MountTarget}？`, danger: true });
    if (!ok) return;
    await run(() => api.deleteGroupDisk(d.ID), "数据盘已移除", { reload: load, errMsg: "移除失败" });
  };

  return (
    <Modal open={open} onClose={onClose} title={`分组数据盘 · ${group.Name}`} size="md"
      footer={<><button className="btn" onClick={onClose}>关闭</button>{adding && <button className="btn primary" disabled={busy} onClick={add}>{busy ? "保存中…" : "保存"}</button>}</>}>
      <div className="hint" style={{ marginBottom: 10 }}>开机时随系统盘一起挂载的附加盘（如游戏盘 / D 盘），每块由镜像 + 配置提供。盘符改动于终端下次开机自动生效。</div>
      <div style={{ background: "var(--bg-0)", border: "1px solid var(--line-soft)", borderRadius: 6, overflow: "hidden", marginBottom: 14 }}>
        <table className="t" style={{ margin: 0 }}>
          <thead><tr><th>挂载目标</th><th>镜像</th><th>配置</th><th className="t-actions"></th></tr></thead>
          <tbody>
            {disks.map(d => (
              <tr key={d.ID}>
                <td className="mono" style={{ fontWeight: 600 }}>{d.MountTarget}</td>
                <td className="muted">{imageName(d.ImageID)}</td>
                <td className="mono muted" style={{ fontSize: 12 }}>{d.ConfigID}</td>
                <td className="t-actions"><div><button className="btn ghost icon danger" onClick={() => del(d)} title="移除数据盘" aria-label={`移除数据盘 ${d.MountTarget}`}><Icons.Trash size={12}/></button></div></td>
              </tr>
            ))}
            <TableState colSpan={4} loading={loading} empty={!disks.length} hint="暂无数据盘"/>
          </tbody>
        </table>
      </div>
      <div className="card-title" style={{ marginBottom: 10, cursor: "pointer", userSelect: "none" }} onClick={() => setAdding(a => !a)}>
        {adding ? <Icons.X size={12}/> : <Icons.Plus size={12}/>} 添加数据盘
      </div>
      {adding && <>
        <Field label="挂载目标" required hint="Windows 组填盘符 D:–Z:；Linux 组填挂载点"><input className="input mono" style={{ width: "100%" }} value={form.mount_target} onChange={e => setForm({ ...form, mount_target: e.target.value })} placeholder="D:"/></Field>
        {(() => {
          // 数据盘只能选与分组同系统的数据盘镜像。
          const sysOS = images.find(i => i.ID === group.SystemImageID)?.OSType;
          const dataImages = images.filter(i => i.Purpose === "data" && (!sysOS || i.OSType === sysOS));
          return dataImages.length
            ? <ImageBindingPicker images={dataImages} imageLabel="数据盘镜像" configLabel="配置"
                imageId={form.image_id} configId={form.config_id}
                onChange={({ image_id, config_id }) => setForm({ ...form, image_id, config_id })}/>
            : <div className="hint" style={{ margin: "4px 0 10px" }}>还没有{sysOS === "linux" ? " Linux" : " Windows"} 数据盘镜像。<span style={{ color: "var(--cyan)", cursor: "pointer" }} onClick={() => store.goto("images")}>去镜像页新建</span></div>;
        })()}
      </>}
      {conf.node}
    </Modal>
  );
}

// NetworkStatusLine 在输入时就给出与服务端保存时相同的网络判定。
function NetworkStatusLine({ status, form, network, onFill, toast }) {
  const [probe, setProbe] = useState(null); // { ip, reachable, rtt_ms } | { ip, error }
  const [showSnippet, setShowSnippet] = useState(false);
  useEffect(() => { setProbe(null); }, [form.gateway]);
  if (status === "unknown" || !network) return null;
  const range = rangeText(form.start_ip, form.client_max);
  const nets = (network.client_networks || []).join("、");
  const iface = network.client_iface || "?";
  const box = (color, bg, children) => (
    <div style={{ display: "flex", gap: 8, alignItems: "flex-start", padding: "8px 10px", borderRadius: 6, background: bg, color, fontSize: 13, lineHeight: 1.6, marginBottom: 12 }}>
      <span className="dot" style={{ background: color, marginTop: 6, flexShrink: 0 }}/>
      <span style={{ minWidth: 0 }}>{children}</span>
    </div>
  );
  if (status === "same") {
    // 正常只给一行短提示，有问题才给处理说明。
    return box("var(--emerald)", "var(--emerald-soft)", <><b>同网段</b> · 客户机开机可直接拿到地址</>);
  }
  if (status === "blocked") {
    const sug = network.suggest;
    return box("var(--rose)", "var(--rose-soft)", <>
      <b>不在服务器的客户机网段</b> · 服务器客户机网卡是 {iface}（{nets}），{range} 的客户机拿不到地址、开不了机。<br/>
      改成服务器网段{sug ? <>（<button type="button" className="btn" style={{ padding: "1px 8px", marginLeft: 2 }} onClick={onFill}>填入 {rangeText(sug.start_ip, Number(form.client_max) || sug.client_max)}</button>）</> : ""}，
      或者客户机确实在别的 VLAN → 到系统参数打开「跨网段分组」。
    </>);
  }
  const doProbe = async () => {
    const ip = (form.gateway || "").trim();
    if (!ip) { toast("先填网关（中继地址）", "err"); return; }
    try { setProbe(await api.probeNetwork(ip)); } catch (e) { setProbe({ ip, error: e.message || "检测失败" }); }
  };
  return box("var(--amber)", "var(--amber-soft)", <>
    <b>跨网段 · DHCP 中继</b> · 客户机在 {range} 所在网段，请求经交换机中继到服务器 {network.boot_host || nets}。网关必须填该网段在交换机上的三层接口（做中继的那台）。
    <span style={{ display: "inline-flex", gap: 8, marginLeft: 6 }}>
      <button type="button" className="btn" style={{ padding: "1px 8px" }} onClick={doProbe}>检测</button>
      <button type="button" className="btn" style={{ padding: "1px 8px" }} onClick={() => setShowSnippet(v => !v)}>{showSnippet ? "收起" : "查看交换机配置"}</button>
    </span>
    {probe && (probe.error
      ? <div>检测失败：{probe.error}</div>
      : probe.reachable
        ? <div>从服务器 ping {probe.ip}：通{probe.rtt_ms ? `（${probe.rtt_ms} ms）` : ""} ✓</div>
        : <div>从服务器 ping 不通 {probe.ip}。客户机可能拿不到地址：请网管检查该 VLAN 三层接口上的 DHCP 中继（helper-address {network.boot_host}）和到服务器的路由（交换机禁 ping 时可忽略）。</div>)}
    {showSnippet && <pre className="mono" style={{ marginTop: 6, whiteSpace: "pre-wrap", fontSize: 12, color: "var(--fg)" }}>{relaySnippet(network.boot_host || nets)}</pre>}
  </>);
}

function KV2({ k, v }) {
  return (
    <div style={{ display: "flex", justifyContent: "space-between", padding: "5px 0", borderBottom: "1px dashed var(--line-soft)", fontSize: 13 }}>
      <span style={{ color: "var(--fg-faint)" }}>{k}</span>
      <span className="mono" style={{ color: "var(--fg)", maxWidth: "65%", overflow: "hidden", textOverflow: "ellipsis", whiteSpace: "nowrap" }}>{v}</span>
    </div>
  );
}

window.PageGroups = PageGroups;

export { PageGroups };
