// 后端 User 只有 { ID, Username, CreatedAt }，没有后端支撑的列和页签一律不做。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, SearchBox, Select, TableState, Pager } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, useMutation, usePaged } from '../../lib/hooks.js';
import { HACard } from '../ha.jsx';
const { useState, useEffect } = React;

const NET_STATUS = {
  same:    { chip: "emerald", text: "同网段" },
  relay:   { chip: "amber",   text: "跨网段·中继" },
  blocked: { chip: "rose",    text: "不通" },
  unknown: { chip: "",        text: "无法判定" },
};

const getImportDir = (settings) => (settings && (settings.import_dir || settings.ImportDir)) || "";
const getDefaultImportDir = (settings) => (settings && (settings.default_import_dir || settings.DefaultImportDir)) || "/var/lib/ndiskless/imports";
const isAbsolutePath = (v) => /^\//.test((v || "").trim());
const isVarPath = (v) => /^\/var(\/|$)/.test((v || "").trim());

function PageSettings() {
  const store = useStore();
  const conf = useConfirm();
  const [q, setQ] = useState("");
  const [modal, setModal] = useState(null); // add | rename | password
  const [form, setForm] = useState({});

  let currentUsername = null;
  try { currentUsername = JSON.parse(localStorage.getItem("nd_user") || "null")?.username; } catch { /* ignore */ }

  const { data, loading, error, reload: load } = useResource(() => api.listUsers(), []);
  const users = (data && data.items) || (Array.isArray(data) ? data : []);
  const { busy, run } = useMutation(store.toast);

  const filtered = users.filter(u => !q || u.username.toLowerCase().includes(q.toLowerCase()));
  const paged = usePaged(filtered);

  const openAdd = () => { setForm({ username: "", password: "" }); setModal("add"); };
  const openRename = (u) => { setForm({ id: u.id, username: u.username }); setModal("rename"); };
  const openPassword = (u) => { setForm({ id: u.id, username: u.username, old_password: "", new_password: "" }); setModal("password"); };

  const doAdd = async () => {
    if (!form.username?.trim()) { store.toast("请输入用户名", "err"); return; }
    if (!form.password || form.password.length < 6) { store.toast("初始密码至少 6 位", "err"); return; }
    if (await run(() => api.createUser({ username: form.username.trim(), password: form.password }),
      `用户 ${form.username.trim()} 已创建`, { reload: load, errMsg: "创建失败" })) setModal(null);
  };
  const doRename = async () => {
    if (!form.username?.trim()) { store.toast("请输入用户名", "err"); return; }
    if (await run(() => api.updateUser(form.id, { username: form.username.trim() }),
      "用户名已更新", { reload: load, errMsg: "更新失败" })) setModal(null);
  };
  const doPassword = async () => {
    if (!form.old_password || !form.new_password) { store.toast("请填写原密码与新密码", "err"); return; }
    if (form.new_password.length < 6) { store.toast("新密码至少 6 位", "err"); return; }
    if (await run(() => api.changeUserPassword(form.id, { old_password: form.old_password, new_password: form.new_password }),
      `已修改 ${form.username} 的密码`, { errMsg: "修改失败" })) setModal(null);
  };
  const del = async (u) => {
    const ok = await conf.ask({ title: "删除用户", message: `确定删除用户 ${u.username}？`, danger: true });
    if (!ok) return;
    await run(() => api.deleteUser(u.id), "用户已删除", { reload: load, errMsg: "删除失败" });
  };

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div className="card" style={{ padding: "10px 14px", display: "flex", alignItems: "center", gap: 12 }}>
        <SearchBox value={q} onChange={setQ} placeholder="搜索用户名"/>
        <span className="chip">{users.length} 用户</span>
        <div style={{ flex: 1 }}/>
        <button className="btn ghost icon" onClick={() => load()} title="立即刷新" aria-label="立即刷新"><Icons.Refresh size={13}/></button>
        <button className="btn primary" onClick={openAdd}><Icons.Plus size={12}/> 新建用户</button>
      </div>

      <div className="card" style={{ padding: 0 }}>
        <table className="t">
          <thead><tr><th>用户ID</th><th>用户名</th><th className="t-actions">操作</th></tr></thead>
          <tbody>
            {paged.rows.map(u => (
              <tr key={u.id}>
                <td className="mono muted" style={{ fontSize: 12 }}>{u.id}</td>
                <td>
                  <div className="row" style={{ gap: 8 }}>
                    <div style={{ width: 24, height: 24, borderRadius: "50%", background: u.username === "admin" ? "linear-gradient(135deg,var(--cyan),var(--violet))" : "var(--bg-3)", display: "grid", placeItems: "center", fontSize: 11, fontWeight: 600, color: u.username === "admin" ? "oklch(0.18 0.014 250)" : "var(--fg)" }}>{u.username.charAt(0).toUpperCase()}</div>
                    <span className="mono" style={{ fontWeight: 500 }}>{u.username}</span>
                    {u.username === currentUsername && <span className="chip cyan">当前</span>}
                  </div>
                </td>
                <td className="t-actions"><div>
                  <button className="btn" style={{ padding: "3px 8px" }} onClick={() => openRename(u)}>改名</button>
                  <button className="btn" style={{ padding: "3px 8px" }} onClick={() => openPassword(u)}><Icons.Key size={11}/> 改密码</button>
                  {u.username !== "admin" && <button className="btn ghost icon danger" onClick={() => del(u)} title="删除用户" aria-label={`删除用户 ${u.username}`}><Icons.Trash size={12}/></button>}
                </div></td>
              </tr>
            ))}
            <TableState colSpan={3} loading={loading} error={error} empty={!filtered.length} hint={users.length ? "没有匹配的用户" : "暂无用户"}/>
          </tbody>
        </table>
        <Pager {...paged.pager}/>
      </div>

      <div className="hint" style={{ padding: "0 4px" }}>
        角色权限、菜单管理暂未接入后端，后续版本提供。
      </div>

      {/* 新建 */}
      <Modal open={modal === "add"} onClose={() => !busy && setModal(null)} title="新建用户" size="sm"
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={doAdd}>{busy ? "创建中…" : "创建"}</button></>}>
        <Field label="用户名" required><input className="input mono" style={{ width: "100%" }} value={form.username || ""} onChange={e => setForm({ ...form, username: e.target.value })} placeholder="operator"/></Field>
        <Field label="初始密码" required hint="至少 6 位"><input className="input mono" type="password" style={{ width: "100%" }} value={form.password || ""} onChange={e => setForm({ ...form, password: e.target.value })}/></Field>
      </Modal>

      {/* 改名 */}
      <Modal open={modal === "rename"} onClose={() => !busy && setModal(null)} title="修改用户名" size="sm"
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={doRename}>{busy ? "保存中…" : "保存"}</button></>}>
        <Field label="用户名" required><input className="input mono" style={{ width: "100%" }} value={form.username || ""} onChange={e => setForm({ ...form, username: e.target.value })}/></Field>
      </Modal>

      {/* 改密码 */}
      <Modal open={modal === "password"} onClose={() => !busy && setModal(null)} title={`修改密码 · ${form.username || ""}`} size="sm"
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={doPassword}>{busy ? "提交中…" : "确认"}</button></>}>
        <Field label="原密码" required><input className="input mono" type="password" style={{ width: "100%" }} value={form.old_password || ""} onChange={e => setForm({ ...form, old_password: e.target.value })}/></Field>
        <Field label="新密码" required hint="至少 6 位"><input className="input mono" type="password" style={{ width: "100%" }} value={form.new_password || ""} onChange={e => setForm({ ...form, new_password: e.target.value })}/></Field>
      </Modal>

      {conf.node}
    </div>
  );
}

function PageSystemParams() {
  const store = useStore();
  const [form, setForm] = useState({ import_dir: "" });
  const { data, loading, error, reload } = useResource(() => api.getSettings(), []);
  const { busy, run } = useMutation(store.toast);

  const savedImportDir = getImportDir(data);
  const defaultImportDir = getDefaultImportDir(data);
  const value = form.import_dir || "";
  const dirty = value.trim() !== (savedImportDir || "").trim();
  const valid = isAbsolutePath(value);

  useEffect(() => {
    if (data) setForm({ import_dir: getImportDir(data) });
  }, [data]);

  const save = async () => {
    const dir = value.trim();
    if (!dir) { store.toast("请输入镜像导入目录", "err"); return; }
    if (!isAbsolutePath(dir)) { store.toast("镜像导入目录必须是绝对路径", "err"); return; }
    await run(() => api.saveSettings({ import_dir: dir }), "镜像导入目录已更新", { reload, errMsg: "保存失败" });
  };

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <HACard/>
      <ClientNetworkSettings store={store}/>
      <ImageDirectorySettings
        loading={loading}
        error={error}
        value={value}
        savedValue={savedImportDir}
        defaultValue={defaultImportDir}
        dirty={dirty}
        valid={valid}
        busy={busy}
        onChange={(v) => setForm({ import_dir: v })}
        onReset={() => setForm({ import_dir: savedImportDir })}
        onSave={save}
      />
    </div>
  );
}

// ClientNetworkSettings 设置 dnsmasq 服务的网卡，以及分组能否跨网段（交换机做 DHCP 中继的现场）。
// 打开开关时的确认文案写给网管看；仍有分组依赖时服务端会拒绝关闭，原样显示拒绝信息。
function ClientNetworkSettings({ store }) {
  const { data, loading, error, reload } = useResource(() => api.getNetwork(30), []);
  const { busy, run } = useMutation(store.toast);
  const [confirmOn, setConfirmOn] = useState(false);
  const [saveError, setSaveError] = useState("");
  const [iface, setIface] = useState(null); // null 表示未改动
  const goto = store.goto;
  const view = data || {};
  const chosenIface = iface === null ? (view.client_iface_setting || "") : iface;
  const ifaceOptions = [{ value: "", label: `自动识别${view.client_iface ? `（当前 ${view.client_iface}）` : ""}` }]
    .concat((view.interfaces || []).map(i => ({ value: i.name, label: `${i.name} · ${(i.addrs || []).join(", ") || "无地址"}` })));
  const save = async (allow) => {
    setSaveError("");
    const ok = await run(() => api.saveNetwork({ client_iface: chosenIface, allow_cross_subnet: allow }), allow ? "已允许跨网段分组，dnsmasq 改为在所有网卡上接收请求" : "已保存", {
      reload: () => { setIface(null); reload(); },
      errMsg: "保存失败",
    });
    if (!ok) {
      // 拒绝信息点名了相关分组，要留在卡片上，不能只放在三秒就消失的提示里。
      try { await api.saveNetwork({ client_iface: chosenIface, allow_cross_subnet: allow }); } catch (e) { setSaveError(e.message || "保存失败"); }
    }
    return ok;
  };
  const onToggle = async () => {
    if (view.allow_cross_subnet) { await save(false); return; }
    setConfirmOn(true);
  };
  const stateChip = loading ? { cls: "", text: "读取中" } : !view.known ? { cls: "rose", text: "网卡无地址" } : view.allow_cross_subnet ? { cls: "amber", text: "跨网段模式" } : { cls: "emerald", text: "同网段模式" };
  return (
    <div className="card" style={{ padding: 0, overflow: "hidden" }}>
      <div className="card-h">
        <div className="card-title"><Icons.Network size={14}/> 客户机网络</div>
        <span className={`chip ${stateChip.cls}`}>{stateChip.text}</span>
      </div>
      <div style={{ padding: 16 }}>
        <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(260px, 1fr))", gap: 16, alignItems: "start" }}>
          <Field label="客户机网卡" helpAfterLabel help="dnsmasq 给客户机发地址的网卡；改了服务器网卡后点「重新识别」">
            <div style={{ display: "flex", gap: 8 }}>
              <Select value={chosenIface} aria-label="客户机网卡" onChange={v => setIface(v)} options={ifaceOptions} style={{ flex: 1, minWidth: 0 }}/>
              {iface !== null && iface !== (view.client_iface_setting || "") && <button className="btn primary" disabled={busy} onClick={() => save(!!view.allow_cross_subnet)}>保存</button>}
              <button className="btn" disabled={busy || loading} onClick={() => { setIface(null); reload(); }}>重新识别</button>
            </div>
          </Field>
          <Field label="客户机网段" helpAfterLabel help={view.allow_cross_subnet ? "在所有网卡上接收请求（含中继来的）" : `只在 ${view.client_iface || "客户机网卡"} 上接收请求`}>
            <div className="mono" style={{ fontSize: 14, minHeight: 32, display: "flex", alignItems: "center", gap: 6, flexWrap: "wrap" }}>
              {(view.client_networks || []).length
                ? view.client_networks.map(n => <span key={n} className="chip">{n}</span>)
                : (loading ? "读取中…" : <span style={{ color: "var(--rose)" }}>客户机网卡 {view.client_iface || ""} 没有 IPv4 地址</span>)}
            </div>
          </Field>
          <Field label="跨网段分组" helpAfterLabel help={`不在同一网段时打开，交换机要把 DHCP 中继指向 ${view.boot_host || "服务器"}`}>
            <label style={{ display: "flex", alignItems: "center", gap: 8, minHeight: 32, fontSize: 14, cursor: "pointer" }}>
              <input type="checkbox" checked={!!view.allow_cross_subnet} disabled={busy || loading} onChange={onToggle} aria-label="允许跨网段分组（DHCP 中继）"/>
              允许（DHCP 中继）
            </label>
          </Field>
        </div>
        {saveError && <div style={{ fontSize: 13, color: "var(--rose)", marginBottom: 10 }}>{saveError}</div>}
        {error && <div style={{ fontSize: 13, color: "var(--rose)", marginBottom: 10 }}>读取失败：{error}</div>}

        {(view.groups || []).length > 0 && (
          <div>
            <div style={{ border: "1px solid var(--line)", borderRadius: "var(--r-md)", overflow: "hidden" }}>
              <table className="t">
                <thead><tr><th>分组</th><th>起始地址</th><th>台数</th><th>状态</th></tr></thead>
                <tbody>
                  {view.groups.map(g => {
                    const st = NET_STATUS[g.status] || NET_STATUS.unknown;
                    return (
                      <tr key={g.id}>
                        <td><button type="button" style={{ background: "none", border: 0, padding: 0, color: "var(--cyan)", cursor: "pointer", font: "inherit" }} onClick={() => goto("groups", { groupId: g.id })}>{g.name}</button></td>
                        <td className="mono">{g.start_ip}</td>
                        <td className="mono">{g.client_max}</td>
                        <td>
                          {st.text && <span className={`chip ${st.chip}`}>{st.text}</span>}
                          {g.status === "blocked" && <span className="meta" style={{ marginLeft: 8 }}>拿不到地址，去分组页「改到服务器网段」</span>}
                        </td>
                      </tr>
                    );
                  })}
                </tbody>
              </table>
            </div>
          </div>
        )}
      </div>

      <Modal open={confirmOn} onClose={() => !busy && setConfirmOn(false)} title="打开跨网段分组" size="md"
        footer={<><button className="btn" disabled={busy} onClick={() => setConfirmOn(false)}>取消</button><button className="btn primary" disabled={busy} onClick={async () => { if (await save(true)) setConfirmOn(false); }}>{busy ? "保存中…" : "打开"}</button></>}>
        <div className="hint" style={{ marginBottom: 8 }}>打开后，分组网段可以和服务器不在一个网段，需要交换机配置 DHCP 中继，分组的网关填做中继的那台三层接口地址（编辑跨网段分组时可查看交换机配置）。</div>
        <div className="hint">dnsmasq 会改为在所有网卡上接收 DHCP 请求；只回应已登记的终端，办公网上的其他设备不会拿到地址。</div>
      </Modal>
    </div>
  );
}

function ImageDirectorySettings({ loading, error, value, savedValue, defaultValue, dirty, valid, busy, onChange, onReset, onSave }) {
  const risk = isVarPath(value);
  const empty = !(value || "").trim();
  const stateChip = loading
    ? { cls: "", text: "读取中" }
    : dirty
      ? { cls: "amber", text: "未保存" }
      : risk
        ? { cls: "amber", text: "系统盘风险" }
        : { cls: "emerald", text: "已生效" };
  return (
    <div className="card" style={{ padding: 0, overflow: "hidden" }}>
      <div className="card-h">
        <div className="card-title"><Icons.Folder size={14}/> 镜像导入目录</div>
        <span className={`chip ${stateChip.cls}`}>{stateChip.text}</span>
      </div>
      <div style={{ padding: 16 }}>
        <Field label="目录路径" required hint={risk ? "当前路径位于 /var，镜像较大时容易占满系统盘。" : "服务器绝对路径；保存时会做写入测试。"}>
          <div style={{ display: "grid", gridTemplateColumns: "minmax(260px, 1fr) auto auto", gap: 8, alignItems: "center" }}>
            <input
              className="input mono"
              style={{ width: "100%", minWidth: 0 }}
              value={value}
              onChange={e => onChange(e.target.value)}
              placeholder="/tank/imports"
            />
            {dirty && <button className="btn primary" style={{ minWidth: 58, justifyContent: "center", whiteSpace: "nowrap" }} disabled={busy || loading || empty || !valid} onClick={onSave}>
              {busy ? "保存中…" : "保存"}
            </button>}
            {dirty && <button className="btn" style={{ whiteSpace: "nowrap" }} disabled={busy} onClick={onReset}>撤销</button>}
          </div>
        </Field>
        <div style={{ display: "flex", flexWrap: "wrap", gap: 8, alignItems: "center", minHeight: 22 }}>
          {savedValue && <span className="mono" style={{ fontSize: 12, color: "var(--fg-faint)", wordBreak: "break-all" }}>当前：{savedValue}</span>}
          <span className="chip mono" title={defaultValue}>默认目录</span>
          {!empty && !valid && <span className="chip rose">需要绝对路径</span>}
          {error && <span className="chip rose">{error}</span>}
        </div>
      </div>
    </div>
  );
}

window.PageSettings = PageSettings;
window.PageSystemParams = PageSystemParams;

export { PageSettings, PageSystemParams };
