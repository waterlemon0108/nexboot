// 品牌：名称、字标和标志集中在一处，侧栏、登录页、标签页标题保持一致。
// 标志按 NexBoot 品牌板：开口六边形框（轻量无盘终端）、三个相连节点（集中编排）、
// 从底座升起的向上箭头（网络启动）。配色青 #00E0FF 到蓝 #1A6DFF，与控制台青色强调色协调。
import React from 'react';

export const BRAND = {
  name: "NexBoot",
  nameLead: "Nex",
  nameAccent: "Boot",
  tagline: "Diskless Endpoint Platform",
  taglineZh: "无盘终端平台",
  motto: "CENTRALIZE · DEPLOY · OPERATE",
  cyan: "#00E0FF",
  sky: "#4DA3FF",
  blue: "#1A6DFF",
  navy: "#0D1B2E",
  grey: "#A3B1C6",
};

// 标志在各尺寸下用同一幅图，侧栏、登录页和标签页图标才像同一个品牌。
export function Logo({ size = 28, title = BRAND.name, style }) {
  const gid = React.useId().replace(/:/g, "");
  return (
    <svg width={size} height={size} viewBox="0 0 64 64" role="img" aria-label={title} style={{ display: "block", flexShrink: 0, ...style }}>
      <defs>
        <linearGradient id={`${gid}-frame`} x1="0" y1="0" x2="1" y2="1">
          <stop offset="0" stopColor={BRAND.cyan}/>
          <stop offset="1" stopColor={BRAND.blue}/>
        </linearGradient>
        <linearGradient id={`${gid}-arrow`} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stopColor={BRAND.cyan}/>
          <stop offset="1" stopColor={BRAND.blue}/>
        </linearGradient>
      </defs>
      {/* 六边形框 */}
      <polygon points="32,4 56.2,18 56.2,46 32,60 7.8,46 7.8,18" fill="none" stroke={`url(#${gid}-frame)`} strokeWidth="4" strokeLinejoin="round"/>
      {/* 节点网络 */}
      <g>
        <path d="M32 15 L19 27 M32 15 L45 27 M19 27 L32 33 M45 27 L32 33" fill="none" stroke={BRAND.sky} strokeWidth="1.6" strokeDasharray="2.2 2.2" opacity="0.9"/>
        <circle cx="32" cy="15" r="3.2" fill={BRAND.sky} stroke="#fff" strokeWidth="1.3"/>
        <circle cx="19" cy="27" r="2.8" fill={BRAND.sky} stroke="#fff" strokeWidth="1.3"/>
        <circle cx="45" cy="27" r="2.8" fill={BRAND.sky} stroke="#fff" strokeWidth="1.3"/>
      </g>
      {/* 向上箭头 */}
      <path d="M32 19 L23.5 29 L28.5 29 L28.5 44 L35.5 44 L35.5 29 L40.5 29 Z" fill={`url(#${gid}-arrow)`}/>
      {/* 底座 */}
      <rect x="15" y="45" width="34" height="10" rx="3" fill="#E9F0F8"/>
      <rect x="19.5" y="49" width="14" height="2" rx="1" fill={BRAND.navy}/>
      <circle cx="43" cy="50" r="1.7" fill={BRAND.navy}/>
    </svg>
  );
}

// 字标："Nex" 用表面文字色，"Boot" 用品牌蓝。
export function Wordmark({ size = 14, style }) {
  return (
    <span style={{ fontSize: size, fontWeight: 700, letterSpacing: "-0.01em", lineHeight: 1, whiteSpace: "nowrap", ...style }}>
      <span>{BRAND.nameLead}</span>
      <span className="brand-accent">{BRAND.nameAccent}</span>
    </span>
  );
}
