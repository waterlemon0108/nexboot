import React from 'react';
import { Icons } from '../icons.jsx';
import { BRAND, Logo, Wordmark } from '../brand.jsx';
import { api } from '../../lib/api.js';
const { useState: useLS, useEffect: useLE, useRef: useLR } = React;

// 页面不知道服务端有没有 TLS，但浏览器知道本次连接的协议，按实际情况显示。
const secure = typeof window !== "undefined" && window.location.protocol === "https:";

function LoginBootGrid() {
  // 纯装饰：一格格小终端模拟 PXE 开机。
  const [tick, setTick] = useLS(0);
  useLE(() => {
    const id = setInterval(() => setTick(t => t + 1), 180);
    return () => clearInterval(id);
  }, []);

  const cells = React.useMemo(() => {
    const arr = [];
    for (let i = 0; i < 96; i++) {
      // 每格开机起点和速度各不相同
      arr.push({
        offset: (i * 37) % 220,
        speed: 0.5 + ((i * 13) % 15) / 10,
        ip: `10.0.${(i >> 4) + 1}.${(i & 15) * 16 + 10}`,
        seat: `T${String((i % 24) + 1).padStart(2, "0")}`,
        flicker: ((i * 7) % 9) === 0,
      });
    }
    return arr;
  }, []);

  return (
    <div className="login-grid">
      {cells.map((c, i) => {
        const phase = ((tick * c.speed - c.offset) % 220 + 220) % 220;
        // 0–60 空闲，60–120 开机闪烁，120–200 在线，200–220 重新循环
        let state = "idle";
        if (phase < 60) state = "idle";
        else if (phase < 110) state = "boot";
        else if (phase < 200) state = "on";
        else state = "off";
        const load = state === "on" ? 30 + ((i * 17 + tick) % 60) : 0;
        return (
          <div key={i} className={`bgcell s-${state} ${c.flicker && state === "on" ? "flicker" : ""}`}>
            <div className="bgcell-h">
              <span className="bgdot"/>
              <span className="bgseat">{c.seat}</span>
            </div>
            <div className="bgip">{c.ip}</div>
            <div className="bgbar"><span style={{ width: `${load}%` }}/></div>
          </div>
        );
      })}
    </div>
  );
}

function LoginAside() {
  // 只放能力标签不放数字：真实统计要登录后才有，编出来的数字只会误导。
  const stats = [
    { label: "网络启动", value: "PXE", unit: "iPXE 链载", color: "emerald" },
    { label: "系统盘", value: "iSCSI", unit: "LIO 内核态", color: "cyan" },
    { label: "还原点", value: "ZFS", unit: "秒级克隆", color: "violet" },
    { label: "驱动适配", value: "离线", unit: "免进系统", color: "amber" },
  ];

  // 滚动的开机日志
  const logs = React.useMemo(() => {
    const samples = [
      "[ OK ] iPXE 1.21.1+ loaded · ROM 0x9000",
      "[ OK ] DHCP lease 10.0.3.142 → T-042",
      "[ OK ] TFTP undionly.kpxe 87.4 KiB",
      "[ OK ] iSCSI target nd-sys-win11.img attached",
      "[ OK ] write-cache vfs allocated 8 GiB",
      "[ ⋯ ] kernel handoff · 2.4s",
      "[ OK ] T-017 kernel ready · runlevel 5",
      "[ OK ] zfs import pool/games · 14.2 TB",
      "[ ⋯ ] image diff push → group LAN-A",
      "[ OK ] heartbeat 200 OK · srv-master-01",
      "[ ⋯ ] snapshot lol-s14 → 2 nodes · 12%",
      "[ OK ] T-088 logon admin@steam · 3.1s",
    ];
    return samples;
  }, []);

  const [logIdx, setLogIdx] = useLS(0);
  useLE(() => { const id = setInterval(() => setLogIdx(i => (i + 1) % logs.length), 950); return () => clearInterval(id); }, [logs.length]);
  const window6 = [];
  for (let i = 0; i < 7; i++) window6.push(logs[(logIdx + i) % logs.length]);

  return (
    <div className="login-aside">
      <div className="login-brand">
        <div className="login-mark"><Logo size={56}/></div>
        <div>
          <div className="login-brand-name"><Wordmark size={26}/></div>
          <div className="login-brand-sub">{BRAND.tagline} · {BRAND.taglineZh}</div>
        </div>
      </div>

      <div className="login-tag">
        <span className="mono" style={{ color: "var(--cyan)", letterSpacing: "0.18em", fontSize: 11 }}>{BRAND.motto}</span>
        <span style={{ flex: 1 }}/>
      </div>

      <div className="login-headline">
        <div className="login-eyebrow mono">ONE PLATFORM · EVERY ENDPOINT</div>
        <h1>一个平台，<br/>管好每一台无盘终端。</h1>
        <p>从镜像导入到还原点，从分组网络到在线状态，<br/>所有运维操作一处可达。</p>
      </div>

      <div className="login-stats">
        {stats.map((s, i) => (
          <div key={i} className={`login-stat c-${s.color}`}>
            <div className="login-stat-label">{s.label}</div>
            <div className="login-stat-num mono">{s.value}<span className="login-stat-unit"> {s.unit}</span></div>
          </div>
        ))}
      </div>

      <div className="login-log">
        <div className="login-log-h">
          <span className="dot cyan"/>
          <span className="mono" style={{ fontSize: 11, color: "var(--fg-mute)", letterSpacing: "0.12em" }}>BOOT&nbsp;·&nbsp;FLOW</span>
          <span style={{ flex: 1 }}/>
          <span className="mono meta">无盘启动流程示意</span>
        </div>
        <div className="login-log-body mono">
          {window6.map((l, i) => (
            <div key={`${logIdx}-${i}`} className="login-log-line" style={{ opacity: 0.4 + (i / window6.length) * 0.6 }}>
              <span style={{ color: "var(--fg-faint)" }}>{String(Date.now() % 100000 + i * 7).slice(-5)} </span>
              <span style={{ color: l.includes("[ ⋯ ]") ? "var(--amber)" : l.includes("[ OK ]") ? "var(--emerald)" : "var(--fg-mute)" }}>
                {l}
              </span>
            </div>
          ))}
        </div>
      </div>
    </div>
  );
}

function LoginForm({ onLogin }) {
  const [user, setUser] = useLS("admin");
  const [pass, setPass] = useLS("");
  const [showPass, setShowPass] = useLS(false);
  // 安全相关的文案必须属实：服务端不做 TLS（见 README 部署前提），可能挂在反代后面，
  // 所以按浏览器实际协议显示。运维会据此判断能否放到共享网络里。
  const [busy, setBusy] = useLS(false);
  const [step, setStep] = useLS(""); // 开机动画步骤
  const [err, setErr] = useLS("");

  const submit = async (e) => {
    e && e.preventDefault();
    if (!user || !pass) { setErr("请输入用户名与密码"); return; }
    setErr("");
    setBusy(true);
    setStep("AUTH · 校验凭据...");
    let data;
    try {
      data = await api.login(user, pass);
    } catch (ex) {
      setBusy(false);
      setStep("");
      setErr(ex.message || "登录失败");
      return;
    }
    // 登录成功后先播开机过渡动画再进控制台
    const steps = [
      "RBAC · 加载角色权限...",
      "TOKEN · 签发会话密钥...",
      "BOOT · 进入控制台",
    ];
    let i = -1;
    const tick = () => {
      i++;
      if (i < steps.length) {
        setStep(steps[i]);
        setTimeout(tick, 300);
      } else {
        setTimeout(() => { onLogin(data.user || { username: user }); }, 200);
      }
    };
    setTimeout(tick, 300);
  };

  return (
    <form className="login-card" onSubmit={submit}>
      <div className="login-card-corner tl"/>
      <div className="login-card-corner tr"/>
      <div className="login-card-corner bl"/>
      <div className="login-card-corner br"/>

      <div className="login-card-head">
        <div className="row" style={{ gap: 10 }}>
          <span className={secure ? "dot cyan" : "dot amber"}/>
          <span className="mono" style={{ fontSize: 11, letterSpacing: "0.16em", color: "var(--fg-mute)" }}>
            {secure ? <>SECURE&nbsp;·&nbsp;TLS</> : <>内网专用&nbsp;·&nbsp;明文&nbsp;HTTP</>}
          </span>
        </div>
        <div className="mono" style={{ fontSize: 11, color: "var(--fg-faint)" }}>
          {new Date().toISOString().slice(0, 19).replace("T", " ")}
        </div>
      </div>

      <h2 className="login-title">
        登录控制台
        <span className="login-title-cursor"/>
      </h2>
      <div className="login-sub">使用管理员账户进入 {BRAND.name} 控制台</div>

      <label className="login-field">
        <span className="login-field-label">
          <span>用户名</span>
          <span className="mono" style={{ color: "var(--fg-faint)", fontSize: 11 }}>USER</span>
        </span>
        <div className="login-input">
          <Icons.Group size={14}/>
          <input
            value={user}
            onChange={(e) => setUser(e.target.value)}
            placeholder="admin"
            autoComplete="username"
            disabled={busy}
          />
        </div>
      </label>

      <label className="login-field">
        <span className="login-field-label">
          <span>密码</span>
          <span className="mono" style={{ color: "var(--fg-faint)", fontSize: 11 }}>PASS</span>
        </span>
        <div className="login-input">
          <Icons.Shield size={14}/>
          <input
            type={showPass ? "text" : "password"}
            value={pass}
            onChange={(e) => setPass(e.target.value)}
            placeholder="••••••••"
            autoComplete="current-password"
            disabled={busy}
          />
          <button
            type="button"
            className="login-eye"
            onClick={() => setShowPass(s => !s)}
            tabIndex={-1}
            title={showPass ? "隐藏" : "显示"}
          >
            <Icons.Eye size={14}/>
          </button>
        </div>
      </label>


      {err && (
        <div className="login-err">
          <span className="dot err"/>
          <span>{err}</span>
        </div>
      )}

      <button type="submit" className={`login-submit ${busy ? "busy" : ""}`} disabled={busy}>
        <span className="login-submit-bg"/>
        <span className="login-submit-content">
          {busy ? (
            <>
              <span className="login-spin"/>
              <span className="mono" style={{ fontSize: 12 }}>{step}</span>
            </>
          ) : (
            <>
              <Icons.Power size={14}/>
              <span>进入控制台</span>
              <span style={{ flex: 1 }}/>
              <span className="mono" style={{ fontSize: 11, opacity: 0.7 }}>↵</span>
            </>
          )}
        </span>
      </button>

      {/* 页脚只写登录前能确知的事：运维输入的地址和服务是否健康（/healthz 公开）。
          不写节点名或集群规模，登录前无从知道。 */}
      <ServiceFoot/>
    </form>
  );
}

function ServiceFoot() {
  const [health, setHealth] = useLS("checking");
  useLE(() => {
    let alive = true;
    fetch("/healthz")
      .then(r => alive && setHealth(r.ok ? "ok" : "bad"))
      .catch(() => alive && setHealth("down"));
    return () => { alive = false; };
  }, []);
  const text = health === "ok" ? "服务正常"
    : health === "checking" ? "检查中…"
      : health === "bad" ? "服务不可用"
        : "连不上服务";
  const color = health === "ok" ? "var(--emerald)" : health === "checking" ? "var(--fg-faint)" : "var(--rose)";
  return (
    <div className="login-foot">
      <div className="login-foot-row">
        <span className="mono" style={{ fontSize: 11, color: "var(--fg-faint)" }}>
          {typeof window !== "undefined" ? window.location.host : ""}
        </span>
        <span className="dot" style={{ width: 6, height: 6, background: color }}/>
      </div>
      <div className="login-foot-row">
        <span className="mono" style={{ fontSize: 11, color }}>{text}</span>
        <Icons.Server size={11}/>
      </div>
    </div>
  );
}

function LoginPage({ onLogin }) {
  return (
    <div className="login-root">
      <LoginBootGrid/>
      <div className="login-vignette"/>
      <div className="login-scanline"/>

      <div className="login-stage">
        <LoginAside/>
        <div className="login-form-wrap">
          <LoginForm onLogin={onLogin}/>
        </div>
      </div>

      <div className="login-bottom">
        <span className="mono" style={{ fontSize: 11, color: "var(--fg-faint)", letterSpacing: "0.12em" }}>
          © NEXBOOT · DISKLESS ENDPOINT PLATFORM
          {secure ? " · TLS" : " · 请部署在专用管理/PXE 网段内"}
        </span>
        <span style={{ flex: 1 }}/>
        <span className="mono meta">简体中文 · 中国 / 上海</span>
      </div>
    </div>
  );
}

window.LoginPage = LoginPage;

export { LoginPage };
