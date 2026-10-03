// Package ha 保证同一时刻只有一个节点修改共享状态：写闸门、角色切换、复制通道与集群成员管理。
package ha

import (
	"fmt"
	"sync"
)

// Gate 是单写入者守卫。所有修改操作（写 API、启动路径、后台清理、dnsmasq 重启）都先问 Allow()。
// 单机上在进程生命周期内一直打开；HA 下备机保持关闭，由激活流程打开。
type Gate struct {
	mu     sync.RWMutex
	closed *ClosedError
}

// ClosedError 表示本节点不得修改，并指出可以修改的节点在哪。
type ClosedError struct {
	Reason    string
	ActiveURL string
}

func (e *ClosedError) Error() string {
	if e.ActiveURL != "" {
		return fmt.Sprintf("%s，请到 %s 操作", e.Reason, e.ActiveURL)
	}
	return e.Reason
}

// NewOpenGate 返回单机用的闸门：一直打开，直到有人关闭。
func NewOpenGate() *Gate { return &Gate{} }

// Allow 在本节点可以修改时返回 nil，否则返回 *ClosedError。
func (g *Gate) Allow() error {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if g.closed != nil {
		return g.closed
	}
	return nil
}

// Close 让之后所有 Allow 都拒绝，并指向主机。
func (g *Gate) Close(reason, activeURL string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = &ClosedError{Reason: reason, ActiveURL: activeURL}
}

// Open 重新放行修改。
func (g *Gate) Open() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.closed = nil
}
