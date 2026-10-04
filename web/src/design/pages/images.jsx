// 三级模型：镜像 → 配置 → 还原点。镜像靠导入（或新建空数据盘）得到；后端没有发布、回滚、
// 设默认的接口，页面也不提供这些操作。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { Modal, useConfirm } from '../overlay.jsx';
import { Field, Select, TableState, SearchBox } from '../primitives.jsx';
import { ConfigsTab, RestorePointsTab, loadConfigUsers, UsageCell } from './image-configs.jsx';
import { api } from '../../lib/api.js';
import { fmtDateTime, redName } from '../../lib/format.js';
import { useResource, useMutation, usePolling } from '../../lib/hooks.js';
const { useState, useEffect, useRef } = React;

// 尽力列出引用该对象的分组，删除确认时直接点名；查询失败返回空列表，最终以后端判定为准。
async function groupsReferencing(pick) {
  try {
    const r = await api.listGroups();
    return ((r && r.items) || []).filter(pick).map(g => g.Name);
  } catch {
    return [];
  }
}
const namesClause = (names) => names.slice(0, 5).join("、") + (names.length > 5 ? ` 等 ${names.length} 个` : "");

const GiB = 1024 ** 3;
const fmtGiB = (b) => `${(Number(b || 0) / GiB).toFixed(1)} GiB`;
const normalizeImageName = (v) => {
  v = (v || "").trim().toLowerCase();
  let out = "";
  for (const ch of v) {
    if ((ch >= "a" && ch <= "z") || (ch >= "0" && ch <= "9") || ch === "_" || ch === "-") {
      out += ch;
    } else if (/\s/.test(ch)) {
      if (out && !out.endsWith("-")) out += "-";
    } else if (out && !out.endsWith("_")) {
      out += "_";
    }
  }
  return out.replace(/^[-_]+|[-_]+$/g, "");
};
const IMG_STATE = { normal: { t: "就绪", c: "emerald" }, importing: { t: "导入中", c: "cyan" }, error: { t: "错误", c: "rose" } };
// 颜色只表示用途（系统盘青、数据盘紫）；系统靠图标形状区分，标签保持中性。
const OS_META = { windows: { t: "Windows" }, linux: { t: "Linux" } };
// 镜像用途：系统盘用来启动，数据盘是挂在旁边的已格式化卷，两者在不同地方选用。
const PURPOSE_META = { system: { t: "系统盘镜像", c: "cyan" }, data: { t: "数据盘镜像", c: "violet" } };
const purposeOf = (img) => (img.Purpose === "data" ? "data" : "system");
const FS_META = { ntfs: { t: "NTFS（Windows）" }, ext4: { t: "ext4（Linux）" } };

function PageImages() {
  const store = useStore();
  const conf = useConfirm();
  const { route, goto } = store;

  const [tab, setTab] = useState("images"); // images | configs | reductions
  const [purposeF, setPurposeF] = useState("all");
  const [osF, setOsF] = useState("all");
  const [q, setQ] = useState("");
  const [redPick, setRedPick] = useState({ imageId: "", configId: "" });
  const [selImageId, setSelImageId] = useState(null);

  const [modal, setModal] = useState(null); // import | image | export | blank
  // 默认不压缩：裸流已带池的压缩，局域网里 gzip 反而更慢。
  const [exportGzip, setExportGzip] = useState(false);
  // 两条进出路径并存：浏览器受运维上行带宽限制；服务器目录快得多，但文件只在当前服务的那台上。
  const [importMode, setImportMode] = useState("upload");
  const [sources, setSources] = useState(null);
  const [form, setForm] = useState({});
  const [importDir, setImportDir] = useState("");
  const [importPoolAvail, setImportPoolAvail] = useState(null); // 字节；null 表示取不到池空间
  const [up, setUp] = useState(null); // { name, size, sent, state, error }
  const uploadIdRef = React.useRef("");
  // 当前这次上传的 AbortController 兼作令牌：放弃、关弹窗或重选文件后旧循环立刻失效，不再发片、不再改进度。
  const uploadCtlRef = React.useRef(null);
  const stopUpload = () => { if (uploadCtlRef.current) uploadCtlRef.current.abort(); uploadCtlRef.current = null; };
  useEffect(() => stopUpload, []);
  // 集群下要说清文件传到哪台：上传只能落在当前主机（备机写闸门关闭），运维只看得到 VIP。
  const nodesRes = useResource(() => api.listClusterNodes().catch(() => null), []);
  const nodes = (nodesRes.data && nodesRes.data.items) || [];
  const activeNode = nodes.find(n => n.ha_state === "active");
  const importPlaceholder = `${importDir || "/tank/imports"}/win11.vmdk`;

  const { data: imgData, loading, error, reload: loadImages } = useResource(() => api.listImages(), []);
  // 后端按 ID（取自名称）排；这里系统盘在前、数据盘在后，各自按创建时间先后。
  const images = [...((imgData && imgData.items) || [])].sort((a, b) =>
    (purposeOf(a) === purposeOf(b) ? 0 : purposeOf(a) === "system" ? -1 : 1)
    || ((Date.parse(a.CreatedAt) || 0) - (Date.parse(b.CreatedAt) || 0))
    || String(a.Name || "").localeCompare(String(b.Name || "")));
  const { busy, run } = useMutation(store.toast);

  const filtered = images.filter(i => (purposeF === "all" || purposeOf(i) === purposeF)
    && (osF === "all" || i.OSType === osF)
    && (!q || String(i.Name || "").toLowerCase().includes(q.toLowerCase())));
  const showReductions = (imageId, configId) => { setRedPick({ imageId, configId }); setModal(null); setTab("reductions"); };
  const selImage = images.find(i => i.ID === selImageId) || null;

  // 有镜像在导入时轮询列表，让卡片自动变成就绪；卡片进度条用 store.tasks（已 3 秒轮询）。
  usePolling(loadImages, { intervalMs: 3000, active: images.some(i => i.State === "importing") });
  const importTaskOf = (imageId) =>
    store.tasks.find(k => k.target === imageId && (k.status === "running" || k.status === "waiting"));

  // 筛选结果变化时保持选中项有效
  useEffect(() => {
    if (!filtered.length) { if (selImageId) setSelImageId(null); return; }
    if (!filtered.some(i => i.ID === selImageId)) setSelImageId(filtered[0].ID);
  }, [images, purposeF, osF, q]); // eslint-disable-line

  // 来自全局搜索或分组页的深链，每次跳转只消费一次；按 params 对象认，再跳同一镜像仍会打开。
  // images 每次渲染都是新数组，不能当依赖，否则详情关掉马上又被弹开。
  const imagesLoaded = !!imgData;
  const deepLinkSeen = useRef(null);
  useEffect(() => {
    if (!route.params.imageId || !imagesLoaded || deepLinkSeen.current === route.params) return;
    deepLinkSeen.current = route.params;
    if (images.some(i => i.ID === route.params.imageId)) {
      setSelImageId(route.params.imageId);
      setModal("image");
    }
  }, [route.params, imagesLoaded]); // eslint-disable-line

  // ── 镜像操作 ──────────────────────────────────────────────────────────────
  const baseName = (n) => (n || "").split("/").pop().replace(/\.(zfs\.gz|zfs|gzip|vmdk|vhdx|vhd|qcow2|qcow|vdi|raw|img)$/i, "");
  const openImageDetail = (imageId) => {
    setSelImageId(imageId);
    setModal("image");
  };
  const setPurpose = async (purpose) => {
    if (!selImage || purpose === purposeOf(selImage)) return;
    await run(() => api.setImagePurpose(selImage.ID, { purpose }), `${selImage.Name} 已改为${PURPOSE_META[purpose].t}`, { reload: loadImages, errMsg: "修改用途失败" });
  };
  const openBlank = () => { setForm({ name: "", size_gib: 100, filesystem: "ntfs", label: "" }); setModal("blank"); };
  const doCreateBlank = async () => {
    const name = (form.name || "").trim();
    if (!name) { store.toast("请输入数据盘名称", "err"); return; }
    const gib = Number(form.size_gib);
    if (!(gib >= 1)) { store.toast("大小至少 1 GiB", "err"); return; }
    // 名称原样提交，服务端折成 ASCII 数据集 ID；卷标只在填写时提交，留空即同名称。
    const body = { name, size_bytes: Math.round(gib * GiB), filesystem: form.filesystem || "ntfs" };
    const label = (form.label || "").trim();
    if (label) body.label = label;
    if (await run(() => api.createBlankImage(body),
      `数据盘 ${name} 创建任务已提交`, { reload: loadImages, errMsg: "创建失败" })) setModal(null);
  };
  const openImport = async () => {
    setForm({ name: "", source_path: "", os_type: "windows", purpose: "system" });
    setImportMode("upload"); setSources(null);
    setUp(null); uploadIdRef.current = "";
    setImportDir(""); setImportPoolAvail(null);
    setModal("import");
    try {
      const r = await api.listImportSources();
      setImportDir((r && r.import_dir) || "");
      setImportPoolAvail(r && typeof r.pool_available === "number" ? r.pool_available : null);
    } catch {
      // 取不到池空间时照常上传，只是少了提前拦截。
    }
  };
  // 分片 8 MiB：太小则往返开销盖过传输，太大则每次失败重传量大，几十 G 的传输里失败是常态。
  const CHUNK = 8 * 1024 * 1024;

  // 从本机分片上传镜像文件，支持断点续传：每片声明起点，断开后先问服务器已收到多少再接着传。
  const uploadFile = async (file) => {
    if (!file) return;
    // 池装不下就当场拒绝，否则传完几十 G 才在导入时被拒。
    if (importPoolAvail != null && file.size > importPoolAvail) {
      setUp({ name: file.name, size: file.size, sent: 0, state: "error",
        error: `存储池放不下：文件 ${fmtGiB(file.size)}，可用 ${fmtGiB(importPoolAvail)}` });
      return;
    }
    stopUpload();
    const ctl = new AbortController();
    uploadCtlRef.current = ctl;
    const live = () => uploadCtlRef.current === ctl;
    setUp({ name: file.name, size: file.size, sent: 0, state: "uploading", error: "" });
    let session;
    try {
      // 带上修改时间，服务端据此区分「同一个文件重选」和「同名同大小的另一个文件」，续错会拼出坏文件。
      session = await api.beginUpload({ file_name: file.name, size_bytes: file.size, last_modified: file.lastModified });
    } catch (e) {
      if (live()) setUp(u => ({ ...u, state: "error", error: e.message || "无法开始上传" }));
      return;
    }
    if (!live()) return;
    uploadIdRef.current = session.upload_id;
    let offset = session.received || 0;
    // 续传时进度条从断点起画，不先显示 0%。
    if (offset > 0) setUp(u => ({ ...u, sent: offset }));
    let retriesLeft = 5;
    while (offset < file.size && live()) {
      const blob = file.slice(offset, Math.min(offset + CHUNK, file.size));
      try {
        const r = await api.uploadChunk(session.upload_id, offset, blob, ctl.signal);
        if (!live()) return;
        offset = typeof r.received === "number" ? r.received : offset + blob.size;
        setUp(u => ({ ...u, sent: offset }));
        if (r.complete) {
          // 上传完成后就是普通的导入源路径，后续照常导入。
          setForm(prev => ({ ...prev, source_path: r.path || "",
            name: prev.name || normalizeImageName(baseName(file.name)) }));
          setUp(u => ({ ...u, sent: file.size, state: "done" }));
          return;
        }
      } catch (e) {
        if (!live()) return;
        if (retriesLeft-- <= 0) {
          setUp(u => ({ ...u, state: "error", error: e.message || "上传中断" }));
          return;
        }
        // 断开后先问服务器已收到多少再续，否则可能重写或跳过一段，留下长度正确但内容损坏的文件。
        try {
          const st = await api.uploadStatus(session.upload_id);
          if (!live()) return;
          offset = typeof st.received === "number" ? st.received : offset;
          setUp(u => ({ ...u, sent: offset }));
        } catch (e2) {
          if (!live()) return;
          setUp(u => ({ ...u, state: "error", error: e2.message || "上传会话已失效，请重新上传" }));
          return;
        }
      }
    }
  };

  const cancelUpload = async () => {
    const id = uploadIdRef.current;
    uploadIdRef.current = "";
    stopUpload();
    setUp(null);
    if (id) { try { await api.abortUpload(id); } catch { /* 放弃即可，不必打扰用户 */ } }
  };

  // 关弹窗只停本地循环、保留服务端会话，重选同一个文件还能续传；「放弃」才作废会话。
  const closeImport = () => { stopUpload(); setModal(null); };

  const loadImportSources = async () => {
    try { setSources(await api.listImportSources()); }
    catch (e) { setSources({ items: [], import_dir: "", node: "", error: e.message }); }
  };

  const doImport = async () => {
    const name = (form.name || "").trim(), source = (form.source_path || "").trim();
    if (!name) { store.toast("请输入镜像名称", "err"); return; }
    if (!source) {
      store.toast(importMode === "server" ? "请先在服务器目录里选一个文件"
        : up && up.state === "uploading" ? "文件还在上传中，传完就能导入"
        : up && up.state === "error" ? "上传没成功，请重新选择文件"
          : "请先选择要导入的镜像文件", "err");
      return;
    }
    const normalizedName = normalizeImageName(name);
    if (!normalizedName) { store.toast("镜像名称只能包含字母、数字、短横线或下划线", "err"); return; }
    // 机器算出的名称直接规范化，不退回让用户重填。
    if (normalizedName !== name) {
      setForm(prev => ({ ...prev, name: normalizedName }));
      store.toast(`镜像名称已规范化为 ${normalizedName}`, "info");
    }
    // 刚上传的源文件导完就删，留着白占几十 G；运维自己放的文件不走这条路，不会被误删。
    if (await run(() => api.importImage({ name: normalizedName, source_path: source, os_type: form.os_type,
      purpose: form.purpose || "system", delete_source_after: importMode === "upload" }),
      `镜像 ${normalizedName} 导入任务已提交`, { reload: loadImages, errMsg: "导入失败" })) setModal(null);
  };
  const doDeleteImage = async () => {
    if (!selImage) return;
    const refs = await groupsReferencing(g => g.SystemImageID === selImage.ID);
    const message = refs.length
      ? `${selImage.Name} 正被分组 ${namesClause(refs)} 用作系统盘，删除会失败。\n请先在分组中改绑其他镜像。`
      : `删除 ${selImage.Name}？其全部配置与还原点将一并删除。若被分组用作数据盘，删除会失败。`;
    const ok = await conf.ask({ title: "删除镜像", message, danger: true });
    if (!ok) return;
    await run(() => api.deleteImage(selImage.ID), "镜像已删除", { reload: loadImages, errMsg: "删除失败" });
  };

  // 下载：用登录态换一次性票据，再交给浏览器普通 URL；fetch+blob 会把整个镜像读进内存。
  const doExportDownload = async () => {
    if (!selImage) return;
    const ok = await run(async () => {
      const t = await api.exportImageTicket(selImage.ID, exportGzip);
      const a = document.createElement("a");
      a.href = t.url;
      document.body.appendChild(a);
      a.click();
      a.remove();
      return t;
    }, "已开始下载，进度见浏览器的下载列表", { errMsg: "导出失败" });
    if (ok) setModal("image");
  };

  // 导出到服务器目录：结果里带着落在哪台、哪个路径，直接告诉运维。
  const doExportToDir = async () => {
    if (!selImage) return;
    let r;
    const ok = await run(async () => (r = await api.exportImage(selImage.ID)), null, { errMsg: "导出失败" });
    if (ok) {
      store.toast(`已开始导出到 ${r.node || "当前主机"} 的 ${r.path}`, "ok");
      setModal("image");
    }
  };

  return (
    <div className="fade-in" style={{ display: "flex", flexDirection: "column", gap: 14 }}>
      <div className="tabs">
        {[["images", "镜像", images.length], ["configs", "配置"], ["reductions", "还原点"]].map(([v, label, n]) => (
          <div key={v} role="button" className={`tab ${tab === v ? "active" : ""}`} onClick={() => setTab(v)}>
            {label}{n != null && <span className="count">{n}</span>}
          </div>
        ))}
        <div style={{ flex: 1 }}/>
        {tab === "images" && <>
          <button className="btn" style={{ marginBottom: 6 }} onClick={openBlank}><Icons.Disk size={12}/> 新建数据盘</button>
          <button className="btn primary" style={{ marginBottom: 6, marginLeft: 6 }} onClick={openImport}><Icons.Plus size={12}/> 导入镜像</button>
        </>}
      </div>

      {tab === "configs" && <ConfigsTab images={images} onShowReductions={showReductions}/>}
      {tab === "reductions" && <RestorePointsTab images={images} imageId={redPick.imageId} configId={redPick.configId}
        onPick={(imageId, configId) => setRedPick({ imageId, configId })}/>}

      {tab === "images" && <div className="row" style={{ gap: 8 }}>
        <Select aria-label="按用途筛选" value={purposeF} onChange={setPurposeF} style={{ width: 140 }}
          options={[{ value: "all", label: "全部用途" }, { value: "system", label: "系统盘镜像" }, { value: "data", label: "数据盘镜像" }]}/>
        <Select aria-label="按系统筛选" value={osF} onChange={setOsF} style={{ width: 130 }}
          options={[{ value: "all", label: "全部系统" }, { value: "windows", label: "Windows" }, { value: "linux", label: "Linux" }]}/>
        <SearchBox value={q} onChange={setQ} placeholder="搜索镜像名称"/>
      </div>}

      {tab === "images" &&       <div style={{ display: "grid", gridTemplateColumns: "repeat(auto-fit, minmax(330px, 1fr))", gap: 12, alignContent: "start" }}>
        {filtered.map(img => {
          const active = selImageId === img.ID;
          const os = OS_META[img.OSType] || { t: img.OSType || "—" };
          const accent = PURPOSE_META[purposeOf(img)].c;
          const st = IMG_STATE[img.State] || { t: img.State || "—", c: "" };
          return (
            <div key={img.ID} onClick={() => openImageDetail(img.ID)} className="card" style={{
              padding: 16, cursor: "pointer",
              borderColor: active ? "oklch(0.55 0.10 200 / 0.5)" : undefined,
              boxShadow: active ? "0 0 0 1px var(--cyan)" : undefined,
            }}>
              <div style={{ display: "flex", alignItems: "flex-start", gap: 12 }}>
                <div style={{ width: 44, height: 44, borderRadius: 8, background: `var(--${accent}-soft)`, border: `1px solid oklch(0.55 0.1 ${accent === "violet" ? 290 : 200} / 0.4)`, display: "grid", placeItems: "center", color: `var(--${accent})` }}
                  role="img" aria-label={purposeOf(img) === "data" ? PURPOSE_META.data.t : `${os.t} ${PURPOSE_META.system.t}`}>
                  {purposeOf(img) === "data" ? <Icons.Disk size={22}/> : img.OSType === "linux" ? <Icons.Linux size={22}/> : img.OSType === "windows" ? <Icons.Windows size={20}/> : <Icons.Layers size={22}/>}
                </div>
                <div style={{ flex: 1, minWidth: 0 }}>
                  <div className="ellip" style={{ fontSize: 14, fontWeight: 600 }}>{img.Name}</div>
                  {/* 同时给逻辑大小和实占：只给一个数会被读成「这个镜像占了池这么多」。 */}
                  <div className="mono meta" style={{ marginTop: 2 }}>逻辑 {fmtGiB(img.Size)} · 实占 {img.Used == null ? "—" : fmtGiB(img.Used)}</div>
                </div>
                <span className={`chip ${st.c}`}>{img.State === "importing" && <span className="dot cyan"/>}{st.t}</span>
              </div>
              {img.State === "importing" && (() => {
                const task = importTaskOf(img.ID);
                return (
                  <div style={{ marginTop: 10 }} onClick={e => { e.stopPropagation(); goto("tasks"); }} title="点击查看任务详情">
                    <div className="bar cyan"><span style={{ width: `${task?.progress || 0}%` }}/></div>
                    <div className="meta" style={{ marginTop: 4 }}>{task ? `导入中 ${task.progress}%` : "导入排队中"} · 查看任务</div>
                  </div>
                );
              })()}
              <div style={{ marginTop: 12, display: "flex", justifyContent: "space-between", alignItems: "center" }} className="meta">
                <span className="row" style={{ gap: 6 }}>
                  <span className="chip">{os.t}</span>
                  <span className={`chip ${PURPOSE_META[purposeOf(img)].c}`}>{PURPOSE_META[purposeOf(img)].t}</span>
                </span>
                <span className="mono">{fmtDateTime(img.CreatedAt)}</span>
              </div>
            </div>
          );
        })}
        {!filtered.length && (
          <div className="card" style={{ padding: 40, textAlign: "center", color: "var(--fg-faint)", fontSize: 13, gridColumn: "1 / -1" }}>
            {loading ? "加载中…" : error ? `加载失败：${error}` : images.length ? "没有符合筛选条件的镜像" : "暂无镜像 · 右上角可导入镜像或新建数据盘"}
          </div>
        )}
      </div>}

      <Modal open={modal === "image"} onClose={() => setModal(null)} title={selImage ? selImage.Name : "镜像详情"} size="lg"
        footer={selImage && <>
          <button className="btn" onClick={() => goto("groups")}>分配到分组</button>
          <button className="btn" onClick={() => setModal("export")}><Icons.Download size={12}/> 导出镜像</button>
          <button className="btn danger" onClick={doDeleteImage}><Icons.Trash size={12}/> 删除镜像</button>
        </>}>
        {!selImage ? (
          <div style={{ padding: "4px 0", fontSize: 13, color: "var(--fg-faint)" }}>未选中镜像</div>
        ) : (
          <>
            <div className="row" style={{ gap: 10, marginBottom: 12, fontSize: 13 }}>
              <span className="lbl">用途</span>
              <Select value={purposeOf(selImage)} aria-label="镜像用途" onChange={setPurpose} style={{ width: 160 }}
                options={[{ value: "system", label: "系统盘镜像" }, { value: "data", label: "数据盘镜像" }]}/>
              <span className="hint">{selImage.Origin === "blank" ? "新建的空数据盘" : "导入的镜像"} · 被分组使用时不能改用途</span>
            </div>
            {purposeOf(selImage) === "system" && <ImageHealthCard imageId={selImage.ID}/>}

            <ConfigOverview image={selImage} onManage={(cfg) => showReductions(selImage.ID, cfg.ID)}/>
          </>
        )}
      </Modal>

      {/* 导入镜像 */}
      {/* 导出：一条可再导入的流，可选 gzip */}
      <Modal open={modal === "export"} onClose={() => !busy && setModal("image")} title={selImage ? `导出镜像 · ${selImage.Name}` : "导出镜像"} size="sm">
        <div className="hint" style={{ marginBottom: 12 }}>
          导出为 {exportGzip ? ".zfs.gz" : ".zfs"} 镜像流文件，下载到本机后可在任意一台服务器的「导入镜像 · 从本机上传」里传回去
          （导入时重新填写名称和系统类型）。导出的是镜像导入时的原始内容，配置与还原点里的改动不在其中；
          要导出某个还原点，请到「还原点」页用「导出为镜像文件」。
        </div>
        <div style={{ display: "flex", flexDirection: "column", gap: 10 }}>
          <label style={{ display: "flex", alignItems: "center", gap: 8, fontSize: 13, cursor: "pointer" }}>
            <input type="checkbox" checked={exportGzip} disabled={busy}
                   onChange={e => setExportGzip(e.target.checked)} aria-label="gzip 压缩"/>
            gzip 压缩
          </label>
          <span className="hint" style={{ marginTop: -4 }}>
            镜像流本身已带存储池的压缩，gzip 通常还能再小三到五成，代价是导出时占用服务器 CPU。
            导入时自动识别，不必解压。
          </span>
          <div className="row" style={{ gap: 10, alignItems: "flex-start" }}>
            <button className="btn primary" style={{ flexShrink: 0, minWidth: 128 }} disabled={busy} onClick={doExportDownload}>
              <Icons.Download size={12}/> 下载到本地
            </button>
            <span className="hint" style={{ paddingTop: 4 }}>浏览器直接下载，进度见下载列表。中断后需重新下载。</span>
          </div>
          {/* 盘对盘导出不经网络，快得多；文件落在当前服务的那台上，要说清是哪台。 */}
          <div className="row" style={{ gap: 10, alignItems: "flex-start" }}>
            <button className="btn" style={{ flexShrink: 0, minWidth: 128 }} disabled={busy} onClick={doExportToDir}>
              <Icons.Server size={12}/> 导出到服务器目录
            </button>
            <span className="hint" style={{ paddingTop: 4 }}>
              写到当前主机的导入目录，盘对盘、不经网络，大镜像快得多。导出过程中若发生主备切换，
              任务会失败，可在任务页看到。gzip 选项对这条出口不生效。
            </span>
          </div>
        </div>
      </Modal>

      <Modal open={modal === "import"} onClose={() => !busy && closeImport()} title="导入镜像"
        footer={<><button className="btn" disabled={busy} onClick={closeImport}>取消</button>
          {/* 源路径要上传完成才有，传完之前按钮禁用并说明在等什么。 */}
          <button className="btn primary" onClick={doImport}
            disabled={busy || (importMode === "server" ? !form.source_path : (!up || up.state !== "done"))}>
            {busy ? "提交中…" : importMode === "upload" && up && up.state === "uploading" ? "上传中…" : "导入"}
          </button></>}>
        <Field label="镜像名称" required><input className="input" style={{ width: "100%" }} value={form.name || ""} onChange={e => setForm({ ...form, name: e.target.value })} placeholder="Win11-Pro-2026Q1"/></Field>
        {/* 两条路的取舍见 importMode 处注释；服务器目录这条必须写明是哪台。 */}
        <div className="row" style={{ gap: 0, marginBottom: 10 }}>
          <div className="btn-group">
            <button type="button" className={`btn ${importMode === "upload" ? "primary" : ""}`}
              onClick={() => setImportMode("upload")}>从本机上传</button>
            <button type="button" className={`btn ${importMode === "server" ? "primary" : ""}`}
              onClick={() => { setImportMode("server"); loadImportSources(); }}>从服务器目录</button>
          </div>
        </div>
        {importMode === "server" ? (
          <Field label="选择服务器上的文件" required
            hint={sources
              ? `目录在当前主机 ${sources.node || "?"} 的 ${sources.import_dir}——把文件 scp 到这台机器上，它就会出现在这里。`
                + "导入过程中若发生主备切换，任务会失败，可在任务页看到。"
                + (sources.pool_available != null ? ` · 存储池可用 ${fmtGiB(sources.pool_available)}` : "")
              : "正在读取服务器目录…"}>
            <div style={{ border: "1px solid var(--line)", borderRadius: 6, maxHeight: 220, overflowY: "auto" }}>
              {(sources?.items || []).length === 0 && (
                <div className="hint" style={{ padding: 12 }}>
                  {sources ? "这个目录里还没有可导入的文件" : "读取中…"}
                </div>
              )}
              {(sources?.items || []).map(f => (
                <div key={f.path} onClick={() => setForm({ ...form, source_path: f.path })}
                  style={{ padding: "8px 12px", cursor: "pointer", display: "flex", gap: 10, alignItems: "center",
                    background: form.source_path === f.path ? "var(--bg-0)" : undefined,
                    boxShadow: form.source_path === f.path ? "inset 2px 0 0 var(--accent)" : undefined }}>
                  <span className="mono" style={{ flex: 1 }}>{f.name}</span>
                  <span className="chip">{f.format}</span>
                  <span className="meta mono">{fmtGiB(f.size)}</span>
                </div>
              ))}
            </div>
          </Field>
        ) : (
          <Field label="选择本机文件" required
            hint={(nodes.length > 1
              ? "上传完成后即可导入，镜像会自动同步到集群所有节点"
              : "上传完成后即可导入")
              + "。支持断点续传：中断后重新选择同一个文件即可接着传。"
              + (importPoolAvail != null ? ` · 存储池可用 ${fmtGiB(importPoolAvail)}` : "")}>
            <input className="input" type="file" style={{ width: "100%" }} disabled={up && up.state === "uploading"}
              onChange={e => uploadFile(e.target.files && e.target.files[0])}/>
            {up && (
              <div style={{ marginTop: 10 }}>
                <div className="row" style={{ justifyContent: "space-between", fontSize: 13, marginBottom: 4 }}>
                  <span className="mono muted">{up.name}</span>
                  <span className="mono">{up.size ? Math.floor((up.sent / up.size) * 100) : 0}%</span>
                </div>
                <div className={`bar ${up.state === "error" ? "rose" : up.state === "done" ? "emerald" : "cyan"}`}>
                  <span style={{ width: `${up.size ? (up.sent / up.size) * 100 : 0}%` }}/>
                </div>
                <div className="row" style={{ justifyContent: "space-between", marginTop: 6 }}>
                  <span className="meta">
                    {up.state === "done" ? "上传完成，可以导入了"
                      : up.state === "error" ? up.error
                        : `${fmtGiB(up.sent)} / ${fmtGiB(up.size)}`}
                  </span>
                  {up.state === "uploading" && (
                    <button className="btn ghost" style={{ padding: "2px 8px" }} onClick={cancelUpload}>放弃</button>
                  )}
                </div>
              </div>
            )}
          </Field>
        )}

        <Field label="操作系统" required><Select value={form.os_type || "windows"} onChange={v => setForm({ ...form, os_type: v })} options={[{ value: "windows", label: "Windows" }, { value: "linux", label: "Linux" }]}/></Field>
        <Field label="用途" required hint={form.purpose === "data" ? "数据盘镜像：挂给分组作 D 盘等，不烘焙启动脚本、不做引导体检" : "系统盘镜像：分组的启动盘"}>
          <Select value={form.purpose || "system"} aria-label="用途" onChange={v => setForm({ ...form, purpose: v })} options={[{ value: "system", label: "系统盘镜像" }, { value: "data", label: "数据盘镜像" }]}/>
        </Field>
      </Modal>

      {/* 新建空数据盘 */}
      <Modal open={modal === "blank"} onClose={() => !busy && setModal(null)} title="新建数据盘"
        footer={<><button className="btn" disabled={busy} onClick={() => setModal(null)}>取消</button><button className="btn primary" disabled={busy} onClick={doCreateBlank}>{busy ? "提交中…" : "创建"}</button></>}>
        <Field label="数据盘名称" required><input className="input" aria-label="数据盘名称" style={{ width: "100%" }} value={form.name || ""} onChange={e => setForm({ ...form, name: e.target.value })} placeholder="games-d"/></Field>
        <Field label="大小" required hint="稀疏分配，只占实际写入；建好后不能缩小">
          <div className="row" style={{ gap: 8 }}>
            <input className="input mono" type="number" min="1" step="1" aria-label="大小" style={{ width: 120 }} value={form.size_gib ?? 100} onChange={e => setForm({ ...form, size_gib: e.target.value })}/>
            <span className="mono meta">GiB</span>
            <span style={{ flex: 1 }}/>
            {[100, 200, 500, 1000].map(g => <button key={g} type="button" className={`btn ${Number(form.size_gib) === g ? "primary" : ""}`} style={{ padding: "3px 8px" }} onClick={() => setForm({ ...form, size_gib: g })}>{g}</button>)}
          </div>
        </Field>
        <Field label="文件系统" required hint={form.filesystem === "ext4" ? "给 Linux 分组用" : "给 Windows 分组用"}>
          <Select value={form.filesystem || "ntfs"} aria-label="文件系统" onChange={v => setForm({ ...form, filesystem: v })} options={Object.entries(FS_META).map(([value, m]) => ({ value, label: m.t }))}/>
        </Field>
        <Field label="卷标" hint={`客户机里显示的盘名，如「${(form.label || form.name || "游戏盘").trim() || "游戏盘"} (F:)」；留空用数据盘名称。盘符不在这里设，在分组的数据盘「挂载目标」里`}>
          <input className="input" aria-label="卷标" style={{ width: "100%" }} value={form.label || ""} onChange={e => setForm({ ...form, label: e.target.value })} placeholder="留空 = 数据盘名称"/>
        </Field>
      </Modal>

      {conf.node}
    </div>
  );
}

// 镜像健康卡片（GET /api/images/{id}/health，POST .../health-check）。
const HEALTH_META = {
  ok: { label: "健康", chip: "emerald" },
  warn: { label: "警告", chip: "amber" },
  block: { label: "阻断", chip: "rose" },
  unknown: { label: "未知", chip: "" },
};

function ImageHealthCard({ imageId }) {
  const store = useStore();
  const [report, setReport] = useState(null);
  const [missing, setMissing] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const { busy: running, run } = useMutation(store.toast);

  const idRef = useRef(imageId);
  const load = async () => {
    try {
      setReport(await api.getImageHealth(imageId));
      setMissing(false);
    } catch {
      setReport(null);
      setMissing(true);
    }
  };
  useEffect(() => { idRef.current = imageId; setReport(null); setMissing(false); setExpanded(false); load(); }, [imageId]);

  // 检查以任务运行，useMutation 跟到结束再回调，避免慢检查时显示旧报告，失败时能说明原因。
  const rerun = () => run(
    () => api.runImageHealthCheck(imageId),
    "体检任务已发起",
    {
      errMsg: "体检发起失败",
      reload: async () => {
        if (idRef.current !== imageId) return; // 检查期间已切换镜像
        await load();
      },
    },
  );

  const meta = report ? (HEALTH_META[report.Level] || HEALTH_META.unknown) : null;
  return (
    <div style={{ background: "var(--bg-0)", border: "1px solid var(--line-soft)", borderRadius: 8, padding: "10px 12px", marginBottom: 14 }}>
      <div style={{ display: "flex", alignItems: "center", gap: 8 }}>
        <span className="lbl">镜像体检</span>
        {report && <span className={`chip ${meta.chip}`}>{meta.label}</span>}
        {report?.PartitionStyle && <span className="chip mono">{report.PartitionStyle.toUpperCase()}</span>}
        {missing && <span className="chip">暂无报告</span>}
        <div style={{ flex: 1 }}/>
        {report && (report.Items || []).length > 0 && (
          <button className="btn ghost" style={{ padding: "2px 8px" }} onClick={() => setExpanded(!expanded)}>
            {expanded ? "收起" : `明细 ${(report.Items || []).length}`}
          </button>
        )}
        <button className="btn" style={{ padding: "2px 8px" }} disabled={running} onClick={rerun}>
          <Icons.Refresh size={11}/> {running ? "体检中…" : "重跑体检"}
        </button>
      </div>
      {expanded && report && (
        <div style={{ marginTop: 10, display: "flex", flexDirection: "column", gap: 6 }}>
          {(report.Items || []).map(item => {
            const m = HEALTH_META[item.level] || HEALTH_META.unknown;
            return (
              <div key={item.name} style={{ display: "flex", gap: 8, alignItems: "flex-start" }}>
                <span className={`chip ${m.chip}`} style={{ flexShrink: 0 }}>{m.label}</span>
                <div>
                  <span className="mono" style={{ fontSize: 12, color: "var(--fg-mute)" }}>{item.name}</span>
                  <div className="hint">{item.detail}</div>
                </div>
              </div>
            );
          })}
        </div>
      )}
    </div>
  );
}

// 详情里只读地概括每个配置：当前用哪个还原点、哪些分组在用；管理去还原点页。
function ConfigOverview({ image, onManage }) {
  const { data } = useResource(async () => {
    const configs = ((await api.listConfigs(image.ID).catch(() => null)) || {}).items || [];
    const reds = await Promise.all(configs.map(c => api.listReductions(c.ID).then(r => r.items || []).catch(() => [])));
    const usersOf = await loadConfigUsers();
    return configs.map((c, i) => {
      return { ...c, current: reds[i].find(r => r.ID === c.DefaultReductionID), count: reds[i].length, users: usersOf(c.ID) };
    });
  }, [image.ID]);
  const rows = data || [];
  return (
    <div style={{ marginTop: 14 }}>
      <div className="lbl" style={{ marginBottom: 8 }}>配置概况</div>
      <div style={{ display: "flex", flexDirection: "column", gap: 6 }}>
        {rows.map(c => (
          <div key={c.ID} className="row" style={{ gap: 10, border: "1px solid var(--line-soft)", borderRadius: 6, padding: "8px 10px", background: "var(--bg-0)", fontSize: 13 }}>
            <Icons.Cog size={12} style={{ color: "var(--fg-mute)" }}/>
            <span style={{ fontWeight: 600 }}>{c.Name}</span>
            <span className="meta">当前还原点 {c.current ? redName(c.current) : "—"} · 共 {c.count} 个</span>
            <span className="meta ellip" style={{ flex: 1 }}><UsageCell users={c.users} empty="没有分组在用"/></span>
            <button className="btn ghost" style={{ padding: "2px 8px" }} onClick={() => onManage(c)} aria-label={`管理还原点 ${c.Name}`}>管理还原点<Icons.Chevron size={11}/></button>
          </div>
        ))}
        {data && !rows.length && <div className="hint">暂无配置 · 可在「配置」页新建</div>}
      </div>
    </div>
  );
}

window.PageImages = PageImages;

export { PageImages };
