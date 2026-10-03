import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, Select, TableState, Pager } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, useMutation, usePolling, usePaged } from '../../lib/hooks.js';

const { useState, useEffect } = React;

const CATEGORY_LABELS = {
  boot_critical_nic: '启动网卡',
  nic: '网卡',
  gpu: '显卡',
  audio: '声卡',
  chipset: '芯片组',
  other: '其它',
};
const RISK_LABELS = { unsigned: '未签名', no_hwids: '无 HWID', outdated: '版本过旧' };

function PageDrivers() {
  const store = useStore();
  const conf = useConfirm();
  const { route } = store;
  // packs | bundles | cure：可深链，客户机详情的「去驱动中心处理」直接落到固化页签。
  const [tab, setTab] = useState(route.params.tab || 'packs');
  useEffect(() => { if (route.params.tab) setTab(route.params.tab); }, [route.params.tab]);
  const [uploadOpen, setUploadOpen] = useState(false);
  const [bundleOpen, setBundleOpen] = useState(false);
  const [uploadForm, setUploadForm] = useState({ name: '', category: 'boot_critical_nic', os_type: 'windows', arch: 'x64', file: null });
  const [bundleForm, setBundleForm] = useState({ name: '', packIds: [] });
  const [q, setQ] = useState('');
  const [catFilter, setCatFilter] = useState('all');

  // 分开 settle，驱动组列表失败不能连带清空驱动包表格。
  const { data, loading, error, reload } = useResource(async () => {
    const [packRes, bundleRes, termRes] = await Promise.allSettled([api.listDriverPacks(), api.listDriverBundles(), api.listTerminals()]);
    if (packRes.status === 'rejected') throw packRes.reason;
    return {
      packs: (packRes.value && packRes.value.items) || [],
      bundles: bundleRes.status === 'fulfilled' ? (bundleRes.value && bundleRes.value.items) || [] : [],
      terminals: termRes.status === 'fulfilled' ? (termRes.value && termRes.value.items) || [] : [],
    };
  }, []);
  const packs = (data && data.packs) || [];
  const bundles = (data && data.bundles) || [];
  const terminals = (data && data.terminals) || [];
  const superTerminal = terminals.find(t => t.IsSuper) || null;
  const curePending = !!(superTerminal && superTerminal.PendingBundleID);
  const { busy, run } = useMutation(store.toast);

  const filteredPacks = packs.filter(({ pack }) =>
    (catFilter === 'all' || pack.Category === catFilter) &&
    (!q || pack.Name.toLowerCase().includes(q.toLowerCase()) || (pack.Vendor || '').toLowerCase().includes(q.toLowerCase())));
  const packPager = usePaged(filteredPacks);
  const bundlePager = usePaged(bundles);

  const doUpload = async () => {
    if (!uploadForm.file) { store.toast('请选择驱动 zip 文件', 'err'); return; }
    const fd = new FormData();
    fd.append('file', uploadForm.file);
    fd.append('name', uploadForm.name);
    fd.append('category', uploadForm.category);
    fd.append('os_type', uploadForm.os_type);
    fd.append('arch', uploadForm.arch);
    if (await run(() => api.uploadDriverPack(fd), '驱动包已上传并解析', { reload })) {
      setUploadOpen(false);
      setUploadForm({ name: '', category: 'boot_critical_nic', os_type: 'windows', arch: 'x64', file: null });
    }
  };

  const toggleStatus = (pack) => {
    const next = pack.Status === 'enabled' ? 'disabled' : 'enabled';
    return run(() => api.setDriverPackStatus(pack.ID, next), next === 'enabled' ? '驱动包已启用' : '驱动包已禁用', { reload });
  };

  const recommend = (pack) => run(() => api.recommendDriverPack(pack.ID), '已设为推荐版本', { reload });

  const removePack = async (pack) => {
    const ok = await conf.ask({ title: '删除驱动包', message: `确定删除 ${pack.Name}？被驱动集引用的包无法删除。`, danger: true });
    if (ok) await run(() => api.deleteDriverPack(pack.ID), '驱动包已删除', { reload });
  };

  const bootPacks = packs.filter(p => p.pack.Category === 'boot_critical_nic' && p.pack.Status === 'enabled');

  const doCreateBundle = async () => {
    if (!bundleForm.name.trim() || bundleForm.packIds.length === 0) {
      store.toast('请填写名称并选择至少一个启动网卡驱动包', 'err');
      return;
    }
    if (await run(() => api.createDriverBundle({ name: bundleForm.name.trim(), pack_ids: bundleForm.packIds }), '启动驱动集已创建', { reload })) {
      setBundleOpen(false);
      setBundleForm({ name: '', packIds: [] });
    }
  };

  const removeBundle = async (bundle) => {
    const ok = await conf.ask({ title: '删除驱动集', message: `确定删除 ${bundle.Name}？`, danger: true });
    if (ok) await run(() => api.deleteDriverBundle(bundle.ID), '驱动集已删除', { reload });
  };

  const downloadBundle = async (bundle) => {
    try {
      const res = await api.downloadDriverBundle(bundle.ID);
      const blob = await res.blob();
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url;
      a.download = `${bundle.Name}.zip`;
      a.click();
      URL.revokeObjectURL(url);
    } catch (e) { store.toast(e.message, 'err'); }
  };

  return (
    <div className="fade-in" style={{ display: 'flex', flexDirection: 'column', gap: 14 }}>
      <div className="tabs">
        <div className={`tab ${tab === 'packs' ? 'active' : ''}`} onClick={() => setTab('packs')}>
          驱动包 <span className="count">{packs.length}</span>
        </div>
        <div className={`tab ${tab === 'bundles' ? 'active' : ''}`} onClick={() => setTab('bundles')}>
          启动驱动集 <span className="count">{bundles.length}</span>
        </div>
        <div className={`tab ${tab === 'cure' ? 'active' : ''}`} onClick={() => setTab('cure')} style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
          驱动固化 {curePending && <span className="dot warn" title="固化会话进行中"/>}
        </div>
      </div>

      {tab === 'packs' && (
      <div className="card" style={{ padding: 0 }}>
        <div className="card-h">
          <div className="row" style={{ gap: 10 }}>
            <input className="input" placeholder="搜索名称 / 厂商" style={{ width: 200 }} value={q}
              onChange={e => { setQ(e.target.value); packPager.setPage(1); }}/>
            <Select value={catFilter} onChange={v => { setCatFilter(v); packPager.setPage(1); }} style={{ width: 130 }}
              options={[{ value: 'all', label: '全部类别' }, ...Object.entries(CATEGORY_LABELS).map(([value, label]) => ({ value, label }))]}/>
            {(q || catFilter !== 'all') && <span className="chip">{filteredPacks.length} / {packs.length}</span>}
          </div>
          <button className="btn primary" onClick={() => setUploadOpen(true)}><Icons.Upload size={12}/> 上传驱动包</button>
        </div>
        <table className="t">
          <thead><tr><th>名称</th><th>类别</th><th>厂商</th><th>版本</th><th>日期</th><th>HWID</th><th>签名</th><th>风险</th><th>状态</th><th className="t-actions"></th></tr></thead>
          <tbody>
            <TableState colSpan={10} loading={loading} error={error} empty={!filteredPacks.length}
              hint={packs.length ? '没有匹配的驱动包 · 调整筛选或搜索条件' : '暂无驱动包 · 先上传一个含 INF 的 zip'}/>
            {packPager.rows.map(({ pack, risks }) => (
              <tr key={pack.ID}>
                <td>
                  <div style={{ display: 'flex', alignItems: 'center', gap: 6 }}>
                    <span style={{ fontWeight: 500 }}>{pack.Name}</span>
                    {pack.Recommended && <span className="chip cyan" style={{ padding: "0 6px" }}>推荐</span>}
                  </div>
                </td>
                <td><span className={`chip ${pack.Category === 'boot_critical_nic' ? 'amber' : ''}`}>{CATEGORY_LABELS[pack.Category] || pack.Category}</span></td>
                <td className="muted">{pack.Vendor || '—'}</td>
                <td className="mono" style={{ fontSize: 12 }}>{pack.Version || '—'}</td>
                <td className="mono muted" style={{ fontSize: 12 }}>{pack.ReleaseDate || '—'}</td>
                <td title={(pack.HWIDs || []).join('\n')} className="mono" style={{ fontSize: 12 }}>{(pack.HWIDs || []).length}</td>
                <td>{pack.Signed ? <span className="chip emerald">已签名</span> : <span className="chip rose">未验证</span>}</td>
                <td>
                  {(risks || []).length === 0 ? <span className="muted">—</span> :
                    (risks || []).map(r => <span key={r} className="chip rose" style={{ marginRight: 4 }}>{RISK_LABELS[r] || r}</span>)}
                </td>
                <td>{pack.Status === 'enabled' ? <span className="chip emerald">启用</span> : <span className="chip">禁用</span>}</td>
                <td className="t-actions"><div>
                  <button className="btn" style={{ padding: '3px 8px' }} onClick={() => toggleStatus(pack)}>{pack.Status === 'enabled' ? '禁用' : '启用'}</button>
                  {!pack.Recommended && <button className="btn" style={{ padding: '3px 8px' }} onClick={() => recommend(pack)}>设推荐</button>}
                  <button className="btn ghost icon danger" onClick={() => removePack(pack)} title="删除驱动包" aria-label={`删除驱动包 ${pack.Name}`}><Icons.Trash size={12}/></button>
                </div></td>
              </tr>
            ))}
          </tbody>
        </table>
        <Pager {...packPager.pager}/>
      </div>
      )}

      {tab === 'bundles' && (
      <div className="card" style={{ padding: 0 }}>
        <div className="card-h">
          <div className="card-title"><Icons.Layers size={14}/> 启动驱动集 <span className="count">{bundles.length}</span></div>
          <button className="btn" onClick={() => setBundleOpen(true)}><Icons.Plus size={12}/> 组合驱动集</button>
        </div>
        <table className="t">
          <thead><tr><th>名称</th><th>系统</th><th>成员包</th><th>创建时间</th><th className="t-actions"></th></tr></thead>
          <tbody>
            <TableState colSpan={5} loading={loading} error={error} empty={!bundles.length} hint="暂无驱动集 · 组合启动网卡驱动后供超管机适配下载"/>
            {bundlePager.rows.map(({ bundle, packs: memberPacks }) => (
              <tr key={bundle.ID}>
                <td style={{ fontWeight: 500 }}>{bundle.Name}</td>
                <td className="muted">{bundle.OSType === 'windows' ? 'Windows' : 'Linux'}</td>
                <td className="muted" style={{ fontSize: 12 }}>{(memberPacks || []).map(p => p.Name).join('、') || '—'}</td>
                <td className="mono muted" style={{ fontSize: 12 }}>{(bundle.CreatedAt || '').slice(0, 10)}</td>
                <td className="t-actions"><div>
                  <button className="btn" style={{ padding: '3px 8px' }} onClick={() => downloadBundle(bundle)}><Icons.Download size={11}/> 下载</button>
                  <button className="btn ghost icon danger" onClick={() => removeBundle(bundle)} title="删除驱动集" aria-label={`删除驱动集 ${bundle.Name}`}><Icons.Trash size={12}/></button>
                </div></td>
              </tr>
            ))}
          </tbody>
        </table>
        <Pager {...bundlePager.pager}/>
      </div>
      )}

      {tab === 'cure' && <DriverCureCard bundles={bundles} superTerminal={superTerminal} conf={conf} reload={reload}/>}

      <Modal open={uploadOpen} onClose={() => setUploadOpen(false)} title="上传驱动包"
        footer={<>
          <button className="btn" onClick={() => setUploadOpen(false)}>取消</button>
          <button className="btn primary" disabled={busy} onClick={doUpload}>{busy ? '解析中…' : '上传并解析'}</button>
        </>}>
        <Field label="驱动 zip 文件" required hint="zip 内须包含至少一个 .inf；服务端自动解析厂商 / 版本 / HWID / 签名状态">
          <input className="input" type="file" accept=".zip" style={{ width: '100%' }}
            onChange={e => setUploadForm({ ...uploadForm, file: e.target.files[0] || null })}/>
        </Field>
        <Field label="名称" hint="留空则取文件名">
          <input className="input" style={{ width: '100%' }} value={uploadForm.name}
            onChange={e => setUploadForm({ ...uploadForm, name: e.target.value })} placeholder="Intel I219 v12.19"/>
        </Field>
        <Field label="类别" required>
          <Select value={uploadForm.category} onChange={v => setUploadForm({ ...uploadForm, category: v })}
            options={Object.entries(CATEGORY_LABELS).map(([value, label]) => ({ value, label }))}/>
        </Field>
        <Field label="客户机系统">
          <Select value={uploadForm.os_type} onChange={v => setUploadForm({ ...uploadForm, os_type: v })}
            options={[{ value: 'windows', label: 'Windows' }, { value: 'linux', label: 'Linux' }]}/>
        </Field>
      </Modal>

      <Modal open={bundleOpen} onClose={() => setBundleOpen(false)} title="组合启动驱动集"
        footer={<>
          <button className="btn" onClick={() => setBundleOpen(false)}>取消</button>
          <button className="btn primary" disabled={busy} onClick={doCreateBundle}>创建</button>
        </>}>
        <Field label="名称" required>
          <input className="input" style={{ width: '100%' }} value={bundleForm.name}
            onChange={e => setBundleForm({ ...bundleForm, name: e.target.value })} placeholder="2026 主流网卡启动集"/>
        </Field>
        <Field label="启动网卡驱动包" required hint="仅列出已启用的 boot_critical_nic 包">
          {bootPacks.length === 0
            ? <div className="muted" style={{ fontSize: 13 }}>暂无可用的启动网卡驱动包，请先上传并归类为“启动网卡”。</div>
            : bootPacks.map(({ pack }) => (
              <label key={pack.ID} style={{ display: 'flex', alignItems: 'center', gap: 8, padding: '6px 0', cursor: 'pointer', fontSize: 13 }}>
                <input type="checkbox" checked={bundleForm.packIds.includes(pack.ID)}
                  onChange={e => setBundleForm(f => ({
                    ...f,
                    packIds: e.target.checked ? [...f.packIds, pack.ID] : f.packIds.filter(id => id !== pack.ID),
                  }))}/>
                <span>{pack.Name}</span>
                <span className="mono muted meta">{(pack.HWIDs || []).length} HWID</span>
                {!pack.Signed && <span className="chip rose">未签名</span>}
              </label>
            ))}
        </Field>
      </Modal>
      {conf.node}
    </div>
  );
}

// 驱动固化：对唯一超管机走 注入 → 验证 → 固化。放在驱动中心是因为输入（驱动组）和产出
// （带驱动的新还原点）都归这里，客户机详情只留一个入口。
const pad2 = (n) => String(n).padStart(2, '0');
const fmtTime = (iso) => {
  if (!iso) return '—';
  const d = new Date(iso);
  if (isNaN(d)) return '—';
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
};

function DriverCureCard({ bundles, superTerminal, conf, reload }) {
  const store = useStore();
  const { busy, run } = useMutation(store.toast);
  const [bundleId, setBundleId] = useState('');
  const [reductionName, setReductionName] = useState('');
  const [result, setResult] = useState(null); // injectResult 返回内容
  const [checkError, setCheckError] = useState('');
  const [curing, setCuring] = useState(false); // 已提交 stop-super 任务

  const pendingId = superTerminal?.PendingBundleID || null;
  const pendingAt = superTerminal?.PendingBundleAt || null;
  const pending = !!(superTerminal && pendingId);
  const bundleName = (id) => bundles.find(b => b.bundle.ID === id)?.bundle.Name || id;
  const superLabel = superTerminal ? `${superTerminal.Name || superTerminal.ID}（${superTerminal.MAC}）` : '';

  const checkInstallResult = async () => {
    if (!superTerminal) return;
    try {
      setResult(await api.injectResult(superTerminal.ID));
      setCheckError('');
    } catch (e) {
      setCheckError(e.message || '检测失败');
    }
  };

  useEffect(() => {
    setResult(null);
    setCheckError('');
    setCuring(false);
    if (pending) checkInstallResult();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [superTerminal?.ID, pendingId, pendingAt]);

  // 第 2 步：轮询客户机内的安装结果，机器可能还在开机，不能靠手动「立即检查」。
  usePolling(checkInstallResult, { intervalMs: 5000, active: pending && !(result && result.done) });

  // 固化任务等关机、生成还原点期间保持同步，客户机退出超管后卡片回到第 1 步。
  usePolling(reload, { intervalMs: 5000, active: curing });

  const doInject = async () => {
    if (!bundleId) { store.toast('请选择启动驱动集', 'err'); return; }
    if (pendingId) {
      const sameBundle = pendingId === bundleId;
      const ok = await conf.ask({
        title: '覆盖尚未固化的驱动',
        message: `当前已注入「${bundleName(pendingId)}」（${fmtTime(superTerminal.PendingBundleAt)}），但还没做『停机存还原点』。继续${sameBundle ? '重新' : ''}注入会重建未固化的超管机系统盘——只有最后一次注入的驱动集会在下次开机生效，是否继续？`,
        danger: true, confirmText: '仍然覆盖注入',
      });
      if (!ok) return;
    }
    if (await run(() => api.injectDriver(superTerminal.ID, { bundle_id: bundleId, overwrite: pending }),
      '驱动集已注入超管机克隆盘', { reload })) {
      setBundleId('');
      setResult(null);
      setCheckError('');
    }
  };

  const doCure = async () => {
    if (!reductionName.trim()) { store.toast('请输入还原点名称', 'err'); return; }
    if (!result?.done || !result.ok) {
      store.toast('请先完成并通过开机验证后再进行固化', 'err');
      return;
    }
    if (await run(() => api.stopSuper(superTerminal.ID, { reduction_name: reductionName.trim() }),
      '停机存还原点任务已提交 · 任务将等待终端关机后固化', { reload })) {
      setReductionName('');
      setCuring(true);
    }
  };

  const active = !pending ? 1 : (result?.done && result.ok ? 3 : 2);
  return (
    <div className="card" style={{ padding: 0, borderColor: pending ? 'oklch(0.55 0.1 80 / 0.5)' : undefined }}>
      <div className="card-h">
        <div className="card-title"><Icons.Snapshot size={14}/> 驱动固化 · 注入超管机
          {pending && <span className="chip amber" style={{ marginLeft: 8 }}>会话进行中</span>}
        </div>
        {superTerminal && <span className="chip violet">超管机：{superLabel}</span>}
      </div>
      {!superTerminal ? (
        <div style={{ padding: 18, fontSize: 13, color: 'var(--fg-mute)', lineHeight: 1.7 }}>
          把启动驱动集固化进系统镜像需要一台<b>超管机</b>：注入驱动 → 开机自动安装 → 停机存为还原点，之后整组终端换用该还原点即获得新驱动。
          <div style={{ marginTop: 8 }}>
            <span className="row" style={{ color: 'var(--cyan)', cursor: 'pointer', display: 'inline-flex' }} onClick={() => store.goto('terminals')}>去客户机管理把一台机器设为超管<Icons.Chevron size={11}/></span>
          </div>
        </div>
      ) : (
        <div style={{ padding: 16, display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(240px, 1fr))', gap: 12 }}>
          <CureStep n={1} title="注入驱动集" state={pending ? 'done' : active === 1 ? 'active' : 'idle'}>
            {pending && <div style={{ marginBottom: 8 }}>已注入 <b>{bundleName(pendingId)}</b><div className="mono meta" style={{ marginTop: 2 }}>{fmtTime(superTerminal.PendingBundleAt)}</div></div>}
            <Select value={bundleId} onChange={setBundleId}
              options={[{ value: '', label: bundles.length ? '选择驱动集…' : '无驱动集 — 先在上方组合' },
                ...bundles.map(b => ({ value: b.bundle.ID, label: `${b.bundle.Name}（${(b.packs || []).length} 驱动包）` }))]}/>
            <button className="btn primary" disabled={busy || !bundles.length} style={{ marginTop: 8 }} onClick={doInject}>{busy ? '注入中…' : pending ? '覆盖注入' : '离线注入'}</button>
            {pending && <div className="hint" style={{ marginTop: 6 }}>可重新选择驱动集并覆盖当前未固化的注入结果</div>}
          </CureStep>
          <CureStep n={2} title="开机验证" state={!pending ? 'idle' : result?.done ? (result.ok ? 'done' : 'error') : checkError ? 'error' : 'active'}
            done={(result?.done && (result.ok
              ? <span className="row" style={{ color: 'var(--emerald)' }}><Icons.Check size={12}/> 驱动安装成功</span>
              : <span className="row" style={{ color: 'var(--rose)', whiteSpace: 'pre-wrap', alignItems: 'flex-start' }}><Icons.X size={12} style={{ flexShrink: 0, marginTop: 2 }}/> 安装失败 · {result.log}</span>))
              || (checkError && <span style={{ color: 'var(--rose)' }}>检测失败 · {checkError}</span>)}>
            {!pending
              ? <span style={{ color: 'var(--fg-faint)' }}>注入后，把超管机开机一次（自动装驱动，不会自动关机），装好后在客户机内确认再关机。</span>
              : <span>等待超管机开机安装…<span className="dot cyan" style={{ marginLeft: 6 }}/><div className="hint" style={{ marginTop: 6 }}>每 5 秒自动检测安装结果</div></span>}
            {result?.done && result.ok && result.log && <div className="mono hint" style={{ marginTop: 6, whiteSpace: 'pre-wrap' }}>{result.log}</div>}
          </CureStep>
          <CureStep n={3} title="停机存还原点" state={!pending ? 'idle' : active === 3 ? 'active' : 'idle'}>
            {!pending
              ? <span style={{ color: 'var(--fg-faint)' }}>验证通过后，确认超管机已关机，把改动固化为该配置的新还原点。</span>
              : <>
                  <input className="input" style={{ width: '100%' }} value={reductionName}
                    onChange={e => setReductionName(e.target.value)} placeholder="还原点名称，如 2026Q3-驱动更新"/>
                  <button className="btn primary" disabled={busy || curing || !result?.done || !result.ok} style={{ marginTop: 8 }} onClick={doCure}>
                    {curing ? '固化任务执行中…' : busy ? '提交中…' : '停机存还原点'}
                  </button>
                  <div className="hint" style={{ marginTop: 6 }}>需超管机处于关机状态；任务最多等待 2 分钟</div>
                </>}
          </CureStep>
        </div>
      )}
    </div>
  );
}

function CureStep({ n, title, state, done, children }) {
  const border = state === 'error' ? 'var(--rose)' : state === 'active' ? 'var(--cyan)' : state === 'done' ? 'oklch(0.55 0.1 155 / 0.5)' : 'var(--line-soft)';
  const chipClass = state === 'error' ? 'rose' : state === 'done' ? 'emerald' : state === 'active' ? 'cyan' : '';
  return (
    <div style={{ border: `1px solid ${border}`, borderRadius: 8, padding: 12, background: state === 'done' ? 'var(--emerald-soft)' : state === 'error' ? 'var(--rose-soft)' : 'var(--bg-0)', opacity: state === 'idle' ? 0.65 : 1 }}>
      <div style={{ display: 'flex', alignItems: 'center', gap: 8, marginBottom: 8 }}>
        <span className={`chip ${chipClass}`}>{state === 'done' ? <Icons.Check size={10}/> : state === 'error' ? <Icons.X size={10}/> : n}</span>
        <span style={{ fontSize: 13, fontWeight: 600 }}>{title}</span>
      </div>
      <div style={{ fontSize: 12, lineHeight: 1.6 }}>{done || children}</div>
    </div>
  );
}

window.PageDrivers = PageDrivers;

export { PageDrivers };
