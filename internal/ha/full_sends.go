package ha

import (
	"net/url"
	"sort"
	"sync"
)

// FullSends 记录哪些备机正在从本节点拉取整个目录。全量发送持续几分钟并占住根下所有数据集，
// 期间什么都删不掉；调用方据此如实说明，而不是归咎于客户机。增量只需几秒，不记录。
type FullSends struct {
	mu sync.Mutex
	by map[string]int
	// datasets：追平时逐个数据集发送，每次只占住该数据集；键为相对目录根的名字（"/img"）。
	datasets map[string]map[string]int
}

func hostOf(standby string) string {
	if u, err := url.Parse(standby); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return standby
}

// beginDataset 记录一台备机正在接收追平轮次中的一个数据集。
func (f *FullSends) beginDataset(standby, rel string) func() {
	if f == nil {
		return func() {}
	}
	host := hostOf(standby)
	f.mu.Lock()
	if f.datasets == nil {
		f.datasets = map[string]map[string]int{}
	}
	if f.datasets[rel] == nil {
		f.datasets[rel] = map[string]int{}
	}
	f.datasets[rel][host]++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		if f.datasets[rel][host]--; f.datasets[rel][host] <= 0 {
			delete(f.datasets[rel], host)
		}
		if len(f.datasets[rel]) == 0 {
			delete(f.datasets, rel)
		}
		f.mu.Unlock()
	}
}

// Sending 列出正在接收这些数据集（以目录直接子级命名，如镜像 ID）或整个目录的备机。
func (f *FullSends) Sending(names ...string) []string {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	hosts := map[string]bool{}
	for host := range f.by {
		hosts[host] = true
	}
	for _, name := range names {
		for host := range f.datasets["/"+name] {
			hosts[host] = true
		}
	}
	out := make([]string, 0, len(hosts))
	for host := range hosts {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}

func (f *FullSends) begin(standby string) func() {
	if f == nil {
		return func() {}
	}
	host := hostOf(standby)
	f.mu.Lock()
	if f.by == nil {
		f.by = map[string]int{}
	}
	f.by[host]++
	f.mu.Unlock()
	return func() {
		f.mu.Lock()
		if f.by[host]--; f.by[host] <= 0 {
			delete(f.by, host)
		}
		f.mu.Unlock()
	}
}

// Active 返回正在接收全量副本的备机，已排序。
func (f *FullSends) Active() []string {
	if f == nil {
		return nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.by))
	for host := range f.by {
		out = append(out, host)
	}
	sort.Strings(out)
	return out
}
