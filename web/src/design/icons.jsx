// 轻量描边图标，默认 16x16、currentColor。
import React from 'react';

const Icon = ({ d, size = 16, fill = "none", stroke = 2, children, ...rest }) => (
  <svg viewBox="0 0 24 24" width={size} height={size} fill={fill} stroke="currentColor" strokeWidth={stroke} strokeLinecap="round" strokeLinejoin="round" {...rest}>
    {d ? <path d={d} /> : children}
  </svg>
);

const Icons = {
  Dashboard: (p) => <Icon {...p}><rect x="3" y="3" width="7" height="9" rx="1.5"/><rect x="14" y="3" width="7" height="5" rx="1.5"/><rect x="14" y="12" width="7" height="9" rx="1.5"/><rect x="3" y="16" width="7" height="5" rx="1.5"/></Icon>,
  Server:   (p) => <Icon {...p}><rect x="3" y="4" width="18" height="6" rx="1.5"/><rect x="3" y="14" width="18" height="6" rx="1.5"/><circle cx="7" cy="7" r="0.6" fill="currentColor"/><circle cx="7" cy="17" r="0.6" fill="currentColor"/></Icon>,
  Monitor:  (p) => <Icon {...p}><rect x="3" y="4" width="18" height="12" rx="1.5"/><path d="M8 20h8M12 16v4"/></Icon>,
  Disk:     (p) => <Icon {...p}><ellipse cx="12" cy="6" rx="8" ry="3"/><path d="M4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6"/><path d="M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6"/></Icon>,
  // 系统标识：Windows 用实心四格窗（与描边的 Dashboard 区分），Linux 用企鹅。
  Windows:  (p) => <Icon fill="currentColor" stroke={0} {...p}><path d="M3 5.6 10.4 4.6v6.9H3zM11.6 4.4 21 3v8.5h-9.4zM3 12.7h7.4v6.9L3 18.6zM11.6 12.7H21V21l-9.4-1.4z"/></Icon>,
  Linux:    (p) => <Icon fill="currentColor" stroke={0} {...p}><path fillRule="evenodd" d="M12 2.5c-2.6 0-4.2 2.1-4.2 4.8 0 1.4-.5 2.4-1.4 3.6C5.3 12.5 4.3 14.6 4.3 17c0 2.4 3.4 3.8 7.7 3.8s7.7-1.4 7.7-3.8c0-2.4-1-4.5-2.1-6.1-.9-1.2-1.4-2.2-1.4-3.6 0-2.7-1.6-4.8-4.2-4.8zM12 10.4c-2.1 0-3.7 2.2-3.7 4.9s1.6 4.3 3.7 4.3 3.7-1.6 3.7-4.3-1.6-4.9-3.7-4.9zM10.3 5.5a1 1 0 1 0 0 2 1 1 0 1 0 0-2zM13.7 5.5a1 1 0 1 0 0 2 1 1 0 1 0 0-2zM10.5 8.3h3L12 9.9z"/><ellipse cx="7.6" cy="20.6" rx="3" ry="1.4"/><ellipse cx="16.4" cy="20.6" rx="3" ry="1.4"/></Icon>,
  Layers:   (p) => <Icon {...p}><path d="M12 3 2 8l10 5 10-5-10-5zM2 13l10 5 10-5M2 18l10 5 10-5"/></Icon>,
  Group:    (p) => <Icon {...p}><circle cx="9" cy="8" r="3"/><circle cx="17" cy="9" r="2.2"/><path d="M3 19c0-3 3-5 6-5s6 2 6 5M15 19c0-2 1.5-3.5 4-3.5s3 1.5 3 3.5"/></Icon>,
  Bell:     (p) => <Icon {...p}><path d="M6 9a6 6 0 0 1 12 0c0 5 2 6 2 6H4s2-1 2-6zM10 19a2 2 0 0 0 4 0"/></Icon>,
  Tasks:    (p) => <Icon {...p}><path d="M9 5h11M9 12h11M9 19h11"/><path d="M3.5 5.5l1.5 1.5 2-3M3.5 12.5l1.5 1.5 2-3M3.5 19.5l1.5 1.5 2-3"/></Icon>,
  Log:      (p) => <Icon {...p}><path d="M5 4h11l3 3v13a1 1 0 0 1-1 1H5a1 1 0 0 1-1-1V5a1 1 0 0 1 1-1z"/><path d="M8 10h8M8 14h8M8 18h5"/></Icon>,
  Cog:      (p) => <Icon {...p}><circle cx="12" cy="12" r="3"/><path d="M19.4 15a1.7 1.7 0 0 0 .3 1.8l.1.1a2 2 0 1 1-2.8 2.8l-.1-.1a1.7 1.7 0 0 0-1.8-.3 1.7 1.7 0 0 0-1 1.5V21a2 2 0 1 1-4 0v-.1a1.7 1.7 0 0 0-1.1-1.5 1.7 1.7 0 0 0-1.8.3l-.1.1a2 2 0 1 1-2.8-2.8l.1-.1a1.7 1.7 0 0 0 .3-1.8 1.7 1.7 0 0 0-1.5-1H3a2 2 0 1 1 0-4h.1a1.7 1.7 0 0 0 1.5-1.1 1.7 1.7 0 0 0-.3-1.8l-.1-.1a2 2 0 1 1 2.8-2.8l.1.1a1.7 1.7 0 0 0 1.8.3H9a1.7 1.7 0 0 0 1-1.5V3a2 2 0 1 1 4 0v.1a1.7 1.7 0 0 0 1 1.5 1.7 1.7 0 0 0 1.8-.3l.1-.1a2 2 0 1 1 2.8 2.8l-.1.1a1.7 1.7 0 0 0-.3 1.8V9a1.7 1.7 0 0 0 1.5 1H21a2 2 0 1 1 0 4h-.1a1.7 1.7 0 0 0-1.5 1z"/></Icon>,
  Search:   (p) => <Icon {...p}><circle cx="11" cy="11" r="7"/><path d="m20 20-3.5-3.5"/></Icon>,
  Plus:     (p) => <Icon {...p}><path d="M12 5v14M5 12h14"/></Icon>,
  Refresh:  (p) => <Icon {...p}><path d="M3 12a9 9 0 0 1 15.5-6.3L21 8M21 3v5h-5M21 12a9 9 0 0 1-15.5 6.3L3 16M3 21v-5h5"/></Icon>,
  Power:    (p) => <Icon {...p}><path d="M12 3v9M5.6 7.6a8 8 0 1 0 12.8 0"/></Icon>,
  Bolt:     (p) => <Icon {...p}><path d="M13 2 4 14h7l-1 8 9-12h-7z"/></Icon>,
  Network:  (p) => <Icon {...p}><circle cx="12" cy="6" r="2.5"/><circle cx="5" cy="18" r="2.5"/><circle cx="19" cy="18" r="2.5"/><path d="M12 8.5v3.5M7 16l4-4M17 16l-4-4"/></Icon>,
  Shield:   (p) => <Icon {...p}><path d="M12 3 4 6v6c0 4.5 3.5 8 8 9 4.5-1 8-4.5 8-9V6z"/></Icon>,
  Cpu:      (p) => <Icon {...p}><rect x="6" y="6" width="12" height="12" rx="1.5"/><rect x="9" y="9" width="6" height="6" rx="0.5"/><path d="M9 2v3M15 2v3M9 19v3M15 19v3M2 9h3M2 15h3M19 9h3M19 15h3"/></Icon>,
  Database: (p) => <Icon {...p}><ellipse cx="12" cy="5" rx="8" ry="2.5"/><path d="M4 5v14c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5V5"/><path d="M4 12c0 1.4 3.6 2.5 8 2.5s8-1.1 8-2.5"/></Icon>,
  Snapshot: (p) => <Icon {...p}><circle cx="12" cy="13" r="4"/><path d="M3 7h4l2-3h6l2 3h4v12H3z"/></Icon>,
  Chevron:  (p) => <Icon {...p}><path d="m9 6 6 6-6 6"/></Icon>,
  ArrowUp:  (p) => <Icon {...p}><path d="M12 19V5M5 12l7-7 7 7"/></Icon>,
  ArrowDn:  (p) => <Icon {...p}><path d="M12 5v14M19 12l-7 7-7-7"/></Icon>,
  Filter:   (p) => <Icon {...p}><path d="M3 5h18l-7 9v6l-4-2v-4z"/></Icon>,
  More:     (p) => <Icon {...p}><circle cx="6" cy="12" r="1.2" fill="currentColor"/><circle cx="12" cy="12" r="1.2" fill="currentColor"/><circle cx="18" cy="12" r="1.2" fill="currentColor"/></Icon>,
  Eye:      (p) => <Icon {...p}><path d="M2 12s3.5-7 10-7 10 7 10 7-3.5 7-10 7S2 12 2 12z"/><circle cx="12" cy="12" r="3"/></Icon>,
  Check:    (p) => <Icon {...p}><path d="m5 12 5 5L20 7"/></Icon>,
  X:        (p) => <Icon {...p}><path d="M6 6l12 12M18 6 6 18"/></Icon>,
  Heart:    (p) => <Icon {...p}><path d="M3 12h4l2-5 4 10 2-5h6"/></Icon>,
  Globe:    (p) => <Icon {...p}><circle cx="12" cy="12" r="9"/><path d="M3 12h18M12 3a14 14 0 0 1 0 18M12 3a14 14 0 0 0 0 18"/></Icon>,
  Backup:   (p) => <Icon {...p}><path d="M21 12a9 9 0 1 1-2.6-6.4"/><path d="M21 4v5h-5"/><path d="M9 13l3 3 5-6"/></Icon>,
  Clock:    (p) => <Icon {...p}><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3 2"/></Icon>,
  User:     (p) => <Icon {...p}><circle cx="12" cy="8" r="4"/><path d="M4 21c0-4.4 3.6-8 8-8s8 3.6 8 8"/></Icon>,
  Lock:     (p) => <Icon {...p}><rect x="4" y="11" width="16" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/></Icon>,
  Key:      (p) => <Icon {...p}><circle cx="8" cy="15" r="4"/><path d="M11 13l9-9M16 8l3 3M14 10l3 3"/></Icon>,
  Download: (p) => <Icon {...p}><path d="M12 4v12M5 12l7 7 7-7M5 21h14"/></Icon>,
  Upload:   (p) => <Icon {...p}><path d="M12 21V9M5 13l7-7 7 7M5 4h14"/></Icon>,
  Folder:   (p) => <Icon {...p}><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v9a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></Icon>,
  Tree:     (p) => <Icon {...p}><circle cx="6" cy="5" r="2"/><circle cx="18" cy="5" r="2"/><circle cx="12" cy="19" r="2"/><path d="M6 7v6a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2V7M12 11v6"/></Icon>,
  Trash:    (p) => <Icon {...p}><path d="M3 6h18M8 6V4a1 1 0 0 1 1-1h6a1 1 0 0 1 1 1v2M5 6l1 14a2 2 0 0 0 2 2h8a2 2 0 0 0 2-2l1-14"/><path d="M10 11v6M14 11v6"/></Icon>,
};

window.Icon = Icon;
window.Icons = Icons;

export { Icon, Icons };
