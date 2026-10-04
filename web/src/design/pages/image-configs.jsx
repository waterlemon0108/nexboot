// 镜像管理的「配置」「还原点」两个标签页，以及它们的弹窗。只给 images.jsx 用。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, Select, TableState, SearchBox, Pager } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { redName, fmtDateTime } from '../../lib/format.js';
import { useResource, useMutation, usePaged } from '../../lib/hooks.js';
const { useState, useEffect } = React;

const RED_STATE = { ready: { t: "就绪", c: "emerald" }, creating: { t: "创建中", c: "cyan" }, error: { t: "错误", c: "rose" } };
const namesClause = (names) => names.slice(0, 5).join("、") + (names.length > 5 ? ` 等 ${names.length} 个` : "");
const byTime = (a, b) => (Date.parse(a.CreatedAt) || 0) - (Date.parse(b.CreatedAt) || 0);
// 与后端同一条规则：配置正在用的、或只剩的那个还原点删不得。快照已坏的不在此列，交给后端判。
const deleteLock = (rp) => {
  if (rp.Status !== "ready") return "";
  if (rp.cfg.reductions.length <= 1) return `这是配置 ${rp.cfg.Name} 唯一的还原点，删了就没有可开机的内容；请先新建一个还原点`;
  if (rp.cfg.DefaultReductionID === rp.ID) return `这是配置 ${rp.cfg.Name} 当前应用的还原点，请先应用其它还原点再删除`;
  return "";
};
const latestOf = (reds) => reds.filter(r => r.Status === "ready").sort(byTime).pop() || null;
const items = (r) => (r && r.items) || [];

async function listGroups() {
  try { return items(await api.listGroups()); } catch { return []; }
}

// loadConfigUsers 按配置列出使用者：用作系统盘的分组（{ group }）和挂成数据盘的分组（{ group, mount }）。
export async function loadConfigUsers() {
  const groups = await listGroups();
  const disks = await Promise.all(groups.map(g => api.listGroupDisks(g.ID).then(items).catch(() => [])));
  return (configId) => [
    ...groups.filter(g => g.SystemConfigID === configId).map(g => ({ group: g.Name })),
    ...disks.flatMap((ds, i) => ds.filter(d => d.ConfigID === configId).map(d => ({ group: groups[i].Name, mount: d.MountTarget }))),
  ];
}

export const usageLabel = (u) => (u.mount ? `${u.group}（数据盘 ${u.mount}）` : u.group);

// 只列前两个，其余收成 +N；整格悬停看完整名单。
export function UsageCell({ users, empty = "—" }) {
  if (!users.length) return <span className="muted">{empty}</span>;
  const shown = users.slice(0, 2);
  return (
    <span className="row" style={{ gap: 6, whiteSpace: "nowrap" }} title={users.map(usageLabel).join("\n")}>
      <span>{shown.map((u, i) => (
        <React.Fragment key={i}>{i > 0 && "、"}{u.group}{u.mount && <span className="meta"> {u.mount}（数据盘）</span>}</React.Fragment>
      ))}</span>
      {users.length > 2 && <span className="chip">+{users.length - 2}</span>}
    </span>
  );
}

// 两个标签页的全部数据：每个镜像的配置及其还原点，加上使用它们的分组。镜像不多，一次取完。
function useCatalogue(images) {
  const key = images.map(i => i.ID).join(",");
  return useResource(async () => {
    const configs = (await Promise.all(images.map(img => api.listConfigs(img.ID).then(items).catch(() => []))))
      .flat();
    const reds = await Promise.all(configs.map(c => api.listReductions(c.ID).then(items).catch(() => [])));
    const usersOf = await loadConfigUsers();
    return configs.map((c, i) => {
      const users = usersOf(c.ID);
      return { ...c, reductions: reds[i].sort(byTime), users, groups: users.map(usageLabel) };
    });
  }, [key]);
}

// ── 配置页 ────────────────────────────────────────────────────────────────────
export function ConfigsTab({ images, onShowReductions }) {
  const store = useStore();
  const conf = useConfirm();
  const { busy, run } = useMutation(store.toast);
  const [imageF, setImageF] = useState("");
  const [q, setQ] = useState("");
  const [modal, setModal] = useState(null);
  const { data, loading, error, reload } = useCatalogue(images);
  const all = data || [];
  const imageName = (id) => images.find(i => i.ID === id)?.Name || id;
  const rows = all.filter(c => (!imageF || c.ImageID === imageF) && (!q || c.Name.toLowerCase().includes(q.toLowerCase())));
  const paged = usePaged(rows);

  const doDelete = async (cfg) => {
    const message = cfg.groups.length
      ? `配置 ${cfg.Name} 正被分组 ${namesClause(cfg.groups)} 用作启动配置，删除会失败。\n请先在分组中改绑其他配置。`
      : `删除配置 ${cfg.Name}？它的 ${cfg.reductions.length} 个还原点将一并删除。`;
    if (!(await conf.ask({ title: "删除配置", message, danger: true }))) return;
    await run(() => api.deleteConfig(cfg.ID), "配置删除任务已提交", { reload, errMsg: "删除失败" });
  };

  return (
    <>
      <div className="row" style={{ gap: 8 }}>
        <Select aria-label="按镜像筛选" value={imageF} onChange={setImageF} style={{ width: 180 }}
          options={[{ value: "", label: "全部镜像" }, ...images.map(i => ({ value: i.ID, label: i.Name }))]}/>
        <SearchBox value={q} onChange={setQ} placeholder="搜索配置名称"/>
        <div style={{ flex: 1 }}/>
        <button className="btn primary" onClick={() => setModal({ kind: "config" })}><Icons.Plus size={12}/> 新建配置</button>
      </div>
      <div className="card" style={{ padding: 0 }}>
        <div style={{ overflowX: "auto" }}><table className="t">
          <thead><tr><th>配置</th><th>所属镜像</th><th>当前还原点</th><th>还原点</th><th>使用中的分组</th><th>创建时间</th><th className="t-actions"></th></tr></thead>
          <tbody>
            {paged.rows.map(cfg => {
              const current = cfg.reductions.find(r => r.ID === cfg.DefaultReductionID);
              const latest = latestOf(cfg.reductions);
              const onlyConfig = !all.some(c => c.ImageID === cfg.ImageID && c.ID !== cfg.ID);
              return (
                <tr key={cfg.ID}>
                  <td style={{ fontWeight: 600 }}>{cfg.Name}</td>
                  <td>{imageName(cfg.ImageID)}</td>
                  <td>
                    <span className="row" style={{ gap: 6 }}>
                      {current ? redName(current) : "—"}
                      {current && latest && current.ID !== latest.ID && <span className="chip amber" title={`最新的是 ${redName(latest)}`}>非最新</span>}
                    </span>
                  </td>
                  <td><button className="btn ghost" style={{ padding: "2px 6px" }} onClick={() => onShowReductions(cfg.ImageID, cfg.ID)}>{cfg.reductions.length} 个</button></td>
                  <td style={{ maxWidth: 260, overflow: "hidden", textOverflow: "ellipsis" }}><UsageCell users={cfg.users}/></td>
                  <td className="mono muted" style={{ fontSize: 12 }}>{fmtDateTime(cfg.CreatedAt)}</td>
                  <td className="t-actions"><div>
                    <button className="btn ghost icon danger" disabled={onlyConfig} onClick={() => doDelete(cfg)} aria-label={`删除配置 ${cfg.Name}`}
                      title={onlyConfig ? `这是镜像 ${imageName(cfg.ImageID)} 的最后一个配置，不能删除；不要这个镜像了请直接删除镜像` : "删除配置"}><Icons.Trash size={12}/></button>
                  </div></td>
                </tr>
              );
            })}
            <TableState colSpan={7} loading={loading && !data} error={error} empty={!rows.length} hint="暂无配置 · 点右上角「新建配置」"/>
          </tbody>
        </table></div>
        <Pager {...paged.pager}/>
      </div>
      {modal?.kind === "config" && <NewConfigModal images={images} catalogue={all} defaultImage={imageF} busy={busy} run={run} onClose={() => setModal(null)} onDone={reload}/>}
      {conf.node}
    </>
  );
}

// 新建配置：起点二选一。选已有配置的还原点时，那个还原点就不能再删，这里说一次。
function NewConfigModal({ images, catalogue, defaultImage, busy, run, onClose, onDone }) {
  const store = useStore();
  const [form, setForm] = useState({ name: "", imageId: defaultImage || images[0]?.ID || "", from: "image", configId: "", reductionId: "" });
  const configsOf = catalogue.filter(c => c.ImageID === form.imageId);
  const src = configsOf.find(c => c.ID === form.configId) || configsOf[0];
  const reds = src ? src.reductions : [];
  useEffect(() => {
    if (form.from !== "config" || !src) return;
    if (form.configId !== src.ID || !reds.some(r => r.ID === form.reductionId)) {
      setForm(f => ({ ...f, configId: src.ID, reductionId: src.DefaultReductionID || reds[reds.length - 1]?.ID || "" }));
    }
  }, [form.from, form.imageId, form.configId]); // eslint-disable-line

  const submit = async () => {
    const name = form.name.trim();
    if (!name) { store.toast("请输入配置名称", "err"); return; }
    if (!form.imageId) { store.toast("请选择所属镜像", "err"); return; }
    const ok = form.from === "image"
      ? await run(() => api.createConfigFromImage(form.imageId, { name }), "配置创建任务已提交", { reload: onDone, errMsg: "创建失败" })
      : form.reductionId
        ? await run(() => api.forkConfig(form.configId, { name, reduction_id: form.reductionId }), "配置创建任务已提交", { reload: onDone, errMsg: "创建失败" })
        : (store.toast("请选择起点还原点", "err"), false);
    if (ok) onClose();
  };

  return (
    <Modal open onClose={() => !busy && onClose()} title="新建配置"
      footer={<><button className="btn" disabled={busy} onClick={onClose}>取消</button><button className="btn primary" disabled={busy} onClick={submit}>{busy ? "提交中…" : "创建"}</button></>}>
      <Field label="配置名称" required>
        <input className="input" aria-label="配置名称" style={{ width: "100%" }} value={form.name} onChange={e => setForm({ ...form, name: e.target.value })} placeholder="例如 教学机-办公版"/>
      </Field>
      <Field label="所属镜像" required>
        <Select aria-label="所属镜像" value={form.imageId} onChange={v => setForm({ ...form, imageId: v, configId: "", reductionId: "" })}
          options={images.map(i => ({ value: i.ID, label: i.Name }))}/>
      </Field>
      <Field label="起点" required>
        <div style={{ display: "flex", flexDirection: "column", gap: 8, fontSize: 13 }}>
          <label className="row" style={{ gap: 8, cursor: "pointer" }}>
            <input type="radio" name="from" checked={form.from === "image"} onChange={() => setForm({ ...form, from: "image" })}/>
            镜像的原始内容（导入时的状态）
          </label>
          <label className="row" style={{ gap: 8, cursor: "pointer" }}>
            <input type="radio" name="from" checked={form.from === "config"} disabled={!configsOf.length} onChange={() => setForm({ ...form, from: "config" })}/>
            已有配置的某个还原点
          </label>
          {form.from === "config" && src && (
            <div className="row" style={{ gap: 8, paddingLeft: 22 }}>
              <Select aria-label="起点配置" value={src.ID} onChange={v => setForm({ ...form, configId: v, reductionId: "" })} style={{ width: 160 }}
                options={configsOf.map(c => ({ value: c.ID, label: c.Name }))}/>
              <Select aria-label="起点还原点" value={form.reductionId} onChange={v => setForm({ ...form, reductionId: v })} style={{ width: 200 }}
                options={reds.map(r => ({ value: r.ID, label: redName(r) + (r.ID === src.DefaultReductionID ? "（当前）" : "") }))}/>
            </div>
          )}
        </div>
      </Field>
      <div className="hint">新配置与起点互不影响，各改各的。{form.from === "config" && "只要新配置还在，作为起点的那个还原点就不能删除。"}</div>
    </Modal>
  );
}

// ── 还原点页 ──────────────────────────────────────────────────────────────────
// 默认列出全部镜像、全部配置的还原点；镜像 / 配置两个筛选从配置页或详情跳来时带上。
export function RestorePointsTab({ images, imageId, configId, onPick }) {
  const store = useStore();
  const conf = useConfirm();
  const { busy, run } = useMutation(store.toast);
  const [modal, setModal] = useState(null);
  const [selected, setSelected] = useState([]);
  const [q, setQ] = useState("");
  const { data, loading, error, reload } = useCatalogue(images);
  const catalogue = data || [];
  const imageOrder = (id) => images.findIndex(i => i.ID === id);
  const imageOf = (id) => images.find(i => i.ID === id) || null;
  const cfgOf = (id) => catalogue.find(c => c.ID === id) || null;
  const configChoices = catalogue.filter(c => !imageId || c.ImageID === imageId);
  const rows = catalogue
    .filter(c => (!imageId || c.ImageID === imageId) && (!configId || c.ID === configId))
    .sort((a, b) => imageOrder(a.ImageID) - imageOrder(b.ImageID) || byTime(a, b))
    .flatMap(c => c.reductions.map(r => ({ ...r, cfg: c })))
    .filter(r => !q || redName(r).toLowerCase().includes(q.toLowerCase()));
  const paged = usePaged(rows);
  useEffect(() => { setSelected([]); }, [imageId, configId, q, paged.page, paged.size]);
  // 已勾选的还原点若被应用或删除就移出勾选，免得解锁后又带着勾回来。
  const chosen = rows.filter(r => selected.includes(r.ID) && !deleteLock(r));
  useEffect(() => {
    if (chosen.length !== selected.length) setSelected(chosen.map(r => r.ID));
  }, [data]); // eslint-disable-line react-hooks/exhaustive-deps

  const apply = (rp) => run(() => api.applyReduction(rp.ID), `已应用 ${redName(rp)}，客户机下次开机生效`, { reload, errMsg: "应用失败" });

  const confirmDelete = async (rp) => {
    const refs = (await listGroups()).filter(g => g.SystemReductionID === rp.ID).map(g => g.Name);
    const warnings = [];
    if (refs.length) warnings.push(`正被分组 ${namesClause(refs)} 用作还原点，删除会失败`);
    const message = warnings.length ? `还原点 ${redName(rp)}：${warnings.join("；")}。确定删除？` : `删除还原点 ${redName(rp)}？`;
    if (!(await conf.ask({ title: "删除还原点", message, danger: true, confirmText: "删除" }))) return;
    await run(() => api.deleteReduction(rp.ID), "还原点删除任务已提交", { reload, errMsg: "删除失败" });
  };

  const deleteSelected = async () => {
    const picked = chosen;
    if (!(await conf.ask({ title: "删除还原点", message: `删除 ${picked.length} 个还原点：${namesClause(picked.map(r => `${r.cfg.Name}/${redName(r)}`))}？删除后无法恢复。`, danger: true, confirmText: "删除" }))) return;
    const failed = [];
    for (const rp of picked) {
      try { await api.deleteReduction(rp.ID); } catch (e) { failed.push(`${redName(rp)}：${e.message || "删除失败"}`); }
    }
    if (failed.length) store.toast(failed.join("\n"), "err");
    else store.toast(`已提交删除 ${picked.length} 个还原点`, "ok");
    setSelected([]);
    reload();
  };

  const overwrite = async (rp) => {
    const image = imageOf(rp.cfg.ImageID);
    const siblings = catalogue.filter(c => c.ImageID === rp.cfg.ImageID && c.ID !== rp.cfg.ID).map(c => c.Name);
    const later = rp.cfg.reductions.filter(r => byTime(r, rp) > 0).map(redName);
    const message = [
      `用还原点 ${redName(rp)} 的内容替换镜像 ${image?.Name || rp.cfg.ImageID} 的原始内容。`,
      siblings.length ? `镜像下的其它配置（${namesClause(siblings)}）会被删除。` : null,
      `配置 ${rp.cfg.Name} 的全部还原点会被删除，只留一个新的起点${later.length ? `，比它新的 ${namesClause(later)} 也在其中` : ""}。`,
      "使用这个配置的分组自动改用新的起点。此操作不可恢复。",
    ].filter(Boolean).join("\n");
    if (!(await conf.ask({ title: "覆盖原镜像", message, danger: true, confirmText: "覆盖" }))) return;
    await run(() => api.overwriteImage(rp.ID), "覆盖原镜像任务已提交", { reload, errMsg: "覆盖失败" });
  };

  const pageIds = paged.rows.filter(r => !deleteLock(r)).map(r => r.ID);
  const allOnPage = pageIds.length > 0 && pageIds.every(id => chosen.some(r => r.ID === id));
  return (
    <>
      <div className="row" style={{ gap: 8 }}>
        <Select aria-label="按镜像筛选" value={imageId || ""} onChange={v => onPick(v, "")} style={{ width: 170 }}
          options={[{ value: "", label: "全部镜像" }, ...images.map(i => ({ value: i.ID, label: i.Name }))]}/>
        <Select aria-label="按配置筛选" value={configId || ""} onChange={v => onPick(imageId, v)} style={{ width: 170 }}
          options={[{ value: "", label: "全部配置" }, ...configChoices.map(c => ({ value: c.ID, label: imageId ? c.Name : `${imageOf(c.ImageID)?.Name || c.ImageID} / ${c.Name}` }))]}/>
        <SearchBox value={q} onChange={setQ} placeholder="搜索还原点名称" width={200}/>
        <div style={{ flex: 1 }}/>
        <button className="btn danger" disabled={!chosen.length} onClick={deleteSelected}><Icons.Trash size={12}/> 删除所选{chosen.length ? ` (${chosen.length})` : ""}</button>
      </div>
      <div className="card" style={{ padding: 0 }}>
        <div style={{ overflowX: "auto" }}><table className="t">
          <thead><tr>
            <th style={{ width: 36 }}><input type="checkbox" aria-label="选择本页全部还原点" checked={allOnPage} disabled={!pageIds.length} onChange={e => setSelected(e.target.checked ? pageIds : [])}/></th>
            <th>还原点</th><th>镜像</th><th>配置</th><th>状态</th><th>创建时间</th><th className="t-actions"></th>
          </tr></thead>
          <tbody>
            {paged.rows.map(rp => {
              const current = rp.cfg.DefaultReductionID === rp.ID;
              const latest = latestOf(rp.cfg.reductions);
              const rs = RED_STATE[rp.Status] || { t: rp.Status || "—", c: "" };
              const ready = rp.Status === "ready";
              const name = redName(rp);
              const lock = deleteLock(rp);
              return (
                <tr key={rp.ID}>
                  <td><input type="checkbox" aria-label={`选择还原点 ${name}`} checked={!lock && selected.includes(rp.ID)} disabled={!!lock} title={lock || undefined}
                    onChange={() => setSelected(s => s.includes(rp.ID) ? s.filter(x => x !== rp.ID) : [...s, rp.ID])}/></td>
                  <td>
                    <span className="row" style={{ gap: 6, whiteSpace: "nowrap" }}>
                      <span className="ellip" style={{ fontWeight: 600, maxWidth: 140 }} title={name}>{name}</span>
                      {current && <span className="chip cyan" title="客户机开机使用的还原点">应用中</span>}
                      {latest?.ID === rp.ID && <span className="meta">最新</span>}
                    </span>
                    {rp.Remark && <div className="hint">{rp.Remark}</div>}
                  </td>
                  <td>{imageOf(rp.cfg.ImageID)?.Name || rp.cfg.ImageID}</td>
                  <td>{rp.cfg.Name}</td>
                  <td><span className={`chip ${rs.c}`}>{rs.t}</span></td>
                  <td className="mono muted" style={{ fontSize: 12 }}>{fmtDateTime(rp.CreatedAt)}</td>
                  <td className="t-actions"><div>
                    {!current && ready && <button className="btn" style={{ padding: "3px 8px" }} disabled={busy} onClick={() => apply(rp)} aria-label={`应用 ${name}`}>应用</button>}
                    <button className="btn" style={{ padding: "3px 8px" }} disabled={!ready} onClick={() => setModal({ kind: "saveAs", rp })} aria-label={`另存为新镜像 ${name}`}>另存为新镜像</button>
                    <button className="btn" style={{ padding: "3px 8px" }} disabled={!ready} onClick={() => setModal({ kind: "export", rp })} aria-label={`导出为镜像文件 ${name}`}>导出为镜像文件</button>
                    <MoreMenu label={`更多操作 ${name}`} items={[
                      { label: "覆盖原镜像", danger: true, disabled: !ready, onClick: () => overwrite(rp) },
                      { label: "删除", danger: true, disabled: !!lock, title: lock, onClick: () => confirmDelete(rp) },
                    ]}/>
                  </div></td>
                </tr>
              );
            })}
            <TableState colSpan={7} loading={loading && !data} error={error} empty={!rows.length} hint={catalogue.length ? "没有符合条件的还原点" : "暂无还原点"}/>
          </tbody>
        </table></div>
        <Pager {...paged.pager}/>
      </div>
      {modal?.kind === "saveAs" && <SaveAsImageModal rp={modal.rp} image={imageOf(modal.rp.cfg.ImageID)} busy={busy} run={run} onClose={() => setModal(null)}/>}
      {modal?.kind === "export" && <ExportModal busy={busy} run={run} onClose={() => setModal(null)}
        title={`导出还原点 · ${imageOf(modal.rp.cfg.ImageID)?.Name || modal.rp.cfg.ImageID} / ${redName(modal.rp)}`}
        source="导出内容为该还原点的完整磁盘数据。"
        ticket={(gzip) => api.exportReductionTicket(modal.rp.ID, gzip)}
        toDir={() => api.exportReduction(modal.rp.ID)}/>}
      {conf.node}
    </>
  );
}

// 页内的「更多」下拉，与 servers.jsx 的 Tip 同一种浮层写法。
function MoreMenu({ label, items: entries }) {
  const [open, setOpen] = useState(false);
  return (
    <span style={{ position: "relative", display: "inline-flex" }}>
      <button className="btn ghost" style={{ padding: "3px 8px" }} aria-label={label} onClick={() => setOpen(v => !v)}>更多 ▾</button>
      {open && (
        <>
          <span onClick={() => setOpen(false)} style={{ position: "fixed", inset: 0, zIndex: "var(--z-popover)" }}/>
          <span className="card" style={{ position: "absolute", top: 28, right: 0, zIndex: "var(--z-popover)", padding: 4, minWidth: 120,
            display: "flex", flexDirection: "column", boxShadow: "0 8px 24px oklch(0 0 0 / 0.45)" }}>
            {entries.map(e => (
              <button key={e.label} className={`btn ghost${e.danger ? " danger" : ""}`} disabled={e.disabled} title={e.title || undefined} style={{ justifyContent: "flex-start" }}
                onClick={() => { setOpen(false); e.onClick(); }}>{e.label}</button>
            ))}
          </span>
        </>
      )}
    </span>
  );
}

function SaveAsImageModal({ rp, image, busy, run, onClose }) {
  const [name, setName] = useState(`${image?.Name || "镜像"}-${redName(rp)}`);
  const submit = async () => {
    if (!name.trim()) return;
    if (await run(() => api.saveReductionAsImage(rp.ID, { name: name.trim() }), "另存为新镜像任务已提交，进度见任务中心", { errMsg: "另存失败" })) onClose();
  };
  return (
    <Modal open onClose={() => !busy && onClose()} title={`另存为新镜像 · ${redName(rp)}`} size="sm"
      footer={<><button className="btn" disabled={busy} onClick={onClose}>取消</button><button className="btn primary" disabled={busy || !name.trim()} onClick={submit}>{busy ? "提交中…" : "另存"}</button></>}>
      <Field label="新镜像名称" required>
        <input className="input" aria-label="新镜像名称" style={{ width: "100%" }} value={name} onChange={e => setName(e.target.value)}/>
      </Field>
      <div className="hint">用这个还原点的内容完整复制一个新镜像，自带默认配置；和原镜像互不依赖，以后可以各自删除。复制要写一整份数据，大镜像需要几分钟。</div>
    </Modal>
  );
}

// ExportModal 是镜像与还原点共用的导出弹窗：两者导出的都是可再导入的 ZFS 数据流，差别只在来源。
// ticket(gzip) 换取一次性下载地址；toDir 可选，提供时显示「导出到服务器目录」。
export function ExportModal({ title, source, ticket, toDir, busy, run, onClose }) {
  const store = useStore();
  const [gzip, setGzip] = useState(false);
  // 用票据换普通 URL 交给浏览器下载；fetch+blob 会把整个镜像读进内存。
  const download = async () => {
    const ok = await run(async () => {
      const t = await ticket(gzip);
      const a = document.createElement("a");
      a.href = t.url;
      document.body.appendChild(a);
      a.click();
      a.remove();
      return t;
    }, "下载已开始，进度可在浏览器下载列表中查看", { errMsg: "导出失败" });
    if (ok) onClose();
  };
  const exportToDir = async () => {
    let r;
    const ok = await run(async () => (r = await toDir()), null, { errMsg: "导出失败" });
    if (ok) {
      store.toast(`导出任务已提交，目标文件：${r.node || "当前主机"} 上的 ${r.path}`, "ok");
      onClose();
    }
  };
  return (
    <Modal open onClose={() => !busy && onClose()} title={title} size="sm">
      <div className="hint" style={{ marginBottom: 12 }}>
        {source}文件格式为 ZFS 数据流（.zfs，启用压缩时为 .zfs.gz），可在任一服务器的「导入镜像」中导入。
      </div>
      <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
        <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13, cursor: "pointer" }}>
          <input type="checkbox" checked={gzip} disabled={busy} onChange={e => setGzip(e.target.checked)} aria-label="gzip 压缩"/>
          gzip 压缩
        </label>
        <span className="hint" style={{ marginTop: -4 }}>
          数据流已包含存储池压缩，启用 gzip 通常可再减小 30%–50% 的体积，但会增加服务器 CPU 开销。导入时自动识别压缩格式。
        </span>
        <div className="row" style={{ gap: 10, alignItems: "flex-start" }}>
          <button className="btn primary" style={{ flexShrink: 0, minWidth: 128 }} disabled={busy} onClick={download}>
            <Icons.Download size={12}/> 下载到本地
          </button>
          <span className="hint" style={{ paddingTop: 4 }}>通过浏览器下载，进度可在浏览器下载列表中查看；传输中断后需重新下载。</span>
        </div>
        {toDir && (
          <div className="row" style={{ gap: 10, alignItems: "flex-start" }}>
            <button className="btn" style={{ flexShrink: 0, minWidth: 128 }} disabled={busy} onClick={exportToDir}>
              <Icons.Server size={12}/> 导出到服务器目录
            </button>
            <span className="hint" style={{ paddingTop: 4 }}>
              写入当前主机的导入目录，不经网络传输，适用于大容量镜像。此方式不支持 gzip 压缩；导出期间如发生主备切换，任务将失败，可在「任务中心」查看。
            </span>
          </div>
        )}
      </div>
    </Modal>
  );
}
