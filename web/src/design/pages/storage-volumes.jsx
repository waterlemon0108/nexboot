// 存储池详情：这个池里有什么、属于谁。只给 storage.jsx 用。
import React from 'react';
import { Icons } from '../icons.jsx';
import { useStore } from '../store.jsx';
import { useConfirm } from '../overlay.jsx';
import { TableState } from '../primitives.jsx';
import { api } from '../../lib/api.js';
import { useResource, useMutation } from '../../lib/hooks.js';
import { fmtCap, fmtDateTime, fmtMac } from '../../lib/format.js';
const { useState } = React;

// 类别按钮兼任图例：颜色和色条一一对应，数字只出现一次。
const CATEGORY = {
  images: { label: "镜像与配置", color: "var(--cyan)" },
  imports: { label: "导入文件", color: "var(--violet)" },
  client: { label: "客户机盘", color: "var(--emerald)" },
  residue: { label: "残留", color: "var(--amber)" },
  system: { label: "系统", color: "var(--fg-faint)" },
  backup: { label: "备份副本", color: "var(--violet)" },
};

const IMAGE_STATE = {
  pending_delete: { cls: "amber", text: "待复制删除", who: "主机已删除，下一轮复制跟随" },
  pending_copy: { cls: "cyan", text: "待复制", who: "主机有，这台还没收到" },
  behind: { cls: "amber", text: "和主机不一致" },
  missing: { cls: "rose", text: "池里没有", who: "库里有记录，这个池里没有数据" },
  importing: { cls: "cyan", text: "导入中", who: "正在导入" },
};
// 这些状态库里没有这个镜像，也就不知道它的类型。
const NOT_IN_CATALOGUE = new Set(["pending_delete", "importing"]);

// 颜色和镜像管理一致：系统盘青、数据盘紫。
const PURPOSE = { system: { text: "系统盘", cls: "cyan" }, data: { text: "数据盘", cls: "violet" } };

// 界面上把身份层叫「网卡层」：里面是这台机器的网卡信息，操作者不认「身份层」这个内部叫法。
const DISK_KIND = {
  identity: "网卡层", adapt: "适配盘", installation: "安装盘", inspect: "体检临时盘", export: "导出临时盘",
};

// 客户机克隆按 LUN 分系统盘和数据盘：LUN 0 是系统盘。
const isClone = (d) => d.kind === "client" || d.kind === "super";
const diskName = (d) => (isClone(d) ? (d.lun ? "数据盘" : "系统盘") : DISK_KIND[d.kind] || d.kind);

// 每台一句：有超管盘说超管盘，有临时盘说临时盘，都没有才说网卡层等其他盘。
function diskSummary(disks) {
  const count = (kind) => disks.filter(d => d.kind === kind).length;
  const phrase = (label, n) => (n > 1 ? `${label} ${n} 块` : label);
  if (count("super")) return phrase("超管盘", count("super"));
  if (count("client")) return phrase("临时盘", count("client"));
  return [...new Set(disks.map(diskName))].join("、");
}

// 悬停看每块盘：系统盘在前，网卡层在后。
function diskDetail(disks) {
  const rank = (d) => (isClone(d) ? d.lun : 100);
  return [...disks].sort((a, b) => rank(a) - rank(b)).map(d => `${diskName(d)} ${fmtBytes(d.used)}`).join("、");
}

// 状态里的来源以系统盘为准，没有系统盘时取第一块说得出来源的盘。
function clientSource(disks) {
  const system = disks.find(d => isClone(d) && !d.lun && d.source);
  return (system || disks.find(d => d.source))?.source;
}

const SYSTEM_DIR = {
  winpe: "WinPE 适配环境", drivers: "驱动", installations: "安装记录", "pe-sessions": "PE 会话",
  "imports/.uploads": "没传完的上传",
};

const BACKUP_STATE = {
  same: { text: "和数据池一致" },
  removed: { text: "数据池已删除，下一轮备份时移除", warn: true },
  newer: { text: "数据池有变化，等下一轮备份", warn: true },
};

// 这一页要看清小占用（网卡层只有几 MiB），fmtCap 的精度不够。
function fmtBytes(b) {
  b = Number(b || 0);
  if (b >= 1024 ** 3) return `${(b / 1024 ** 3).toFixed(2)} GiB`;
  if (b >= 1024 ** 2) return `${(b / 1024 ** 2).toFixed(1)} MiB`;
  if (b >= 1024) return `${Math.round(b / 1024)} KiB`;
  return `${b} B`;
}

const daysSince = (iso) => Math.max(0, Math.floor((Date.now() - Date.parse(iso)) / 86400000));

function clientStatus(c) {
  if (!c.mac) return "用完自动清理，服务重启时也会清扫";
  if (!c.known) return "库里没有这台机器";
  if (c.state === "online") return "在线";
  return c.offline_at ? `关机 ${daysSince(c.offline_at)} 天` : "关机";
}

function residueNote(r) {
  const users = (r.dependents || []).length ? `${r.dependents.join("、")} 还在用` : "";
  switch (r.kind) {
    case "diverged":
    case "rebuilding": {
      const unique = (r.unique || []).length;
      const what = unique ? `有 ${unique} 个当前目录里没有的还原点` : "当前目录已有同样内容";
      return users ? `${what} · ${users}` : what;
    }
    case "held": return users ? `主机已删，${users}` : "主机已删，等待自动回收";
    case "unregistered": return r.has_baseline ? "库里没有记录，不处理的话下次重启会收回目录" : "库里没有记录";
    case "superseded": return "超管保存中断时留下，重新保存会接着用它";
    case "recv_temp": return "复制接收中断时留下，下一轮复制自动清理";
    default: return "";
  }
}

const RESIDUE_LABEL = {
  diverged: "分叉副本", rebuilding: "重建留下的旧副本", held: "等客户机放手的数据",
  unregistered: "未登记的卷", superseded: "保存中断留下的配置", recv_temp: "接收中断留下的数据",
};

function systemName(item) {
  switch (item.kind) {
    case "files_dir": return SYSTEM_DIR[item.name] || item.name;
    case "files": return "系统文件（文件库没有挂载）";
    case "db": return "数据库副本";
    case "rep_markers": return "同步记录";
    case "deleted_files": return "已删除的文件";
    case "backup_markers": return `备份标记 ${item.count} 个`;
    case "legacy_imports": return "旧版导入目录";
    default: return "未识别的数据集";
  }
}

function systemNote(item) {
  switch (item.kind) {
    case "rep_markers": return "服务器之间同步用的比对点，只留最近几轮，不用处理";
    case "deleted_files": return "等同步记录轮转后释放，约 5 分钟；有服务器离线时最长 24 小时";
    case "backup_markers": return "每轮备份时轮转，备份没开时不轮转";
    case "unknown": return "认不出是什么，只读";
    default: return "由系统维护";
  }
}

function Table({ head, colSpan, state, children }) {
  return (
    <div className="card" style={{ padding: 0, overflowX: "auto" }}>
      <table className="t">
        <thead><tr>{head}</tr></thead>
        <tbody>
          {children}
          <TableState colSpan={colSpan} {...state}/>
        </tbody>
      </table>
    </div>
  );
}

const usedCell = (used, size) => (
  <td className="mono" style={{ textAlign: "right", whiteSpace: "nowrap" }}>
    {fmtBytes(used)}{size ? <span className="meta"> / {fmtCap(size)}</span> : null}
  </td>
);
const parts = (r) => `${r.configs} 个配置 · ${r.points} 个还原点`;

function ImagesTable({ rows, state }) {
  return (
    <Table colSpan={5} state={state} head={<>
      <th>镜像</th><th>镜像类型</th><th>包含</th><th style={{ textAlign: "right" }}>实占 / 大小</th><th>创建时间</th>
    </>}>
      {rows.map(r => {
        const st = IMAGE_STATE[r.state];
        const purpose = NOT_IN_CATALOGUE.has(r.state) ? null : PURPOSE[r.purpose === "data" ? "data" : "system"];
        return (
          <tr key={r.id}>
            <td>
              <span className="row" style={{ gap: 8 }}>
                <span className="ellip" style={{ fontWeight: 500 }} title={`${r.name}（${r.dataset}）`}>{r.name}</span>
                {st && <span className={`chip ${st.cls}`} title={st.who}>{st.text}</span>}
              </span>
            </td>
            <td>{purpose ? <span className={`chip ${purpose.cls}`}>{purpose.text}</span> : <span className="muted">—</span>}</td>
            <td className="muted meta" style={{ whiteSpace: "nowrap" }}>{parts(r)}</td>
            {usedCell(r.used, r.size)}
            <td className="mono meta" style={{ whiteSpace: "nowrap" }}>{fmtDateTime(r.created_at)}</td>
          </tr>
        );
      })}
    </Table>
  );
}

function ImportsTable({ rows, readable, state, onDelete, busy }) {
  return (
    <Table colSpan={onDelete ? 4 : 3} state={{ ...state, hint: readable ? "这个池里没有导入文件" : "这台的文件库没有挂载，看不到文件清单（占用算在「系统」里）" }} head={<>
      <th>文件</th><th style={{ textAlign: "right" }}>实占 / 大小</th><th>上传时间</th>{onDelete && <th className="t-actions"></th>}
    </>}>
      {rows.map(f => {
        // 主机已经删了，这台下一轮同步才跟上；再删一次什么也不会发生。
        const following = f.state === "pending_delete";
        return (
          <tr key={f.name}>
            <td>
              <span className="row" style={{ gap: 8 }}>
                <span className="ellip" title={f.name} style={{ fontWeight: 500 }}>{f.name}</span>
                {following && <span className="chip amber" title="主机已删除，下一轮同步时跟着删">待复制删除</span>}
              </span>
            </td>
            {usedCell(f.used, f.size)}
            <td className="mono meta" style={{ whiteSpace: "nowrap" }}>{fmtDateTime(f.modified_at)}</td>
            {onDelete && (
              <td className="t-actions"><div>
                {!following && <button className="btn ghost danger" style={{ padding: "3px 8px" }} disabled={busy} aria-label={`删除 ${f.name}`} onClick={() => onDelete(f)}>删除</button>}
              </div></td>
            )}
          </tr>
        );
      })}
    </Table>
  );
}

// 文件随复制到每台，空间要等旧同步记录轮转才回来，确认前逐台说清楚。
function deleteImportMessage(file, data, nodes) {
  const lines = [`删除 ${file.name}？不能恢复，已经导入的镜像不受影响。`];
  const writer = nodes.find(n => n.role === "active");
  const peers = nodes.filter(n => n !== writer);
  const follow = peers.filter(n => n.online).map(n => n.ip);
  const offline = peers.filter(n => !n.online).map(n => n.ip);
  if (writer) lines.push("", `${writer.ip}：立刻删除`);
  if (follow.length) lines.push(`${follow.join("、")}：下一轮同步时跟着删，约 1~2 分钟`);
  if (offline.length) lines.push(`${offline.join("、")}：离线，恢复后跟着删`);
  lines.push("");
  if ((data.system || []).some(s => s.kind === "backup_markers")) {
    lines.push("备份标记也占着这部分空间，要再跑 3 轮备份才释放。");
  } else if (data.node_role === "single") {
    lines.push("空间马上释放。");
  } else {
    lines.push(`空间约 5 分钟后释放${offline.length ? "；有服务器离线，最长要等 24 小时" : ""}。`);
  }
  return lines.join("\n");
}

function ClientsTable({ rows, state }) {
  return (
    <Table colSpan={5} state={{ ...state, hint: "这台服务器上现在没有客户机盘" }} head={<>
      <th>客户机</th><th>MAC</th><th>盘</th><th style={{ textAlign: "right" }}>实占</th><th>状态</th>
    </>}>
      {rows.map(c => {
        const source = clientSource(c.disks || []);
        return (
          <tr key={c.mac || "scratch"}>
            <td style={{ fontWeight: 500 }}>{c.mac ? (c.name || "未命名") : "临时克隆"}</td>
            <td className="mono meta">{c.mac ? fmtMac(c.mac) : "—"}</td>
            <td style={{ whiteSpace: "nowrap" }}><span title={diskDetail(c.disks || [])} style={{ cursor: "help" }}>{diskSummary(c.disks || [])}</span></td>
            {usedCell(c.used)}
            <td className="muted">{clientStatus(c)}{source ? ` · 来自 ${source}` : ""}</td>
          </tr>
        );
      })}
    </Table>
  );
}

function ResiduesTable({ rows, state }) {
  return (
    <Table colSpan={5} state={{ ...state, hint: "这台服务器上没有残留" }} head={<>
      <th>对象</th><th>数据集</th><th style={{ textAlign: "right" }}>实占</th><th>产生时间</th><th>说明</th>
    </>}>
      {rows.map(r => (
        <tr key={r.dataset}>
          <td style={{ fontWeight: 500 }}>{RESIDUE_LABEL[r.kind] || r.kind}</td>
          <td className="mono meta ellip" style={{ maxWidth: 280 }} title={r.dataset}>{r.dataset}</td>
          {usedCell(r.used)}
          <td className="mono meta" style={{ whiteSpace: "nowrap" }}>{fmtDateTime(r.created_at)}</td>
          <td className="muted" title={(r.unique || []).join("、") || undefined}>{residueNote(r)}</td>
        </tr>
      ))}
    </Table>
  );
}

function SystemTable({ rows, state }) {
  return (
    <Table colSpan={4} state={{ ...state, hint: "没有系统占用" }} head={<>
      <th>对象</th><th>位置</th><th style={{ textAlign: "right" }}>实占</th><th>说明</th>
    </>}>
      {rows.map(item => (
        <tr key={item.path}>
          <td style={{ fontWeight: 500 }}>{systemName(item)}</td>
          <td className="mono meta">{item.path}</td>
          {usedCell(item.used)}
          <td className="muted">{systemNote(item)}</td>
        </tr>
      ))}
    </Table>
  );
}

function BackupTable({ backup, state }) {
  const items = backup?.items || [];
  return (
    <Table colSpan={5} state={{ ...state, empty: !items.length, hint: "这个池里还没有备份副本" }} head={<>
      <th>镜像</th><th>包含</th><th style={{ textAlign: "right" }}>实占 / 大小</th><th>创建时间</th><th>和数据池比较</th>
    </>}>
      {items.map(r => {
        const st = BACKUP_STATE[r.state] || BACKUP_STATE.same;
        return (
          <tr key={r.id}>
            <td className="ellip" style={{ fontWeight: 500 }} title={r.dataset}>{r.name}</td>
            <td className="muted meta" style={{ whiteSpace: "nowrap" }}>{parts(r)}</td>
            {usedCell(r.used, r.size)}
            <td className="mono meta" style={{ whiteSpace: "nowrap" }}>{fmtDateTime(r.created_at)}</td>
            <td className="muted" style={st.warn ? { color: "var(--amber)" } : undefined}>{st.text}</td>
          </tr>
        );
      })}
      {items.length > 0 && backup.system_used > 0 && (
        <tr>
          <td className="muted">系统文件与数据库副本</td><td className="mono meta">nd/files、nd/db</td>
          {usedCell(backup.system_used)}<td></td><td className="muted">由系统维护</td>
        </tr>
      )}
    </Table>
  );
}

function syncLine(data) {
  if (data.role === "backup") {
    const b = data.backup;
    if (!b?.present) return { text: "还没有备份" };
    if (b.pending) return { text: "有新内容等下一轮备份", level: "warn" };
    return { text: b.last_backup_at ? `最后备份 ${fmtDateTime(b.last_backup_at)}` : "" };
  }
  const { level, message, pending } = data.sync || {};
  const more = pending ? `还有 ${pending} 项等下一轮跟随` : "";
  return { text: [message, more].filter(Boolean).join("，"), level: level === "ok" && pending ? "warn" : level };
}

const LEVEL_COLOR = { warn: "var(--amber)", error: "var(--rose)" };

function hintOf(cat, data) {
  const single = data.node_role === "single";
  switch (cat) {
    case "imports": return single ? "" : "导入文件在每台服务器各有一份。";
    case "client": return "只在这台。网卡层存这台机器的网卡信息，提前做好、关机也留着，开机直接用；临时盘关机满 5 分钟自动回收。";
    case "residue": return "只在这台，不参与复制。";
    case "system": return "由系统维护，只读。";
    case "backup": return "这是目录的整份备份，只在这台。单个镜像删不掉，删了下一轮备份又会送回来。";
    default: return "";
  }
}

function PoolVolumes({ pool, node, nodes = [], serverLabel, roleChip, statusChip, missing, disksPanel }) {
  const store = useStore();
  const conf = useConfirm();
  const { busy, run } = useMutation(store.toast);
  const { data, loading, error, reload } = useResource(
    async () => (missing ? null : api.listVolumes(node, pool.Name)),
    [missing, node, pool.Name],
  );
  const [picked, setPicked] = useState(null);

  const reachable = !!data?.reachable;
  // 空的类别不占地方；文件库读不到时导入文件要留着，好说明原因。
  const categories = ((reachable && data.categories) || []).filter(c =>
    c.bytes > 0 || c.count > 0 || c.id === "images" || (c.id === "imports" && !data.files_readable));
  const fallback = categories[0]?.id || "disks";
  const cat = missing ? "disks" : (picked === "disks" || categories.some(c => c.id === picked) ? picked : fallback);

  const used = reachable ? data.used : Number(pool.Used || 0);
  const cap = reachable ? data.used + data.available : Number(pool.Capacity || 0);
  const line = reachable ? syncLine(data) : { text: "" };

  const images = data?.images || [];
  const state = { loading: loading && !data, error, empty: false, hint: "" };

  const deleteImport = async (file) => {
    const ok = await conf.ask({ title: "删除导入文件", message: deleteImportMessage(file, data, nodes), danger: true, confirmText: "删除" });
    if (ok) await run(() => api.deleteImportFile(file.name), `已删除 ${file.name}`, { reload, errMsg: "删除失败" });
  };

  return (
    <>
      <div data-testid="pool-header">
        <div className="row" style={{ flexWrap: "wrap", gap: "6px 10px", marginBottom: 10 }}>
          <span className="mono" style={{ fontSize: 18, fontWeight: 600 }}>{pool.Name}</span>
          {roleChip}
          <span className="mono muted">{data?.ip || serverLabel}</span>
          {data?.node_role && data.node_role !== "single" && (
            <span className={`chip ${data.node_role === "writer" ? "cyan" : ""}`}>{data.node_role === "writer" ? "主机" : "备机"}</span>
          )}
          {statusChip}
          {line.text && <span className="meta" style={{ color: LEVEL_COLOR[line.level] }}>{line.text}</span>}
          {!missing && (
            <span className="mono meta" style={{ marginLeft: "auto" }}>
              已用 {fmtCap(used)} / 共 {fmtCap(cap)} · 可用 {fmtCap(cap - used)}
              {data?.freeing > 0 ? ` · 释放中 ${fmtCap(data.freeing)}` : ""}
            </span>
          )}
        </div>
        {!missing && (
          <div className="bar" style={{ display: "flex", gap: 1, height: 8 }} role="img"
            aria-label={categories.map(c => `${CATEGORY[c.id]?.label || c.id} ${fmtBytes(c.bytes)}`).join("，")}>
            {categories.filter(c => c.bytes > 0 && cap > 0).map(c => (
              <span key={c.id} style={{ width: `${c.bytes / cap * 100}%`, background: CATEGORY[c.id]?.color, borderRadius: 0, transition: "none" }}/>
            ))}
          </div>
        )}
      </div>

      {!missing && (
        <div className="row" style={{ flexWrap: "wrap", gap: 6 }}>
          {categories.map(c => (
            <button key={c.id} type="button" className={`vol-cat ${cat === c.id ? "on" : ""}`} aria-pressed={cat === c.id} onClick={() => setPicked(c.id)}>
              <i style={{ background: CATEGORY[c.id]?.color }}/>{CATEGORY[c.id]?.label || c.id}<b>{fmtBytes(c.bytes)}</b>
            </button>
          ))}
          {categories.length > 0 && <span className="vol-cat-sep"/>}
          <button type="button" className={`vol-cat ${cat === "disks" ? "on" : ""}`} aria-pressed={cat === "disks"} onClick={() => setPicked("disks")}>磁盘与缓存</button>
          <div style={{ flex: 1 }}/>
          <button className="btn ghost icon" onClick={() => reload()} title="立即刷新" aria-label="刷新这个池"><Icons.Refresh size={13}/></button>
        </div>
      )}

      {data && !reachable && <div className="vol-error">{data.error}</div>}

      {cat !== "disks" && reachable && hintOf(cat, data) && <div className="hint">{hintOf(cat, data)}</div>}

      {cat === "disks" && (reachable || missing || data || error) && disksPanel}
      {cat === "disks" && !missing && !data && !error && <div className="hint">读取中…</div>}
      {reachable && cat === "images" && <ImagesTable rows={images} state={{ ...state, empty: !images.length, hint: "这个池里没有镜像" }}/>}
      {reachable && cat === "imports" && <ImportsTable rows={data.imports || []} readable={data.files_readable} busy={busy}
        onDelete={data.authoritative ? deleteImport : null} state={{ ...state, empty: !(data.imports || []).length }}/>}
      {reachable && cat === "client" && <ClientsTable rows={data.clients || []} state={{ ...state, empty: !(data.clients || []).length }}/>}
      {reachable && cat === "residue" && <ResiduesTable rows={data.residues || []} state={{ ...state, empty: !(data.residues || []).length }}/>}
      {reachable && cat === "system" && <SystemTable rows={data.system || []} state={{ ...state, empty: !(data.system || []).length }}/>}
      {reachable && cat === "backup" && <BackupTable backup={data.backup} state={state}/>}
    </>
  );
}

export { PoolVolumes };
