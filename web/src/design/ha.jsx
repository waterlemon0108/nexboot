// 操作者视角的高可用：本节点角色、对端是否跟随，以及唯一的主动操作——计划切换。
// 危险操作已由后端闸门拦住（备机上的写入返回 503 并给出指引），这里只负责指路。
import React from 'react';
import { Icons } from './icons.jsx';
import { useStore } from './store.jsx';
import { Modal } from './overlay.jsx';
import { Tip } from './primitives.jsx';
import { api } from '../lib/api.js';
import { useResource, useMutation } from '../lib/hooks.js';
const { useState, useEffect, useRef } = React;

const ROLE = {
  active:  { chip: 'emerald', text: '主机' },
  standby: { chip: 'amber',   text: '备机' },
};

// HACard 在系统设置页上。单节点（无对端）时直接说明，不把未配对当成问题。
export function HACard() {
  const store = useStore();
  const { data, loading, error, reload } = useResource(() => api.getHA(), []);
  const repl = useResource(() => api.getReplication(), []);
  // 主机上的 peer_url 是 VIP（即它自己），其余节点从节点表列出。
  const roster = useResource(() => api.listClusterNodes(), []);
  const { busy, run } = useMutation(store.toast);
  const [confirmOn, setConfirmOn] = useState(false);
  const [rate, setRate] = useState(null); // null 表示未改动
  const settle = useRef(null);
  useEffect(() => () => clearTimeout(settle.current), []);

  const ha = data || {};
  const paired = !!ha.peer_url;
  const role = ROLE[ha.role] || { chip: '', text: ha.role || '…' };
  const targets = (repl.data && repl.data.targets) || [];
  const nodes = (roster.data && roster.data.items) || [];
  const others = nodes.filter(n => n.id !== ha.node_id);
  const selfIP = (nodes.find(n => n.id === ha.node_id) || {}).ip;

  const doSwitch = async () => {
    if (await run(() => api.plannedSwitch(),
      '已发起计划切换：本机将重启为备机，虚 IP 会漂到一台备机', { errMsg: '切换未执行' })) {
      setConfirmOn(false);
      // 服务会在底下重启，页面下次读取时显示新角色；等稳定后重载能重载的数据。
      settle.current = setTimeout(() => { reload(); repl.reload(); roster.reload(); }, 4000);
    }
  };

  return (
    <div className="card" style={{ padding: 0 }}>
      <div className="card-h">
        <div className="card-title"><Icons.Server size={14}/> 高可用</div>
      </div>
      <div style={{ padding: 16 }}>
        {loading && <div className="hint">读取中…</div>}
        {error && <div style={{ fontSize: 13, color: 'var(--rose)' }}>读取失败：{error}</div>}
        {!loading && !error && !paired && <StandaloneCluster />}
        {!loading && !error && paired && (
          <>
            <div style={{ display: 'grid', gridTemplateColumns: 'repeat(auto-fit, minmax(240px, 1fr))', gap: 16, fontSize: 14 }}>
              <div data-testid="ha-self">
                <div className="row meta" style={{ gap: 6, minHeight: 22 }}>
                  本机 <span className={`chip ${role.chip}`}>{role.text}</span>
                </div>
                <div className="row" style={{ gap: 6, marginTop: 6, minHeight: 32 }}>
                  <span className="mono" title={`任期 ${ha.epoch} · 节点 ${ha.node_id}`}>{selfIP || ha.node_id}</span>
                  {ha.role === 'active' && (
                    <>
                      <button className="btn" style={{ padding: '2px 10px', marginLeft: 4 }} disabled={busy} onClick={() => setConfirmOn(true)}>计划切换</button>
                      <Tip label="计划切换说明" text="维护本机（打补丁、重启、换硬件）前，先把主机交给一台备机：会等它把目录追平再切，追不平就不切，不丢改动。运行中的客户机会重启一次。"/>
                    </>
                  )}
                </div>
              </div>
              {ha.role === 'active' ? (
                <div>
                  <div className="row meta" style={{ gap: 6, minHeight: 22 }}>
                    其余节点 {!roster.error && others.some(n => n.online && (!n.ha_state || n.ha_state === 'standby')) && <span className="chip amber">备机</span>}
                  </div>
                  <div data-testid="ha-others" className="row" style={{ flexWrap: 'wrap', gap: 12, marginTop: 6, minHeight: 32 }}>
                    {roster.loading && <span className="meta">读取中…</span>}
                    {!roster.loading && roster.error && <span style={{ color: 'var(--rose)' }}>读不到节点列表：{roster.error}</span>}
                    {!roster.loading && !roster.error && others.length === 0 && <span className="meta">暂无</span>}
                    {!roster.error && others.map(n => (
                      <span key={n.id} className="row" style={{ gap: 4 }}>
                        <span className="mono">{n.ip || n.id}</span>
                        {!n.online
                          ? <span className="chip rose">离线</span>
                          : n.ha_state && n.ha_state !== 'standby' && <span className={`chip ${(ROLE[n.ha_state] || {}).chip || ''}`}>{(ROLE[n.ha_state] || {}).text || n.ha_state}</span>}
                      </span>
                    ))}
                  </div>
                </div>
              ) : (
                <div>
                  <div className="row meta" style={{ gap: 6, minHeight: 22 }}>
                    主机 {ha.peer_reachable
                      ? <span className={`chip ${(ROLE[ha.peer_role] || {}).chip || ''}`}>{(ROLE[ha.peer_role] || {}).text || ha.peer_role}</span>
                      : <span className="chip rose">不可达</span>}
                  </div>
                  <div className="mono" style={{ marginTop: 6, minHeight: 32, display: 'flex', alignItems: 'center' }}>{ha.peer_url}</div>
                </div>
              )}
              <div>
                <div className="row meta" style={{ gap: 6, minHeight: 22 }}>
                  目录同步 <span style={{ color: 'var(--fg)' }}>{replSummary(targets)}</span>
                </div>
                {ha.role === 'active' && (
                  <div className="row" style={{ flexWrap: 'wrap', gap: 8, marginTop: 6, minHeight: 32 }}>
                    <label className="meta" htmlFor="ha-rate">同步限速</label>
                    <input id="ha-rate" aria-label="同步限速" className="input" style={{ width: 90 }} type="number" min="0"
                      value={rate === null ? String(ha.rate_mbps ?? '') : rate}
                      onChange={e => setRate(e.target.value)}/>
                    <span className="meta">MB/s{ha.rate_source === 'default' ? '（默认值）' : ''}</span>
                    {rate !== null && rate !== String(ha.rate_mbps) && (
                      <button className="btn" disabled={busy} onClick={async () => {
                        const n = parseInt(rate, 10);
                        if (Number.isNaN(n) || n < 0) { store.toast('限速必须是 ≥ 0 的整数（0 = 不限速）', 'err'); return; }
                        if (await run(() => api.setHARate(n),
                          n === 0 ? '已取消限速——确认没有客户机在上课，同步会占满带宽' : `同步限速已设为 ${n} MB/s`,
                          { errMsg: '保存失败' })) { setRate(null); reload(); }
                      }}>保存限速</button>
                    )}
                    <Tip label="同步限速说明" text="目录同步和客户机共用网卡：营业时段保持限速；空闲时可设为 0 取消上限，下一轮同步生效。"/>
                  </div>
                )}
              </div>
            </div>
            {ha.role === 'standby' && (
              <div className="hint" style={{ marginTop: 12 }}>
                本机为备机，管理操作请到主机 <a className="mono" href={ha.peer_url} style={{ color: 'var(--cyan)' }}>{ha.peer_url}</a> 进行。
              </div>
            )}
          </>
        )}
      </div>

      <Modal open={confirmOn} onClose={() => !busy && setConfirmOn(false)} title="计划切换" size="md"
        footer={<>
          <button className="btn" disabled={busy} onClick={() => setConfirmOn(false)}>取消</button>
          <button className="btn primary" disabled={busy} onClick={doSwitch}>{busy ? '切换中…' : '切换'}</button>
        </>}>
        <div className="hint">
          先把目录同步追平（最多等 3 分钟，追不平则保持本机为主并放弃切换），然后本机重启为备机、
          虚 IP 漂到一台备机。<b>运行中的普通客户机会重启一次</b>（约 2~4 分钟回到桌面），超管机未保存的会话会丢失——请选无人上课的时间。
        </div>
      </Modal>
    </div>
  );
}

// StandaloneCluster 是单机节点唯一的 HA 操作：成为集群的第一个节点。其它机器从集群的「添加节点」加入。
function StandaloneCluster() {
  const store = useStore();
  const { busy, run } = useMutation(store.toast);
  const [creating, setCreating] = useState(false);
  const [vip, setVip] = useState('');

  const submitCreate = async () => {
    if (!vip.trim()) { store.toast('请填写虚 IP', 'err'); return; }
    if (await run(() => api.createCluster({ vip: vip.trim() }),
      '正在建立集群：本机将重启为主机并接管虚 IP，稍后请改用虚 IP 打开管理页面')) {
      setCreating(false);
    }
  };

  return (
    <>
      <div className="hint">本机是独立节点，未加入任何集群。</div>
      <div style={{ marginTop: 12, display: 'flex', gap: 8, alignItems: 'center', flexWrap: 'wrap' }}>
        <button className="btn primary" onClick={() => setCreating(true)}>创建集群…</button>
        <span className="hint">第一台建集群；其余机器在「服务器管理」里添加，不必逐台配置。</span>
      </div>

      <Modal open={creating} onClose={() => !busy && setCreating(false)} title="创建集群" size="md"
        footer={<>
          <button className="btn" disabled={busy} onClick={() => setCreating(false)}>取消</button>
          <button className="btn primary" disabled={busy} onClick={submitCreate}>{busy ? '创建中…' : '创建'}</button>
        </>}>
        <div style={{ display: 'grid', gap: 12 }} data-testid="create-cluster-form">
          <div className="hint">
            本机成为<b>主机</b>，管理页面和客户机此后都走虚 IP；其余机器在「服务器管理 → 添加节点」里加入。
            还没建数据池也能先创建，之后到「存储池管理」补建，建好之前备机无法接管。
          </div>
          <label style={{ display: 'grid', gap: 4 }}>
            <span className="meta">虚 IP</span>
            <input className="input" placeholder="例如 192.168.10.250" value={vip}
              autoFocus onChange={e => setVip(e.target.value)} />
            <span className="hint">客户机网段内未被占用的地址，不能落在分组的 IP 区间里。</span>
          </label>
        </div>
      </Modal>

    </>
  );
}

function replSummary(targets) {
  if (!targets.length) return <span className="meta">暂无同步记录</span>;
  const t = targets[0];
  if (t.last_error) return <span style={{ color: 'var(--rose)' }}>出错：{t.last_error}</span>;
  if (!t.last_snapshot) return <span className="meta">尚未同步</span>;
  return <span>{t.lag_seconds} 秒前 <span className="mono meta">({t.last_snapshot})</span></span>;
}

// StandbyBanner 在外壳中：备机上每个页面先给出去主机的入口，免得第一次写入就被闸门弹回。
// 持有 VIP 的备机例外：操作者本就经 VIP 进来，此时要说明为何还不是写入者、接下来会怎样。
export function StandbyBanner() {
  const { data, reload } = useResource(() => api.getHA(), []);
  const ha = data || {};
  const takingOver = ha.role === 'standby' && !!ha.holds_vip;
  React.useEffect(() => {
    if (!takingOver) return undefined;
    const timer = setInterval(reload, 10000); // 过渡状态，及时刷新避免显示旧信息
    return () => clearInterval(timer);
  }, [takingOver, reload]);
  if (ha.role !== 'standby') return null;
  const bar = {
    padding: '6px 14px', fontSize: 14, background: 'rgba(245,158,11,.12)',
    borderBottom: '1px solid var(--line)', display: 'flex', gap: 8, alignItems: 'center',
  };
  if (takingOver) {
    const t = ha.takeover || {};
    return (
      <div role="status" style={{ ...bar, flexWrap: 'wrap' }}>
        <Icons.Shield size={14}/>
        {t.phase === 'refused' ? (
          <>
            <span>本节点持有虚 IP，但还没有接任主机：{t.reason}。</span>
            <span className="hint">无需操作，产品会在几分钟内自行追平并接任；超过十分钟仍停在这里，请检查原主机上的 ndiskless 与 keepalived 服务。</span>
          </>
        ) : (
          <span>本节点持有虚 IP，正在接任主机{t.reason ? `：${t.reason}` : '，稍后页面会自动刷新'}。此时暂不能做管理操作。</span>
        )}
      </div>
    );
  }
  return (
    <div role="status" style={bar}>
      <Icons.Shield size={14}/>
      <span>当前是备机（只读跟随主机）。管理操作请到主机</span>
      <a className="mono" href={ha.peer_url} style={{ color: 'var(--cyan)' }}>{ha.peer_url}</a>
      <span>进行。</span>
    </div>
  );
}
