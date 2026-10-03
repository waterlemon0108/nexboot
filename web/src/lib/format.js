// 公共格式化函数和标签表，各页面统一从这里取。
export const pad2 = (n) => String(n).padStart(2, "0");

// null（new Date(null) 是 1970 年）和 Go 零值时间都表示「尚未」。
const toDate = (iso) => {
  if (!iso) return null;
  const d = new Date(iso);
  return isNaN(d) || d.getFullYear() < 1971 ? null : d;
};

export const fmtDateTime = (iso) => {
  const d = toDate(iso);
  if (!d) return "—";
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
};

export const fmtDateTimeSec = (iso) => {
  const d = toDate(iso);
  if (!d) return "—";
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
};

export const fmtTime = (iso) => {
  const d = new Date(iso);
  if (isNaN(d)) return "—";
  return `${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
};

export const GiB = 1024 ** 3;
export const TiB = 1024 ** 4;

export const fmtCap = (b) => {
  b = Number(b || 0);
  if (b >= TiB) return `${(b / TiB).toFixed(2)} TiB`;
  if (b >= GiB) return `${(b / GiB).toFixed(1)} GiB`;
  return `${(b / (1024 ** 2)).toFixed(0)} MiB`;
};

// 任务名用界面上按钮的说法而非内部动作名，否则操作者对不上自己点的是哪件事。
export const TASK_LABELS = {
  import_image: "镜像导入", export_image: "镜像导出", create_blank_image: "新建数据盘", create_config: "创建配置", delete_config: "删除配置",
  create_reduction: "创建还原点", delete_reduction: "删除还原点",
  merge_config: "覆盖原镜像", merge_reduction: "合并还原点", copy_image: "另存为新镜像",
  super_stop: "关机存还原点", publish_data_disk: "发布数据盘",
  create_pool: "创建存储池", destroy_pool: "销毁存储池", add_disk: "添加存储盘",
  migrate_disk: "移除磁盘", replace_disk: "替换磁盘", detach_disk: "摘除镜像盘", mirror_upgrade: "升级为镜像",
  add_special: "添加元数据盘", remove_special: "移除元数据盘", add_spare: "添加热备盘", remove_spare: "移除热备盘",
  add_read_cache: "添加读缓存", remove_read_cache: "移除读缓存",
  add_write_cache: "添加写缓存", remove_write_cache: "移除写缓存",
  flush_cache: "刷新写缓存", backup_dataset: "数据备份", image_health_check: "镜像体检",
};

// redName 返回还原点的显示名：优先用操作者输入的名称，没有显示名的旧记录退回 ASCII 快照名。
export const redName = (r) => (r && (r.DisplayName || r.Name)) || "";
