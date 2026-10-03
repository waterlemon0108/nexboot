// ── 浮层体系 ───────────────────────────────────────────────────────────
// 统一管理遮罩、z-index、进场动画、ESC 关闭、点背板关闭和 body 滚动锁定，保证各弹层定位一致。
// `placement` 选锚点：center（弹窗）、drawer（右侧）、top（命令面板）。
import React from 'react';
import { createPortal } from 'react-dom';
import { Icons } from './icons.jsx';

const MODAL_SIZE = { sm: 440, md: 560, lg: 700, xl: 820 };
// 共享栈保证嵌套浮层正确：ESC 只关最上层，最后一个浮层卸载后才释放 body 滚动锁定。
const overlayStack = [];
// 滚动锁原值只在栈从空变非空时记一次；各层各自记录会把别层设的 "hidden" 当成原值，全关后滚动锁死。
let bodyOverflowBeforeLock = "";

function Overlay({ open, onClose, placement = "center", size = "md", width, closeOnBackdrop = true, className, panelStyle, children }) {
  // 调用方多传内联 onClose；放进 effect 依赖会让每次重渲染都把本层重新压到栈顶，ESC 关错层。
  const onCloseRef = React.useRef(onClose);
  onCloseRef.current = onClose;
  React.useEffect(() => {
    if (!open) return;
    const token = {};
    if (overlayStack.length === 0) {
      bodyOverflowBeforeLock = document.body.style.overflow;
      document.body.style.overflow = "hidden";
    }
    overlayStack.push(token);
    const onKey = (e) => {
      if (e.key === "Escape" && onCloseRef.current && overlayStack[overlayStack.length - 1] === token) onCloseRef.current();
    };
    document.addEventListener("keydown", onKey);
    return () => {
      document.removeEventListener("keydown", onKey);
      const i = overlayStack.indexOf(token);
      if (i >= 0) overlayStack.splice(i, 1);
      if (overlayStack.length === 0) document.body.style.overflow = bodyOverflowBeforeLock;
    };
  }, [open]);
  if (!open) return null;

  const w = width || MODAL_SIZE[size] || MODAL_SIZE.md;
  const scrim = {
    position: "fixed", inset: 0, zIndex: "var(--z-overlay)",
    background: "var(--overlay-scrim)", backdropFilter: "blur(4px)",
    display: "grid", animation: "fadeIn 0.15s",
  };
  let panel;
  if (placement === "drawer") {
    scrim.placeItems = "stretch end";
    panel = { width: w, maxWidth: "100vw", height: "100%", animation: "slideIn 0.2s ease" };
  } else if (placement === "top") {
    scrim.placeItems = "start center"; scrim.paddingTop = "12vh";
    panel = { width: w, maxWidth: "94vw", maxHeight: "60vh" };
  } else {
    scrim.placeItems = "center";
    panel = { width: w, maxWidth: "94vw", maxHeight: "min(85vh, 720px)" };
  }
  // Portal 到 <body>，让 fixed 遮罩脱离带 transform/filter 的祖先（如 .main 内容区），
  // 否则会被困住、居中弹窗偏到右边。
  return createPortal(
    <div style={scrim} onClick={() => closeOnBackdrop && onClose && onClose()}>
      <div onClick={e => e.stopPropagation()} className={className}
        style={{ display: "flex", flexDirection: "column", overflow: "hidden", ...panel, ...panelStyle }}>
        {children}
      </div>
    </div>,
    document.body,
  );
}

// Modal 是居中变体，带标准卡片框（标题 + 关闭 + 底栏）。
function Modal({ open, onClose, title, children, footer, size = "md", width }) {
  return (
    <Overlay open={open} onClose={onClose} placement="center" size={size} width={width} className="card">
      <div className="card-h">
        <div className="card-title-zh" style={{ fontSize: 14 }}>{title}</div>
        <button className="btn ghost icon" onClick={onClose}><Icons.X size={14}/></button>
      </div>
      <div style={{ padding: 18, overflowY: "auto", flex: 1 }}>{children}</div>
      {footer && <div style={{ padding: "12px 18px", borderTop: "1px solid var(--line-soft)", display: "flex", justifyContent: "flex-end", gap: 8 }}>{footer}</div>}
    </Overlay>
  );
}

// 确认弹窗。`typeToConfirm: "<name>"` 要求输入该名称后才解锁确认按钮，用于不可逆的数据销毁操作。
// 弹窗自己渲染，调用方只拿到 `ask`；若要求调用方自己放置节点，漏放时 Promise 永远不会 resolve。
const ConfirmContext = React.createContext(null);

function ConfirmProvider({ children }) {
  const [state, setState] = React.useState(null);
  const [typed, setTyped] = React.useState("");
  const ask = React.useCallback(
    (opts) => new Promise(resolve => { setTyped(""); setState({ ...opts, resolve }); }),
    [],
  );
  const close = (answer) => { state?.resolve(answer); setState(null); };
  const locked = !!state?.typeToConfirm && typed !== state.typeToConfirm;
  return (
    <ConfirmContext.Provider value={ask}>
      {children}
      {state && (
        <Modal open size="sm" onClose={() => close(false)} title={state.title || "确认操作"}
          footer={<>
            <button className="btn" onClick={() => close(false)}>取消</button>
            <button className={`btn ${state.danger ? "danger" : "primary"}`} disabled={locked} onClick={() => close(true)}>{state.confirmText || "确认"}</button>
          </>}>
          <div style={{ fontSize: 14, color: "var(--fg-mute)", lineHeight: 1.6, whiteSpace: "pre-wrap" }}>{state.message}</div>
          {state.typeToConfirm && (
            <div style={{ marginTop: 12 }}>
              <div style={{ fontSize: 12, color: "var(--fg-faint)", marginBottom: 6 }}>
                请输入 <span className="mono" style={{ color: "var(--rose)", fontWeight: 600 }}>{state.typeToConfirm}</span> 以确认：
              </div>
              <input className="input mono" style={{ width: "100%" }} value={typed} autoFocus
                onChange={e => setTyped(e.target.value)} placeholder={state.typeToConfirm}/>
            </div>
          )}
        </Modal>
      )}
    </ConfirmContext.Provider>
  );
}

// useConfirm 返回 ask 函数。`node` 保留为无作用的值，使现有的 `{conf.node}` 写法在页面迁移期间仍有效。
function useConfirm() {
  const ask = React.useContext(ConfirmContext);
  if (!ask) throw new Error("useConfirm 需要 ConfirmProvider 包裹");
  return { ask, node: null };
}

window.Overlay = Overlay;
window.Modal = Modal;
window.useConfirm = useConfirm;
window.ConfirmProvider = ConfirmProvider;

export { Overlay, Modal, useConfirm, ConfirmProvider };
