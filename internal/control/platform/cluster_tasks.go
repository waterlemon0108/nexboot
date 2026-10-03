package platform

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/tianwei/diskless/internal/control/assets"
	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// TaskSource 返回本节点自己的任务列表。
type TaskSource interface {
	GetTask(context.Context, string) (domain.Task, error)
	ListActiveTasks(context.Context) ([]domain.Task, error)
	ListTasks(context.Context, assets.TaskListQuery) (assets.TaskListResult, error)
}

// ClusterTasks 汇总各节点的任务列表。池写操作在池所在节点执行并记在那里，
// 不去问的话控制台只能显示「已提交」，看不到成败。
type ClusterTasks struct {
	Store   store.Store
	Local   TaskSource
	NodeID  string
	Token   string
	Timeout time.Duration
	// Fetch 读取一个远端节点的池任务；nil 时用集群 HTTP 通道。
	Fetch func(ctx context.Context, apiURL string) ([]domain.Task, error)
}

func (c ClusterTasks) GetTask(ctx context.Context, id string) (domain.Task, error) {
	task, err := c.Local.GetTask(ctx, id)
	if !errors.Is(err, store.ErrNotFound) {
		if err == nil {
			task = c.named(ctx, task)
		}
		return task, err
	}
	remote, _ := c.remote(ctx)
	for _, t := range remote {
		if t.ID == id {
			return c.named(ctx, t), nil
		}
	}
	return task, err
}

func (c ClusterTasks) named(ctx context.Context, t domain.Task) domain.Task {
	items := []domain.Task{t}
	c.nameTargets(ctx, items)
	return items[0]
}

// nameTargets 从目录填 TargetName，让任务显示「Win11 电竞版 / default」而非中文名被折叠后的数据集 ID。
// 只查本页涉及的引用（列表每几秒刷新一次）；未知引用（池、已删对象）不填。
func (c ClusterTasks) nameTargets(ctx context.Context, tasks []domain.Task) {
	if c.Store == nil || len(tasks) == 0 {
		return
	}
	names := map[string]string{}
	for i := range tasks {
		ref := tasks[i].TargetRef
		if ref == "" {
			continue
		}
		name, seen := names[ref]
		if !seen {
			name = c.targetName(ctx, ref)
			names[ref] = name
		}
		tasks[i].TargetName = name
	}
}

func (c ClusterTasks) targetName(ctx context.Context, ref string) string {
	st := c.Store
	if img, err := st.Images().Get(ctx, ref); err == nil {
		return img.Name
	}
	configName := func(cfg domain.Config) string {
		if img, err := st.Images().Get(ctx, cfg.ImageID); err == nil {
			return img.Name + " / " + cfg.Name
		}
		return cfg.Name
	}
	if cfg, err := st.Configs().Get(ctx, ref); err == nil {
		return configName(cfg)
	}
	if r, err := st.Reductions().Get(ctx, ref); err == nil {
		label := r.DisplayName
		if label == "" {
			label = r.Name
		}
		if cfg, err := st.Configs().Get(ctx, r.ConfigID); err == nil {
			return configName(cfg) + " / " + label
		}
		return label
	}
	// 超管保存和数据盘发布以 MAC 指代机器。
	for _, id := range []string{ref, "terminal-" + ref} {
		if t, err := st.Terminals().Get(ctx, id); err == nil {
			return t.Name
		}
	}
	if g, err := st.Groups().Get(ctx, ref); err == nil {
		return g.Name
	}
	return ""
}

func (c ClusterTasks) ListActiveTasks(ctx context.Context) ([]domain.Task, error) {
	items, err := c.Local.ListActiveTasks(ctx)
	if err != nil {
		return nil, err
	}
	remote, _ := c.remote(ctx)
	for _, t := range remote {
		if t.Status == domain.TaskStatusPending || t.Status == domain.TaskStatusRunning {
			items = append(items, t)
		}
	}
	c.nameTargets(ctx, items)
	return items, nil
}

func (c ClusterTasks) ListTasks(ctx context.Context, q assets.TaskListQuery) (assets.TaskListResult, error) {
	remote, unreachable := c.remote(ctx)
	var extra []domain.Task
	for _, t := range remote {
		if q.Status == "" || string(t.Status) == q.Status {
			extra = append(extra, t)
		}
	}
	if len(extra) == 0 {
		res, err := c.Local.ListTasks(ctx, q)
		res.Unreachable = unreachable
		c.nameTargets(ctx, res.Items)
		return res, err
	}
	page, size := q.Page, q.Size
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	local, total, err := c.Store.Tasks().ListPaged(ctx, q.Status, page*size, 0)
	if err != nil {
		return assets.TaskListResult{}, err
	}
	merged := append(local, extra...)
	sort.SliceStable(merged, func(i, j int) bool { return merged[i].CreatedAt.After(merged[j].CreatedAt) })
	from, to := (page-1)*size, page*size
	if from > len(merged) {
		from = len(merged)
	}
	if to > len(merged) {
		to = len(merged)
	}
	items := append([]domain.Task{}, merged[from:to]...)
	c.nameTargets(ctx, items)
	return assets.TaskListResult{Items: items, Total: total + len(extra),
		Page: page, Size: size, Unreachable: unreachable}, nil
}

// remote 收集其它节点执行、本节点没有记录的池任务，标注执行节点，并返回未应答的地址。
func (c ClusterTasks) remote(ctx context.Context) ([]domain.Task, []string) {
	servers, err := c.Store.Servers().List(ctx)
	if err != nil {
		return nil, nil
	}
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	fetch := c.Fetch
	if fetch == nil {
		fetch = func(ctx context.Context, apiURL string) ([]domain.Task, error) {
			return fetchNodeTasks(ctx, apiURL, c.Token)
		}
	}
	var (
		mu          sync.Mutex
		wg          sync.WaitGroup
		out         []domain.Task
		unreachable []string
	)
	for _, srv := range servers {
		if srv.ID == c.NodeID || srv.APIURL == "" {
			continue
		}
		wg.Add(1)
		go func(srv domain.Server) {
			defer wg.Done()
			nctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			tasks, err := fetch(nctx, srv.APIURL)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				unreachable = append(unreachable, srv.IP)
				return
			}
			for _, t := range tasks {
				if t.Node == "" {
					t.Node = srv.IP // 尚未记录节点的旧任务：按来源标注
				}
				out = append(out, t)
			}
		}(srv)
	}
	wg.Wait()
	sort.Strings(unreachable)
	// 备机库里有它当写入者时记下的任务，本机早已有，只留本机没有的
	foreign := out[:0]
	seen := map[string]bool{}
	for _, t := range out {
		if seen[t.ID] {
			continue
		}
		seen[t.ID] = true
		if _, err := c.Store.Tasks().Get(ctx, t.ID); errors.Is(err, store.ErrNotFound) {
			foreign = append(foreign, t)
		}
	}
	return foreign, unreachable
}

func fetchNodeTasks(ctx context.Context, apiURL, token string) ([]domain.Task, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL+"/internal/node/tasks", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &nodeError{status: resp.Status}
	}
	var body struct {
		Items []domain.Task `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	return body.Items, nil
}

// NodePoolTasks 是本节点交给写入者的内容：它执行过的池任务，最新在前。其余任务写入者库里本来就有。
func NodePoolTasks(ctx context.Context, st store.Store) ([]domain.Task, error) {
	recent, _, err := st.Tasks().ListPaged(ctx, "", 200, 0)
	if err != nil {
		return nil, err
	}
	out := []domain.Task{}
	for _, t := range recent {
		if t.Type.RunsOnPoolNode() && len(out) < 100 {
			out = append(out, t)
		}
	}
	return out, nil
}
