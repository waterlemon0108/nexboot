package store

import (
	"database/sql"
	"encoding/json"
	"time"

	"github.com/tianwei/diskless/internal/domain"
)

var imageCodec = entityCodec[domain.Image]{
	table:   "images",
	columns: []string{"id", "name", "os_type", "size", "state", "remark", "mount_script_version", "purpose", "origin", "created_at"},
	values: func(v domain.Image) []any {
		// 没写用途/来源的镜像是导入的系统盘（当时唯一的类型），零值不会撞上 CHECK 约束。
		purpose, origin := v.Purpose, v.Origin
		if purpose == "" {
			purpose = domain.ImagePurposeSystem
		}
		if origin == "" {
			origin = domain.ImageOriginImported
		}
		return []any{v.ID, v.Name, string(v.OSType), v.Size, string(v.State), v.Remark, v.MountScriptVersion, string(purpose), string(origin), timeValue(v.CreatedAt)}
	},
	scan: func(s rowScanner) (domain.Image, error) {
		var v domain.Image
		var osType, state, purpose, origin, created string
		if err := s.Scan(&v.ID, &v.Name, &osType, &v.Size, &state, &v.Remark, &v.MountScriptVersion, &purpose, &origin, &created); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		v.OSType, v.State, v.CreatedAt = domain.OSType(osType), domain.ImageState(state), t
		v.Purpose, v.Origin = domain.ImagePurpose(purpose), domain.ImageOrigin(origin)
		return v, err
	},
}

var configCodec = entityCodec[domain.Config]{
	table:   "configs",
	columns: []string{"id", "image_id", "name", "default_reduction_id", "created_at"},
	values: func(v domain.Config) []any {
		return []any{v.ID, v.ImageID, v.Name, stringPtrValue(v.DefaultReductionID), timeValue(v.CreatedAt)}
	},
	scan: func(s rowScanner) (domain.Config, error) {
		var v domain.Config
		var reduction sql.NullString
		var created string
		if err := s.Scan(&v.ID, &v.ImageID, &v.Name, &reduction, &created); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		v.DefaultReductionID, v.CreatedAt = stringPtr(reduction), t
		return v, err
	},
}

var reductionCodec = entityCodec[domain.Reduction]{
	table:   "reductions",
	columns: []string{"id", "config_id", "name", "display_name", "created_at", "status", "remark"},
	values: func(v domain.Reduction) []any {
		return []any{v.ID, v.ConfigID, v.Name, v.DisplayName, timeValue(v.CreatedAt), string(v.Status), v.Remark}
	},
	scan: func(s rowScanner) (domain.Reduction, error) {
		var v domain.Reduction
		var created, status string
		if err := s.Scan(&v.ID, &v.ConfigID, &v.Name, &v.DisplayName, &created, &status, &v.Remark); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		v.CreatedAt, v.Status = t, domain.ReductionStatus(status)
		return v, err
	},
}

var groupCodec = entityCodec[domain.Group]{
	table:   "groups",
	columns: []string{"id", "name", "is_default", "start_ip", "client_max", "gateway", "netmask", "dns1", "dns2", "system_image_id", "system_config_id", "system_reduction_id", "storage_server_id"},
	values: func(v domain.Group) []any {
		return []any{v.ID, v.Name, v.IsDefault, v.StartIP, v.ClientMax, v.Gateway, v.Netmask, v.DNS1, v.DNS2, v.SystemImageID, v.SystemConfigID, v.SystemReductionID, stringPtrValue(v.StorageServerID)}
	},
	scan: func(s rowScanner) (domain.Group, error) {
		var v domain.Group
		var storageServer sql.NullString
		if err := s.Scan(&v.ID, &v.Name, &v.IsDefault, &v.StartIP, &v.ClientMax, &v.Gateway, &v.Netmask, &v.DNS1, &v.DNS2, &v.SystemImageID, &v.SystemConfigID, &v.SystemReductionID, &storageServer); err != nil {
			return v, err
		}
		v.StorageServerID = stringPtr(storageServer)
		return v, nil
	},
}

var groupDiskCodec = entityCodec[domain.GroupDisk]{
	table:   "group_disks",
	columns: []string{"id", "group_id", "mount_target", "image_id", "config_id"},
	values: func(v domain.GroupDisk) []any {
		return []any{v.ID, v.GroupID, v.MountTarget, v.ImageID, v.ConfigID}
	},
	scan: func(s rowScanner) (domain.GroupDisk, error) {
		var v domain.GroupDisk
		err := s.Scan(&v.ID, &v.GroupID, &v.MountTarget, &v.ImageID, &v.ConfigID)
		return v, err
	},
}

var terminalCodec = entityCodec[domain.Terminal]{
	table:   "terminals",
	columns: []string{"id", "name", "mac", "ip", "group_id", "is_super", "state", "online_since", "offline_at", "last_heartbeat_at", "pending_bundle_id", "pending_bundle_at", "storage_server_id"},
	values: func(v domain.Terminal) []any {
		return []any{v.ID, v.Name, v.MAC, v.IP, v.GroupID, v.IsSuper, string(v.State), timePtrValue(v.OnlineSince), timePtrValue(v.OfflineAt), timePtrValue(v.LastHeartbeatAt), stringPtrValue(v.PendingBundleID), timePtrValue(v.PendingBundleAt), stringPtrValue(v.StorageServerID)}
	},
	scan: func(s rowScanner) (domain.Terminal, error) {
		var v domain.Terminal
		var name, state string
		var online, offline, heartbeat, pendingBundleID, pendingBundleAt, storageServer sql.NullString
		if err := s.Scan(&v.ID, &name, &v.MAC, &v.IP, &v.GroupID, &v.IsSuper, &state, &online, &offline, &heartbeat, &pendingBundleID, &pendingBundleAt, &storageServer); err != nil {
			return v, err
		}
		v.StorageServerID = stringPtr(storageServer)
		var err error
		v.Name = name
		v.State = domain.TerminalState(state)
		v.PendingBundleID = stringPtr(pendingBundleID)
		if v.OnlineSince, err = timePtr(online); err != nil {
			return v, err
		}
		if v.OfflineAt, err = timePtr(offline); err != nil {
			return v, err
		}
		if v.LastHeartbeatAt, err = timePtr(heartbeat); err != nil {
			return v, err
		}
		v.PendingBundleAt, err = timePtr(pendingBundleAt)
		return v, err
	},
}

var serverCodec = entityCodec[domain.Server]{
	table:   "servers",
	columns: []string{"id", "name", "ip", "portal_ip", "api_url", "role", "status", "heartbeat", "epoch", "ha_state", "last_seen_at"},
	values: func(v domain.Server) []any {
		return []any{v.ID, v.Name, v.IP, v.PortalIP, v.APIURL, string(v.Role), string(v.Status), timePtrValue(v.Heartbeat), v.Epoch, v.HAState, timePtrValue(v.LastSeenAt)}
	},
	scan: func(s rowScanner) (domain.Server, error) {
		var v domain.Server
		var role, status string
		var heartbeat, lastSeen sql.NullString
		if err := s.Scan(&v.ID, &v.Name, &v.IP, &v.PortalIP, &v.APIURL, &role, &status, &heartbeat, &v.Epoch, &v.HAState, &lastSeen); err != nil {
			return v, err
		}
		t, err := timePtr(heartbeat)
		if err != nil {
			return v, err
		}
		seen, err := timePtr(lastSeen)
		v.Role, v.Status, v.Heartbeat, v.LastSeenAt = domain.ServerRole(role), domain.ServerStatus(status), t, seen
		return v, err
	},
}

var poolCodec = entityCodec[domain.Pool]{
	table:   "pools",
	columns: []string{"id", "server_id", "name", "disks", "read_cache_disks", "write_cache_disks", "layout", "group_width", "capacity", "used"},
	values: func(v domain.Pool) []any {
		// 没写布局的池是条带（产品唯一建过的类型），零值不会撞上 CHECK 约束。
		layout := v.Layout
		if layout == "" {
			layout = domain.PoolLayoutStripe
		}
		width := v.GroupWidth
		if width < 1 {
			width = 1
		}
		return []any{v.ID, v.ServerID, v.Name, stringsValue(v.Disks), stringsValue(v.ReadCacheDisks), stringsValue(v.WriteCacheDisks), string(layout), width, v.Capacity, v.Used}
	},
	scan: func(s rowScanner) (domain.Pool, error) {
		var v domain.Pool
		var disks, readCache, writeCache, layout string
		if err := s.Scan(&v.ID, &v.ServerID, &v.Name, &disks, &readCache, &writeCache, &layout, &v.GroupWidth, &v.Capacity, &v.Used); err != nil {
			return v, err
		}
		v.Layout = domain.PoolLayout(layout)
		var err error
		if v.Disks, err = parseStrings(disks); err != nil {
			return v, err
		}
		if v.ReadCacheDisks, err = parseStrings(readCache); err != nil {
			return v, err
		}
		v.WriteCacheDisks, err = parseStrings(writeCache)
		return v, err
	},
}

var poolDiskCodec = entityCodec[domain.PoolDisk]{
	table:   "pool_disks",
	columns: []string{"id", "pool_id", "path", "role", "vdev", "capacity", "used"},
	values: func(v domain.PoolDisk) []any {
		return []any{v.ID, v.PoolID, v.Path, string(v.Role), v.Vdev, v.Capacity, v.Used}
	},
	scan: func(s rowScanner) (domain.PoolDisk, error) {
		var v domain.PoolDisk
		var role string
		err := s.Scan(&v.ID, &v.PoolID, &v.Path, &role, &v.Vdev, &v.Capacity, &v.Used)
		v.Role = domain.PoolDiskRole(role)
		return v, err
	},
}

var clientCloneCodec = entityCodec[domain.ClientClone]{
	table:   "client_clones",
	columns: []string{"id", "terminal_mac", "kind", "config_id", "reduction_id", "server_id", "target", "lun", "volpath"},
	values: func(v domain.ClientClone) []any {
		return []any{v.ID, v.TerminalMAC, string(v.Kind), v.ConfigID, v.ReductionID, v.ServerID, v.Target, v.LUN, v.VolPath}
	},
	scan: func(s rowScanner) (domain.ClientClone, error) {
		var v domain.ClientClone
		var kind string
		err := s.Scan(&v.ID, &v.TerminalMAC, &kind, &v.ConfigID, &v.ReductionID, &v.ServerID, &v.Target, &v.LUN, &v.VolPath)
		v.Kind = domain.CloneKind(kind)
		return v, err
	},
}

var taskCodec = entityCodec[domain.Task]{
	table:   "tasks",
	columns: []string{"id", "type", "target_ref", "status", "progress", "message", "result", "error", "created_at", "finished_at", "node"},
	values: func(v domain.Task) []any {
		return []any{v.ID, string(v.Type), v.TargetRef, string(v.Status), v.Progress, v.Message, v.Result, v.Error, timeValue(v.CreatedAt), timePtrValue(v.FinishedAt), v.Node}
	},
	scan: func(s rowScanner) (domain.Task, error) {
		var v domain.Task
		var typ, status, created string
		var finished sql.NullString
		if err := s.Scan(&v.ID, &typ, &v.TargetRef, &status, &v.Progress, &v.Message, &v.Result, &v.Error, &created, &finished, &v.Node); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		if err != nil {
			return v, err
		}
		v.CreatedAt, v.Type, v.Status = t, domain.TaskType(typ), domain.TaskStatus(status)
		v.FinishedAt, err = timePtr(finished)
		return v, err
	},
}

var alarmCodec = entityCodec[domain.Alarm]{
	table:   "alarms",
	columns: []string{"id", "alarm_key", "severity", "type", "source", "resource", "threshold", "value", "status", "message", "created_at", "updated_at", "recovered_at"},
	values: func(v domain.Alarm) []any {
		return []any{v.ID, v.AlarmKey, v.Severity, v.Type, v.Source, v.Resource, v.Threshold, v.Value, v.Status, v.Message, timeValue(v.CreatedAt), timeValue(v.UpdatedAt), timePtrValue(v.RecoveredAt)}
	},
	scan: func(s rowScanner) (domain.Alarm, error) {
		var v domain.Alarm
		var created, updated string
		var recovered sql.NullString
		if err := s.Scan(&v.ID, &v.AlarmKey, &v.Severity, &v.Type, &v.Source, &v.Resource, &v.Threshold, &v.Value, &v.Status, &v.Message, &created, &updated, &recovered); err != nil {
			return v, err
		}
		var err error
		if v.CreatedAt, err = parseTime(created); err != nil {
			return v, err
		}
		if v.UpdatedAt, err = parseTime(updated); err != nil {
			return v, err
		}
		v.RecoveredAt, err = timePtr(recovered)
		return v, err
	},
}

var auditLogCodec = entityCodec[domain.AuditLog]{
	table:   "audit_logs",
	columns: []string{"id", "type", "username", "action", "module", "detail", "ip", "user_agent", "status", "http_status", "cost_ms", "created_at"},
	values: func(v domain.AuditLog) []any {
		return []any{v.ID, v.Type, v.Username, v.Action, v.Module, v.Detail, v.IP, v.UserAgent, v.Status, v.HTTPStatus, v.CostMs, timeValue(v.CreatedAt)}
	},
	scan: func(s rowScanner) (domain.AuditLog, error) {
		var v domain.AuditLog
		var created string
		if err := s.Scan(&v.ID, &v.Type, &v.Username, &v.Action, &v.Module, &v.Detail, &v.IP, &v.UserAgent, &v.Status, &v.HTTPStatus, &v.CostMs, &created); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		if err != nil {
			return v, err
		}
		v.CreatedAt = t
		return v, nil
	},
}

var userCodec = entityCodec[domain.User]{
	table:   "users",
	columns: []string{"id", "username", "password_hash", "created_at"},
	values: func(v domain.User) []any {
		return []any{v.ID, v.Username, v.PasswordHash, timeValue(v.CreatedAt)}
	},
	scan: func(s rowScanner) (domain.User, error) {
		var v domain.User
		var created string
		if err := s.Scan(&v.ID, &v.Username, &v.PasswordHash, &created); err != nil {
			return v, err
		}
		t, err := parseTime(created)
		v.CreatedAt = t
		return v, err
	},
}

var backupConfigCodec = entityCodec[domain.BackupConfig]{
	table:   "backup_config",
	columns: []string{"id", "backup_pool", "enabled", "schedule", "last_run_at"},
	values: func(v domain.BackupConfig) []any {
		return []any{v.ID, v.BackupPool, v.Enabled, v.Schedule, timePtrValue(v.LastRunAt)}
	},
	scan: func(s rowScanner) (domain.BackupConfig, error) {
		var v domain.BackupConfig
		var lastRunAt sql.NullString
		if err := s.Scan(&v.ID, &v.BackupPool, &v.Enabled, &v.Schedule, &lastRunAt); err != nil {
			return v, err
		}
		var err error
		v.LastRunAt, err = timePtr(lastRunAt)
		return v, err
	},
}

var systemSettingsCodec = entityCodec[domain.SystemSettings]{
	table:   "system_settings",
	columns: []string{"id", "import_dir", "client_iface", "allow_cross_subnet", "replication_rate_mbps", "data_pool"},
	values: func(v domain.SystemSettings) []any {
		var rate any
		if v.ReplicationRateMBPS != nil {
			rate = *v.ReplicationRateMBPS
		}
		return []any{v.ID, v.ImportDir, v.ClientIface, v.AllowCrossSubnet, rate, v.DataPool}
	},
	scan: func(s rowScanner) (domain.SystemSettings, error) {
		var v domain.SystemSettings
		var rate sql.NullInt64
		// data_pool 是可空列（迁移 0040 添加时无默认值），旧行读回 NULL；扫进普通 string 会报
		// "converting NULL to string is unsupported" 并让所有设置读取失败。与 replication_rate_mbps 一样把 NULL 当作 ""。
		var dataPool sql.NullString
		err := s.Scan(&v.ID, &v.ImportDir, &v.ClientIface, &v.AllowCrossSubnet, &rate, &dataPool)
		if rate.Valid {
			n := int(rate.Int64)
			v.ReplicationRateMBPS = &n
		}
		v.DataPool = dataPool.String
		return v, err
	},
}

var replicationStateCodec = entityCodec[domain.ReplicationState]{
	table:   "replication_state",
	columns: []string{"target", "kind", "root", "last_snapshot", "last_ok_at", "last_error", "updated_at"},
	values: func(v domain.ReplicationState) []any {
		return []any{v.Target, v.Kind, v.Root, v.LastSnapshot, timePtrValue(v.LastOKAt), v.LastError, timeValue(v.UpdatedAt)}
	},
	scan: func(s rowScanner) (domain.ReplicationState, error) {
		var v domain.ReplicationState
		var lastOK sql.NullString
		var updatedAt string
		if err := s.Scan(&v.Target, &v.Kind, &v.Root, &v.LastSnapshot, &lastOK, &v.LastError, &updatedAt); err != nil {
			return v, err
		}
		ok, err := timePtr(lastOK)
		if err != nil {
			return v, err
		}
		v.LastOKAt = ok
		t, err := parseTime(updatedAt)
		v.UpdatedAt = t
		return v, err
	},
}

var backupStateCodec = entityCodec[domain.BackupState]{
	table:   "backup_state",
	columns: []string{"id", "source_name", "last_snapshot", "updated_at"},
	values: func(v domain.BackupState) []any {
		return []any{v.ID, v.SourceName, v.LastSnapshot, timeValue(v.UpdatedAt)}
	},
	scan: func(s rowScanner) (domain.BackupState, error) {
		var v domain.BackupState
		var updatedAt string
		if err := s.Scan(&v.ID, &v.SourceName, &v.LastSnapshot, &updatedAt); err != nil {
			return v, err
		}
		t, err := parseTime(updatedAt)
		v.UpdatedAt = t
		return v, err
	},
}

func timeValue(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

func parseTime(v string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, v)
	if err != nil {
		return time.Time{}, err
	}
	return t.UTC(), nil
}

func timePtrValue(t *time.Time) any {
	if t == nil {
		return nil
	}
	return timeValue(*t)
}

func timePtr(v sql.NullString) (*time.Time, error) {
	if !v.Valid {
		return nil, nil
	}
	t, err := parseTime(v.String)
	return &t, err
}

func stringPtrValue(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}

func stringPtr(v sql.NullString) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func stringsValue(v []string) string {
	b, _ := json.Marshal(v)
	return string(b)
}

func parseStrings(v string) ([]string, error) {
	var out []string
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, err
	}
	return out, nil
}
