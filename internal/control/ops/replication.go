package ops

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/ha"
	"github.com/tianwei/diskless/internal/storage"
	"github.com/tianwei/diskless/internal/storage/zfs"
	"github.com/tianwei/diskless/internal/store"
)

// 复制器让备机目录跟随写入节点。角色由 Gate 决定而非配置：闸门开的节点打轮次标记，
// 闸门关的节点拉取，激活翻转闸门后两个循环自动换岗。
// 写入侧不保存同步状态，只打递归快照并经 HTTP 供流（ha.ReplicationHandler），由备机驱动同步。
// 增量走 `zfs send -R -I`；promote（合并、超管保存）会让增量收不进来，此时备机自行重建。

// ReplicationZFS 是复制器用到的那部分 ZFS 客户端。
type ReplicationZFS interface {
	ListGUIDs(ctx context.Context, root string) (zfs.GUIDInventory, error)
	SnapshotRecursive(ctx context.Context, root, name string) error
	PruneSnapshotsExcept(ctx context.Context, root, prefix string, keep int, pinned []string) error
	ReceiveReplication(ctx context.Context, root string, r io.Reader) error
	// ReceiveDataset 在追平轮里接收单个数据集的流。
	ReceiveDataset(ctx context.Context, dataset string, r io.Reader) error
	Destroy(ctx context.Context, dataset string) error
	// Rename 把数据集子树挪到一边，用来保住本机自己写的那份而不是销毁它。
	Rename(ctx context.Context, source, dataset string) error
	// DetachMounts 释放挪开子树的显式挂载点。改名不会带走显式 mountpoint（<pool>/nd/db 有一个），
	// 不释放的话旧副本会继续占着 /<pool>/nd/db，遮住新收的 nd/db：数据库副本写进孤儿，
	// 真正复制的数据集冻结，日后接任的备机拿到的是过期目录。
	DetachMounts(ctx context.Context, dataset string) error
	// ListAsideCopies 列出之前各轮挪开的 "-rebuilding-"/"-diverged-" 兄弟数据集，最新的在最后。
	ListAsideCopies(ctx context.Context, root string) ([]string, error)
	// ResumeToken 返回 root 下持有半截接收流的数据集及其续传令牌。
	ResumeToken(ctx context.Context, root string) (dataset, token string, err error)
	// AbortResume 丢弃半截接收流，否则续不动的令牌会被永远重试。
	AbortResume(ctx context.Context, root string) error
	EnsureDBCopyDataset(ctx context.Context) (string, error)
}

// ReplicationPeer 是备机读取写入节点的接口。
type ReplicationPeer interface {
	Inventory(ctx context.Context) (ha.ReplicationInventory, error)
	Stream(ctx context.Context, from, to string) (io.ReadCloser, error)
	StreamResume(ctx context.Context, token string) (io.ReadCloser, error)
	// StreamDataset 为追平轮发送单个数据集的流（见 catchup.go）。
	StreamDataset(ctx context.Context, rel, from, origin, to string) (io.ReadCloser, error)
	// Confirm 告诉写入者本机已完整持有 marker。追平轮可能根本不发根数据集的流，
	// 写入者只有收到确认才记下这一轮。
	Confirm(ctx context.Context, marker string) error
}

// replicationMarkersKept 是写入节点保留的 rep-* 标记数；另外每台备机最后确认的标记也保留（见 pinnedMarkers）。
const replicationMarkersKept = 3

// markerPinWindow 是备机失联后其最后确认标记的保留时长。窗口内回来可以增量续上；
// 超过后不再为可能不回来的节点占空间，回来时整体重建。
const markerPinWindow = 24 * time.Hour

type Replicator struct {
	Store store.Store
	ZFS   ReplicationZFS
	Gate  *ha.Gate
	// Root 是本机目录容器 "<pool>/nd"。
	Root string
	// Peer 和 PeerURL 指向对端；Peer 为 nil 时不拉取。
	Peer    ReplicationPeer
	PeerURL string
	// LiveDBPath 是本机实际服务的数据库，只用来察觉它是否变化（见 dbStamp）。
	LiveDBPath string
	// SelfURL 是本机 API 地址，用来认出角色互换后残留的、指向自己的状态行。
	SelfURL string
	// DBRevision 是目录写入计数（store.Revision），用来判断数据库是否变化。
	// 不能看文件：健康探测每两秒写入再回滚，回滚也会改动 -wal，空闲集群会每分钟白打一轮。
	// 为 nil 时退回文件戳，供没有计数器的单机使用。
	DBRevision func() uint64
	// DBSnapshot 把在用数据库写到指定路径（store.Snapshot），在打标记前执行，使库行与数据集落在同一个 txg。
	DBSnapshot func(ctx context.Context, path string) error
	Now        func() time.Time
	Logger     *slog.Logger

	lastFingerprint string
	// markMu 串行化打标记：周期循环和计划切换的排空都会打，
	// 若有一轮插在排空取样与降级之间，就没有备机能收到它。
	markMu sync.Mutex
	// holdUntil 让周期循环在计划切换排空期间暂停打标记；带时限，切换没完成也不会永久停掉复制。
	holdUntil time.Time
	// pulling 保证同时只有一次拉取（周期拉取与接任前追平写同一份目录），
	// 也保护 PullFrom 临时替换的 Peer/PeerURL。用 channel 而非 mutex，等待才能放弃。
	pulling     chan struct{}
	pullingOnce sync.Once
	// HoldsVIP 报告 VIP 是否在本机。配置的对端就是 VIP，持有 VIP 的备机会去拉自己，连接中途断掉后循环不再运行。
	HoldsVIP func() bool
	// InventoryTimeout 限制单次目录清单请求；零值用默认值。
	InventoryTimeout time.Duration
}

// defaultInventoryTimeout 远长于健康对端列目录所需，又远短于一次挂起请求会拖掉的一轮。
const defaultInventoryTimeout = 30 * time.Second

// acquirePull 占用拉取槽位，ctx 先结束则放弃。
func (r *Replicator) acquirePull(ctx context.Context) (func(), error) {
	r.pullingOnce.Do(func() { r.pulling = make(chan struct{}, 1) })
	select {
	case r.pulling <- struct{}{}:
		return func() { <-r.pulling }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("另一次目录拉取还没结束：%w", ctx.Err())
	}
}

func (r *Replicator) inventory(ctx context.Context) (ha.ReplicationInventory, error) {
	timeout := r.InventoryTimeout
	if timeout <= 0 {
		timeout = defaultInventoryTimeout
	}
	ictx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	inv, err := r.Peer.Inventory(ictx)
	if err != nil && ictx.Err() != nil && ctx.Err() == nil {
		return inv, fmt.Errorf("对端 %s 在 %s 内没有回应目录清单：%w", r.PeerURL, timeout, err)
	}
	return inv, err
}

// MarkOnce 在目录自上次以来有变化时打一轮复制标记：先写数据库副本，再打递归快照，最后裁旧标记。
// 返回是否打了一轮。闸门关闭（备机）时从不打，因为打标记会改动池。
func (r *Replicator) MarkOnce(ctx context.Context) (bool, error) {
	r.markMu.Lock()
	defer r.markMu.Unlock()
	if r.Now().Before(r.holdUntil) {
		return false, nil // 计划切换正在排空
	}
	name, err := r.mark(ctx, false)
	return name != "", err
}

// MarkForSwitch 不论有无变化都打一轮，暂停周期循环 hold 时长，并返回该轮标记；
// 计划切换要等每台备机追到这一轮才降级。
func (r *Replicator) MarkForSwitch(ctx context.Context, hold time.Duration) (string, error) {
	r.markMu.Lock()
	defer r.markMu.Unlock()
	r.holdUntil = r.Now().Add(hold)
	return r.mark(ctx, true)
}

// ResumeMarking 在切换放弃后让周期循环恢复打标记。
func (r *Replicator) ResumeMarking() {
	r.markMu.Lock()
	defer r.markMu.Unlock()
	r.holdUntil = time.Time{}
}

// mark 打一轮并返回标记；无变化且非 force 时返回 ""。调用方须持有 markMu。
func (r *Replicator) mark(ctx context.Context, force bool) (string, error) {
	// force 来自正在退位的写入者（计划切换或交接），它的闸门可能已关，仍须打出最后一轮。
	if !force && r.Gate != nil && r.Gate.Allow() != nil {
		return "", nil
	}
	inv, err := r.ZFS.ListGUIDs(ctx, r.Root)
	if err != nil {
		return "", err
	}
	// 指纹必须含数据库：分组、客户机、用户、设置只在 SQLite 里，只看数据集的话这些改动
	// 要等下次导入镜像才会复制出去，故障切换会把它们回滚。
	fingerprint := catalogueFingerprint(r.Root, inv) + "|db:" + r.dbFingerprint()
	if fingerprint == r.lastFingerprint && !force {
		return "", nil
	}
	dbDir, err := r.ZFS.EnsureDBCopyDataset(ctx)
	if err != nil {
		return "", err
	}
	// 数据库副本和快照构成同一轮：两者之间不能落下目录改动，也不能有改动做到一半。
	name := fmt.Sprintf("%s%d", storage.ReplicationSnapshotPrefix, r.Now().UTC().UnixNano())
	if err := func() error {
		defer storage.HoldCatalogue()()
		if err := r.DBSnapshot(ctx, filepath.Join(dbDir, "ndiskless.db")); err != nil {
			return err
		}
		return r.ZFS.SnapshotRecursive(ctx, r.Root, name)
	}(); err != nil {
		return "", err
	}
	// 裁旧标记只是清理，失败不算本轮失败：常见原因是备机正用 `zfs send` 读那个快照，
	// destroy 报 "dataset is busy"。若当作致命错误，计划切换的排空轮会失败并被拒绝。
	// 这轮没删掉的下一轮再删。
	if err := r.ZFS.PruneSnapshotsExcept(ctx, r.Root, storage.ReplicationSnapshotPrefix, replicationMarkersKept, r.pinnedMarkers(ctx)); err != nil {
		r.logf("旧的复制标记这轮没删掉（多半是备机正在读它），下一轮再试", "error", err)
	}
	r.lastFingerprint = fingerprint
	return name, nil
}

// pinnedMarkers 返回 markerPinWindow 内见过的每台备机最后确认的标记。
// 裁掉它，备机回来时就与本机没有共同快照，重启几分钟也要整体重建。
func (r *Replicator) pinnedMarkers(ctx context.Context) []string {
	if r.Store == nil {
		return nil
	}
	rows, err := r.Store.ReplicationStates().List(ctx)
	if err != nil {
		return nil
	}
	cutoff := r.Now().Add(-markerPinWindow)
	var out []string
	for _, row := range rows {
		if row.Kind == "primary-serve" && row.LastSnapshot != "" && row.LastOKAt != nil && row.LastOKAt.After(cutoff) {
			out = append(out, row.LastSnapshot)
		}
	}
	return out
}

// dbFingerprint 优先用写入计数，没有时才退回文件戳。
func (r *Replicator) dbFingerprint() string {
	if r.DBRevision != nil {
		return fmt.Sprintf("rev:%d", r.DBRevision())
	}
	return dbStamp(r.LiveDBPath)
}

// dbStamp 用主文件和 -wal 的大小与 mtime 粗判数据库是否变化；WAL 模式下提交落在 -wal，只看主文件会漏掉。
// 空路径返回常量，永不触发新一轮。
func dbStamp(path string) string {
	if path == "" {
		return ""
	}
	var parts []string
	for _, p := range []string{path, path + "-wal"} {
		if fi, err := os.Stat(p); err == nil {
			parts = append(parts, fmt.Sprintf("%d@%d", fi.Size(), fi.ModTime().UnixNano()))
		}
	}
	return strings.Join(parts, ",")
}

// catalogueFingerprint 对目录内容取哈希，排除记账快照，否则每次打标记都会改变指纹、无限触发下一轮。
func catalogueFingerprint(root string, inv zfs.GUIDInventory) string {
	rows := make([]string, 0, len(inv.Entries))
	for _, e := range inv.Entries {
		if _, name, ok := strings.Cut(e.Name, "@"); ok && storage.SystemSnapshot(name) {
			continue
		}
		rows = append(rows, e.Name+"\x00"+e.GUID)
	}
	sort.Strings(rows)
	sum := sha256.Sum256([]byte(strings.Join(rows, "\n")))
	return fmt.Sprintf("%x", sum[:8])
}

// PullOnce 让备机向写入节点追一轮：有中断的流先续传，再从最新共同快照增量拉取；
// 增量收不进或没有共同快照时整体重建。闸门开（写入节点）时从不拉取，因为拉取会回滚本机状态。
func (r *Replicator) PullOnce(ctx context.Context) error {
	release, err := r.acquirePull(ctx)
	if err != nil {
		return err
	}
	defer release()
	if r.Peer == nil || (r.Gate != nil && r.Gate.Allow() == nil) {
		return nil
	}
	if r.HoldsVIP != nil && r.HoldsVIP() {
		return nil // VIP 在本机：在它移走或本机接任之前没有可跟随的对象
	}
	if err := r.pull(ctx); err != nil {
		r.recordState(ctx, "", err)
		return err
	}
	return nil
}

// PullFrom 从 url 指定的节点而非配置的对端拉一轮。配置的对端是 VIP，
// 备机持有 VIP 准备接任时问 VIP 就是问自己，追不上仍存活的写入者。
func (r *Replicator) PullFrom(ctx context.Context, peer ReplicationPeer, url string) error {
	release, err := r.acquirePull(ctx)
	if err != nil {
		return err
	}
	defer release()
	if peer == nil || (r.Gate != nil && r.Gate.Allow() == nil) {
		return nil
	}
	configured, configuredURL := r.Peer, r.PeerURL
	r.Peer, r.PeerURL = peer, url
	defer func() { r.Peer, r.PeerURL = configured, configuredURL }()
	return r.pull(ctx)
}

func (r *Replicator) pull(ctx context.Context) error {
	// 重建残留在这里统一回收，被重启打断的重建也由之后的轮次收掉。同步到目标后都已没用；
	// 没同步成时留最新一份供人工恢复。
	synced := false
	defer func() {
		keep := asideCopiesKept
		if synced {
			keep = 0
		}
		r.pruneAsideCopies(ctx, keep)
	}()
	// forceRebuild：半截状态废不掉，副本无法再增量，拿到对端目标标记后整体重建。
	forceRebuild := false
	if dataset, token, err := r.ZFS.ResumeToken(ctx, r.Root); err == nil && token != "" {
		if err := r.resume(ctx, dataset, token); err != nil {
			// 续不动就废掉半截状态，本轮继续走增量或全量；直接返回会让下一轮拿同一令牌重试，永远卡住。
			r.logf("resume token is unusable; discarding it and pulling afresh",
				"error", err)
			if abortErr := r.ZFS.AbortResume(ctx, dataset); abortErr != nil {
				// `zfs recv -A` 也可能失败（"dataset already exists"）。那就挪开重建：
				// 半截状态和令牌跟着旧名字走，全量流才收得进来。
				r.logf("could not discard the resume token; rebuilding the copy whole",
					"error", abortErr, "resume_error", err)
				forceRebuild = true
			}
		}
	}

	peerInv, err := r.inventory(ctx)
	if err != nil {
		return err
	}
	target := latestMarker(peerInv.Root, zfs.GUIDInventory{Entries: peerInv.Entries})
	if target == "" {
		return nil // 对端还没打过任何一轮
	}
	local, err := r.ZFS.ListGUIDs(ctx, r.Root)
	if err != nil {
		if !zfs.IsNotExist(err) {
			return err
		}
		// 全新备机还没有 nd/，全量流会创建它。
		local = zfs.GUIDInventory{}
	}
	// 不跟随不自洽的写入者：库里记着、池里却没有的数据集，recv -F 会把它从本机也删掉。
	// 判据是写入者自洽与否而非「会不会删本机的东西」，合法删除会连库行一起删（见 ImageService.DeleteImage）。
	if len(peerInv.Incomplete) > 0 {
		return fmt.Errorf("对端 %s 的目录与它自己的数据库对不上（库里还记着 %s，池里已经没有），"+
			"跟随它会把这些数据集从本机一并删掉：本轮不复制。"+
			"请先在对端处理这处不一致（这些是已经没有数据的残留记录，到「镜像管理」里删掉即可），"+
			"或把目录完整的那台切换为主机", r.PeerURL, strings.Join(peerInv.Incomplete, "、"))
	}
	// 空对端永不覆盖非空副本：刚装好就抢到 VRRP 的空节点会让一次 recv -F 删光全部数据。
	// 对端变空也可能是合法删光了镜像，这里分不清，所以报错里两种都说；主机再导入镜像后下一轮自愈。
	if catalogueEmpty(peerInv.Entries, peerInv.Root) && !catalogueEmpty(local.Entries, r.Root) {
		return fmt.Errorf("对端 %s 的目录是空的，而本机存有镜像与配置：拒绝用空目录覆盖本机数据。"+
			"两种可能：其一，主机上的镜像刚被全部删除——这是正常的，等主机上再导入"+
			"任何一个镜像，本机会自动跟上并清掉本地残留，无需处理；其二，新装的空节点"+
			"抢到了主机身份——查一下主机那台有没有镜像，若确实是空节点抢主，"+
			"在正确的主机上执行计划切换后再让本机复制", r.PeerURL)
	}
	if r.dropStaleRecvTemps(ctx, peerInv, local) {
		if local, err = r.ZFS.ListGUIDs(ctx, r.Root); err != nil {
			return err
		}
	}
	// 放在两道护栏之后：废不掉令牌而整体重建，同样不能跟随异常或空的写入者。
	if forceRebuild {
		if err := r.rebuildWhole(ctx, target); err != nil {
			return err
		}
		synced = true
		r.recordState(ctx, target, nil)
		return nil
	}
	sender := zfs.GUIDInventory{Entries: peerInv.Entries}
	base := sender.LatestCommonSnapshot(local, peerInv.Root, r.Root)
	if base == target {
		// 最新标记相同还不算同步：中断的 -R 流会先落下根的标记（它在流的最前面），
		// 子数据集可能缺失或停在旧内容，而写入者没有新改动时以后也不会再修。
		// 同步要求对端每个数据集本机都有，且对端打了标记的数据集本机也带着该标记。
		behind := datasetsWithoutMarker(peerInv, local, r.Root, target)
		if datasetsComplete(peerInv, local, r.Root) && len(behind) == 0 {
			synced = true
			r.recordState(ctx, target, nil) // 已同步，只刷新心跳
			return nil
		}
		err := r.catchUp(ctx, peerInv, target, target)
		if err == nil {
			r.logf("copy held the newest marker but not all of the round; caught up dataset by dataset", "marker", target, "behind", strings.Join(behind, ","))
			r.confirm(ctx, target)
			synced = true
			r.recordState(ctx, target, nil)
			return nil
		}
		r.logf("catching up under the newest marker failed", "marker", target, "error", err.Error())
		var divergence error
		if divergence, err = r.rebuildAfterCatchUp(ctx, err, target); err != nil {
			return err
		}
		synced = true
		r.recordState(ctx, target, divergence)
		return nil
	}
	// 没有共同快照说明两条谱系已分叉，只能全量接收；ZFS 不允许全量流落在已有快照的数据集上，
	// 所以必须先腾空本机名字，否则复制永久卡住、备机拿不到数据库副本而无法激活。
	// 上面的空对端检查已保证不会用空目录覆盖非空副本。
	// divergence 记录本轮挪走过本机自写的目录，要留到末尾那次 recordState，否则会被 recordState(nil) 清掉。
	var divergence error
	if base == "" && len(local.Entries) > 0 {
		// 区分两种分叉：本机只是落后或被重建过，整体重建即可；本机在分区期间自己当过写入者，
		// 重建会无声丢掉它那段改动。纯副本从不自己打快照（ndbackup-*、rep-* 都由写入节点打，
		// 客户机克隆在 run/ 下），所以本机有、对端没有的快照只能是本机写的。
		if own := divergentSnapshots(local, peerInv.Entries); len(own) > 0 {
			// 挪开保留，删除留给人确认（见 preserveAside）。
			aside, err := r.preserveAside(ctx, "lineages have diverged; preserving the local copy instead of destroying it", own)
			if err != nil {
				return err
			}
			divergence = fmt.Errorf(
				"两侧谱系已分叉：本机在失联期间自己当过写入者（有 %d 个对端没有的快照，例如 %s）。"+
					"本机那份已改名保留为 %s，复制已从对端重建。"+
					"请确认里面有没有要保留的改动，确认完再删除它以释放空间",
				len(own), own[0], aside)
		} else {
			r.logf("no snapshot in common with the peer; rebuilding the copy from scratch", "target", target)
			if err := r.rebuildWhole(ctx, target); err != nil {
				return err
			}
			synced = true
			r.recordState(ctx, target, nil)
			return nil
		}
	}
	// 有共同基准走增量，但 recv -F 会回滚本机多出的轮次。回滚轮次标记无妨；
	// 若其中有本机写的还原点或镜像快照，就不回滚，挪开本机那份再整体重建。
	if own := divergentSnapshots(local, peerInv.Entries); len(own) > 0 && base != "" {
		if content := writtenHere(own, local, r.Root, base); len(content) > 0 {
			aside, err := r.preserveAside(ctx, "an incremental would roll back restore points; preserving the local copy", content)
			if err != nil {
				return err
			}
			divergence = rollbackDivergence(content, aside)
			base = "" // 本机那份已挪走，只能整体接收
		} else {
			r.logf("local-only snapshots are the peer's own deletions and round markers; rolling them back",
				"local_only", len(own), "example", own[0], "base", base, "target", target)
		}
	}
	if err := r.receiveRound(ctx, base, target); err != nil {
		if base == "" {
			return err
		}
		// 增量收不进来，通常是超管保存或合并换掉了同名数据集：先逐个数据集追平，整体重建是最后手段。
		if cerr := r.catchUp(ctx, peerInv, base, target); cerr == nil {
			r.logf("incremental round not receivable; caught up dataset by dataset", "base", base, "target", target, "error", err)
			r.confirm(ctx, target)
		} else {
			r.logf("incremental replication not receivable and catching up failed", "base", base, "error", err, "catch_up", cerr.Error())
			if divergence, err = r.rebuildAfterCatchUp(ctx, cerr, target); err != nil {
				return err
			}
		}
	}
	synced = true
	r.recordState(ctx, target, divergence)
	return nil
}

// rollbackDivergence 是「跟随会回滚本机自写内容、已挪开保留」的告警。
func rollbackDivergence(own []string, aside string) error {
	return fmt.Errorf(
		"本机有 %d 个对端没有的还原点或镜像快照（例如 %s），跟随对端的增量会把它们回滚掉。"+
			"本机那份已改名保留为 %s，复制已从对端重建。"+
			"请确认里面有没有要保留的内容，确认完再删除它以释放空间",
		len(own), own[0], aside)
}

// rebuildAfterCatchUp 在追平失败后整体重建，返回要挂出的分叉告警。追平因本机自写的还原点被拒时
// 不能走 rebuildWhole：它成功后会删掉挪开的副本，那些还原点就静默没了，所以改为保留成 -diverged-。
func (r *Replicator) rebuildAfterCatchUp(ctx context.Context, cerr error, target string) (divergence error, err error) {
	// 传输中断下一轮换个目标重试即可；升级成整体重建会丢掉已经追平的进度。
	if errors.Is(cerr, errCatchUpInterrupted) {
		return nil, cerr
	}
	var diverged catchUpDivergedError
	if !errors.As(cerr, &diverged) {
		return nil, r.rebuildWhole(ctx, target)
	}
	aside, err := r.preserveAside(ctx, "catching up would roll back restore points written here; preserving the local copy", diverged.own)
	if err != nil {
		return nil, err
	}
	if err := r.receiveRound(ctx, "", target); err != nil {
		return nil, err
	}
	return rollbackDivergence(diverged.own, aside), nil
}

// recvTempName 是 libzfs 接收 -R 流时给同名冲突数据集起的临时名（recv-<pid>-<seq>）。
var recvTempName = regexp.MustCompile(`^recv-\d+-\d+$`)

// dropStaleRecvTemps 删掉被打断的接收留下的临时数据集，返回是否删过。它会挂在别的数据集的挂载点上，
// 让之后每轮增量和重建改名都报 busy。拉取是单飞的，轮开始时还在的只能是残留；写入者有同名数据集的不动。
func (r *Replicator) dropStaleRecvTemps(ctx context.Context, peerInv ha.ReplicationInventory, local zfs.GUIDInventory) bool {
	peerHas := map[string]bool{}
	for _, e := range peerInv.Entries {
		peerHas[strings.TrimPrefix(e.Name, peerInv.Root)] = true
	}
	dropped := false
	for _, e := range local.Entries {
		if strings.Contains(e.Name, "@") || !recvTempName.MatchString(filepath.Base(e.Name)) ||
			peerHas[strings.TrimPrefix(e.Name, r.Root)] {
			continue
		}
		_ = r.ZFS.DetachMounts(ctx, e.Name)
		if err := r.ZFS.Destroy(ctx, e.Name); err != nil {
			r.logf("could not remove a temporary dataset left by an interrupted receive", "dataset", e.Name, "error", err)
			continue
		}
		r.logf("removed a temporary dataset left by an interrupted receive", "dataset", e.Name)
		dropped = true
	}
	return dropped
}

// asideCopiesKept 是没同步成时保留的重建残留副本数。每次失败的重建都会留下一份，不回收会把池写满，
// 池满又让下一次接收必然失败。留最新一份足够人工恢复。
const asideCopiesKept = 1

// rebuildWhole 用对端的全量流替换本机副本。
func (r *Replicator) rebuildWhole(ctx context.Context, target string) error {
	// 空目录无可保留，挪开只会每轮多一个空壳，直接原地接收。
	if inv, err := r.ZFS.ListGUIDs(ctx, r.Root); err == nil && catalogueEmpty(inv.Entries, r.Root) {
		if len(inv.Entries) > 0 {
			if drop := r.ZFS.Destroy(ctx, r.Root); drop != nil {
				return fmt.Errorf("清空本机的空目录以便重收失败: %w", drop)
			}
		}
		return r.receiveRound(ctx, "", target)
	}
	return r.rebuildPreserving(ctx, target)
}

// pruneAsideCopies 回收较旧的重建残留副本。尽力而为：删不掉只占空间，不影响正确性。
func (r *Replicator) pruneAsideCopies(ctx context.Context, keep int) {
	all, err := r.ZFS.ListAsideCopies(ctx, r.Root)
	if err != nil {
		return
	}
	// 只回收重建残留；-diverged- 装着别处没有的内容，只能由人确认后删。
	aside := make([]string, 0, len(all))
	for _, name := range all {
		if !strings.Contains(name, r.Root+"-diverged-") {
			aside = append(aside, name)
		}
	}
	if len(aside) <= keep {
		return
	}
	for _, old := range aside[:len(aside)-keep] {
		if err := r.ZFS.Destroy(ctx, old); err != nil {
			r.logf("旧的挪开副本没能回收（占着空间，不影响正确性）", "aside", old, "error", err)
			continue
		}
		r.logf("回收了上一轮留下的挪开副本", "aside", old)
	}
}

// rebuildPreserving 先挪开旧副本、接收、成功后才删旧副本，失败则改回原名。
// 不能先 destroy 再接收：两步之间被打断（对端宕机、断电、keepalived 恰好提升本机）就会丢掉整个目录，
// 而写入者会把它持有的空目录复制出去。
func (r *Replicator) rebuildPreserving(ctx context.Context, target string) error {
	aside := fmt.Sprintf("%s-rebuilding-%d", r.Root, r.Now().UTC().UnixNano())
	if err := r.ZFS.Rename(ctx, r.Root, aside); err != nil {
		return err
	}
	// 接收前先释放挂载点，否则挪开的副本会继续遮住新收的数据集。
	if err := r.ZFS.DetachMounts(ctx, aside); err != nil {
		r.logf("moved-aside copy still holds its mountpoints", "aside", aside, "error", err)
	}
	if err := r.receiveRound(ctx, "", target); err != nil {
		// 失败的接收会把残片留在原名下（"Partially received snapshot is saved"），挡住改回原名。
		// 残片不能用，而本机随时可能被提升，所以删掉残片、放回完整副本，代价是放弃这次的续传令牌。
		if inv, lerr := r.ZFS.ListGUIDs(ctx, r.Root); lerr == nil && len(inv.Entries) > 0 {
			if drop := r.ZFS.Destroy(ctx, r.Root); drop != nil {
				return fmt.Errorf("重建失败，且收到一半的残片挡住了原名：本机目录暂存于 %s，"+
					"请人工删除 %s 后把它改回原名（原因：%w）", aside, r.Root, err)
			}
		}
		if back := r.ZFS.Rename(ctx, aside, r.Root); back != nil {
			// 改不回去，这份就此孤立；旧的由 pull 回收。
			return fmt.Errorf("重建失败且未能改回原名：本机目录暂存于 %s，请人工确认后改回 %s（原因：%w）",
				aside, r.Root, err)
		}
		return err
	}
	if err := r.ZFS.Destroy(ctx, aside); err != nil {
		// 重建已成功，残留只占空间；记日志免得无人察觉。
		r.logf("rebuilt copy is in place but the old one could not be removed",
			"leftover", aside, "error", err)
	}
	return nil
}

// resume 把半截流续收进持有它的数据集；没收到的数据集由随后的正常轮追平。
func (r *Replicator) resume(ctx context.Context, dataset, token string) error {
	stream, err := r.Peer.StreamResume(ctx, token)
	if err != nil {
		return err
	}
	err = r.ZFS.ReceiveReplication(ctx, dataset, stream)
	_ = stream.Close()
	return err
}

func (r *Replicator) receiveRound(ctx context.Context, base, target string) error {
	stream, err := r.Peer.Stream(ctx, base, target)
	if err != nil {
		return err
	}
	defer stream.Close()
	return r.ZFS.ReceiveReplication(ctx, r.Root, stream)
}

// catalogueEmpty 判断清单里是否没有任何产品资产，只有容器本身、它的快照和保留子数据集（db/、files/），
// 即新装节点的样子。
func catalogueEmpty(entries []zfs.GUIDEntry, root string) bool {
	for _, e := range entries {
		rel, ok := strings.CutPrefix(e.Name, root)
		if !ok || rel == "" || strings.Contains(rel, "@") {
			continue // 容器本身或它的快照
		}
		name := strings.TrimPrefix(rel, "/")
		if i := strings.IndexByte(name, '/'); i >= 0 {
			name = name[:i]
		}
		if storage.ReservedCatalogueChild(name) {
			continue
		}
		return false
	}
	return true
}

// datasetsWithoutMarker 列出对端带 marker、本机却没有该标记的数据集（相对名）。
func datasetsWithoutMarker(peerInv ha.ReplicationInventory, local zfs.GUIDInventory, localRoot, marker string) []string {
	have := map[string]bool{}
	for _, e := range local.Entries {
		if rel, ok := strings.CutPrefix(e.Name, localRoot); ok {
			have[rel] = true
		}
	}
	var behind []string
	for _, e := range peerInv.Entries {
		rel, ok := strings.CutPrefix(e.Name, peerInv.Root)
		if !ok {
			continue
		}
		if ds, snap, isSnap := strings.Cut(rel, "@"); isSnap && snap == marker && !have[rel] {
			behind = append(behind, ds)
		}
	}
	return behind
}

// confirm 告诉写入者本机已完整持有 target。尽力而为：失败只会让写入者推迟到下一次递归轮才记下。
func (r *Replicator) confirm(ctx context.Context, target string) {
	cctx, cancel := context.WithTimeout(ctx, defaultInventoryTimeout)
	defer cancel()
	if err := r.Peer.Confirm(cctx, target); err != nil {
		r.logf("could not confirm the round to the writer", "marker", target, "error", err.Error())
	}
}

// datasetsComplete 按容器相对名判断对端根下的每个数据集（不含快照）本机是否都有。
func datasetsComplete(peerInv ha.ReplicationInventory, local zfs.GUIDInventory, localRoot string) bool {
	have := map[string]bool{}
	for _, e := range local.Entries {
		if rel, ok := strings.CutPrefix(e.Name, localRoot); ok && !strings.Contains(rel, "@") {
			have[rel] = true
		}
	}
	for _, e := range peerInv.Entries {
		rel, ok := strings.CutPrefix(e.Name, peerInv.Root)
		if !ok || strings.Contains(rel, "@") {
			continue
		}
		if !have[rel] {
			return false
		}
	}
	return true
}

// latestMarker 返回容器本身最新的复制标记，即列表中最后一个（zfs 按创建顺序列出）。
func latestMarker(root string, inv zfs.GUIDInventory) string {
	prefix := root + "@" + storage.ReplicationSnapshotPrefix
	latest := ""
	for _, e := range inv.Entries {
		if strings.HasPrefix(e.Name, prefix) {
			latest = strings.TrimPrefix(e.Name, root+"@")
		}
	}
	return latest
}

// localOnlySnapshots 按 GUID 列出本机有而对端从未有过的快照。GUID 随 send/recv 传递，
// 对端不认识的 GUID 说明快照是本机创建的。
func localOnlySnapshots(local zfs.GUIDInventory, peerEntries []zfs.GUIDEntry) []string {
	known := make(map[string]bool, len(peerEntries))
	for _, e := range peerEntries {
		if e.GUID != "" {
			known[e.GUID] = true
		}
	}
	var out []string
	for _, e := range local.Entries {
		if !strings.Contains(e.Name, "@") || e.GUID == "" {
			continue // 数据集本身不算：它的内容由快照决定
		}
		if !known[e.GUID] {
			out = append(out, e.Name)
		}
	}
	sort.Strings(out)
	return out
}

// preserveAside 把本机目录改名挪开而不是销毁或回滚，返回新名字。挪开而不是停下：
// 停止复制的备机拿不到数据库副本就拒绝激活，而 keepalived 可能已把 VIP 给了它。
// 挪开的副本占着盘，直到运维确认后手动删除。
func (r *Replicator) preserveAside(ctx context.Context, why string, own []string) (string, error) {
	aside := fmt.Sprintf("%s-diverged-%d", r.Root, r.Now().UTC().UnixNano())
	r.logf(why, "local_only", len(own), "example", own[0], "preserved_as", aside)
	if err := r.ZFS.Rename(ctx, r.Root, aside); err != nil {
		return "", err
	}
	if err := r.ZFS.DetachMounts(ctx, aside); err != nil {
		r.logf("preserved copy still holds its mountpoints", "aside", aside, "error", err)
	}
	return aside, nil
}

// writtenHere 挑出共同基准之后本机自己打的快照，即回滚会永久丢失的那些。
// GUID 分不清「对端删了的还原点」和「本机写的还原点」，创建时间可以：早于基准的属于共同历史。
// 必须用 creation 而非 createtxg：接收端按落盘顺序分配 createtxg，根标记先于子快照落下，会把每次删除都判成本机写的。
// 系统标记一律不算。
func writtenHere(names []string, local zfs.GUIDInventory, root, base string) []string {
	made := make(map[string]int64, len(local.Entries))
	for _, e := range local.Entries {
		made[e.Name] = e.Created
	}
	baseMade := made[root+"@"+base]
	var out []string
	for _, full := range names {
		if _, snap, ok := strings.Cut(full, "@"); !ok || storage.SystemSnapshot(snap) {
			continue
		}
		if baseMade > 0 && made[full] <= baseMade {
			continue // 基准之前就有，是对端后来删的，跟着回滚即可
		}
		out = append(out, full)
	}
	return out
}

// divergentSnapshots 是 localOnlySnapshots 去掉对端只是裁掉的标记。写入节点只保留最近几个 rep-*，
// 落后的备机会持有对端已裁掉的标记；标记名含创建时间，早于对端最旧标记的只能是被裁的。
// 其余本机独有快照（较新的标记、用户或备份快照）才算真分叉。对端没有标记时无从比较，按保守口径全部算。
func divergentSnapshots(local zfs.GUIDInventory, peerEntries []zfs.GUIDEntry) []string {
	own := localOnlySnapshots(local, peerEntries)
	oldest, found := int64(0), false
	for _, e := range peerEntries {
		if _, name, ok := strings.Cut(e.Name, "@"); ok {
			if seq, isMarker := markerSeq(name); isMarker && (!found || seq < oldest) {
				oldest, found = seq, true
			}
		}
	}
	if !found {
		return own
	}
	out := own[:0:0]
	for _, name := range own {
		if _, snap, ok := strings.Cut(name, "@"); ok {
			if seq, isMarker := markerSeq(snap); isMarker && seq < oldest {
				continue // 对端裁掉的，不是本机写的
			}
		}
		out = append(out, name)
	}
	return out
}

func (r *Replicator) recordState(ctx context.Context, snapshot string, pullErr error) {
	if r.Store == nil {
		return
	}
	now := r.Now().UTC()
	row := domain.ReplicationState{
		Target: r.PeerURL, Kind: "standby-pull", Root: r.Root,
		LastSnapshot: snapshot, LastError: "", UpdatedAt: now,
	}
	if pullErr != nil {
		row.LastError = pullErr.Error()
		if rows, err := r.Store.ReplicationStates().List(ctx); err == nil {
			for _, prev := range rows {
				if prev.Target == r.PeerURL {
					row.LastSnapshot, row.LastOKAt = prev.LastSnapshot, prev.LastOKAt
				}
			}
		}
	} else {
		row.LastOKAt = &now
	}
	if err := r.Store.ReplicationStates().Upsert(ctx, row); err != nil {
		r.logf("replication state write failed", "error", err)
	}
}

func (r *Replicator) logf(msg string, args ...any) {
	if r.Logger != nil {
		r.Logger.Warn(msg, args...)
	}
}

// RunPrimary 在闸门开时周期打标记，RunStandby 在闸门关时周期拉取。两者随进程常驻，
// 由闸门决定哪个生效，激活时无需启停循环。
func (r *Replicator) RunPrimary(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if _, err := r.MarkOnce(ctx); err != nil {
				r.logf("replication mark failed", "error", err)
			}
		}
	}
}

// RunStandby 见 RunPrimary。
func (r *Replicator) RunStandby(ctx context.Context, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := r.PullOnce(ctx); err != nil {
				r.logf("replication pull failed", "error", err)
			}
		}
	}
}

// ReplicationStatus 供高可用卡片和复制滞后告警读取。
type ReplicationStatus struct {
	Targets []ReplicationTarget `json:"targets"`
}

type ReplicationTarget struct {
	Target       string     `json:"target"`
	Kind         string     `json:"kind"`
	Root         string     `json:"root"`
	LastSnapshot string     `json:"last_snapshot"`
	LastOKAt     *time.Time `json:"last_ok_at"`
	LagSeconds   int64      `json:"lag_seconds"`
	LastError    string     `json:"last_error"`
}

// fossilServeAfter 是 primary-serve 行多久没更新就视为残留而非滞后。活着的备机每个复制周期（默认 60s）都会拉取，
// 一小时没动静说明它已不再拉取。
const fossilServeAfter = time.Hour

// Status 读取复制状态行，按各目标最后一次成功的轮次算出滞后。
func (r *Replicator) Status(ctx context.Context) (ReplicationStatus, error) {
	rows, err := r.Store.ReplicationStates().List(ctx)
	if err != nil {
		return ReplicationStatus{}, err
	}
	now := r.Now().UTC()
	out := ReplicationStatus{Targets: []ReplicationTarget{}}
	for _, row := range rows {
		// 跳过旧角色安排留下的残留行（指向本机的、长期未更新的 primary-serve），
		// 否则会把它们报成滞后、误触发告警。
		if r.SelfURL != "" && row.Target == r.SelfURL {
			continue
		}
		// 服务中的节点不从任何人拉取：standby-pull 行来自它当备机时，目标是 VIP，SelfURL 匹配不上。
		if row.Kind == "standby-pull" && r.Gate != nil && r.Gate.Allow() == nil {
			continue
		}
		if row.Kind == "primary-serve" && row.LastOKAt != nil &&
			now.Sub(*row.LastOKAt) > fossilServeAfter {
			continue
		}
		tg := ReplicationTarget{
			Target: row.Target, Kind: row.Kind, Root: row.Root,
			LastSnapshot: row.LastSnapshot, LastOKAt: row.LastOKAt, LastError: row.LastError,
		}
		if row.LastOKAt != nil {
			tg.LagSeconds = int64(now.Sub(*row.LastOKAt).Seconds())
		}
		out.Targets = append(out.Targets, tg)
	}
	return out, nil
}

// CaughtUpTo 判断已拉到 got 的备机是否至少到达 target 这一轮。
// 用「至少」而非「相等」：取样后主机通常又打了新一轮，备机会直接拉到更新的标记。
// 标记是 "rep-<unix 纳秒>"，按数值比较；按字符串比较在位数变化时会出错。
func CaughtUpTo(got, target string) bool {
	if target == "" {
		return true // 从未打过标记，无需排空
	}
	g, ok := markerSeq(got)
	if !ok {
		return false
	}
	t, ok := markerSeq(target)
	if !ok {
		return false
	}
	return g >= t
}

func markerSeq(marker string) (int64, bool) {
	rest, ok := strings.CutPrefix(marker, "rep-")
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(rest, 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// LaggingPullers 列出计划切换还需等待的备机：alive 内刷新过 primary-serve 行、却没追到 latest 的每一台。
// 必须等全部而非最先完成的那台，因为 VRRP 按优先级把 VIP 交给谁本进程不知道。指向 self 的行是残留，忽略。
// 没有任何活着的拉取者时返回单个空名字，免得调用方把「没人」当成「都完成了」。
func LaggingPullers(rows []domain.ReplicationState, latest string, now time.Time, alive time.Duration, self string) []string {
	var lagging []string
	live := 0
	for _, row := range rows {
		if row.Kind != "primary-serve" || row.Target == "" || row.Target == self {
			continue
		}
		if row.LastOKAt == nil || now.Sub(*row.LastOKAt) > alive {
			continue
		}
		live++
		if !CaughtUpTo(row.LastSnapshot, latest) {
			lagging = append(lagging, row.Target)
		}
	}
	if live == 0 {
		return []string{""}
	}
	sort.Strings(lagging)
	return lagging
}
