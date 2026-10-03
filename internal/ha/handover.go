package ha

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// 交接中写入者一侧（接任一侧见 takeover.go）。
// 交接是对端发起的计划切换：备机持有 VIP，本节点仍是写入者，上一轮之后确认的改动不能丢。
// 所以先停止接收改动，打最后一轮给对方追平，收到通知后再降级。

// ErrWriterServing 表示写入者因持有 VIP 并在服务而拒绝交接：它活着且在主事，请求方不得接任。
// 不同于打不出轮次，那表示它已无力交出任何东西。
var ErrWriterServing = errors.New("本节点持有虚 IP 并在提供服务，不交出主机身份")

// defaultHandoverHold 是写入者为等接任节点而保持关闭改动的时长，超过就认为对方已不在，继续工作。
const defaultHandoverHold = 5 * time.Minute

func (c *Controller) handoverHold() time.Duration {
	if c.HandoverHold > 0 {
		return c.HandoverHold
	}
	return defaultHandoverHold
}

// PrepareHandover 关闭本写入者的改动并打最后一轮。
func (c *Controller) PrepareHandover(ctx context.Context, to string) (string, error) {
	if c.State().Role != "active" {
		return "", ErrNotActive
	}
	if c.HoldsVIP != nil && c.HoldsVIP() {
		return "", ErrWriterServing
	}
	if c.StampFinalRound == nil {
		return "", errors.New("本节点没有配置目录复制，无法交接")
	}
	if to == "" {
		to = "对端"
	}
	// 先关闸再打标：这一轮之后不能再有任何写入落地。
	c.Gate.Close(fmt.Sprintf("正在把主机身份交给节点 %s，交接完成前不接受改动，请稍后在虚 IP 上重试", to), "")
	marker, err := c.StampFinalRound(ctx, c.handoverHold())
	if err != nil {
		c.releaseHandover()
		return "", err
	}
	c.stateMu.Lock()
	if c.handoverTimer != nil {
		c.handoverTimer.Stop()
	}
	c.handoverTimer = time.AfterFunc(c.handoverHold(), func() {
		c.logf("the node taking over did not finish the handover; taking changes again", "to", to)
		c.releaseHandover()
	})
	c.stateMu.Unlock()
	c.logf("handover prepared: changes closed, final round stamped", "to", to, "round", marker)
	return marker, nil
}

// AbortHandover 重新打开本写入者：对端不接任了。
func (c *Controller) AbortHandover() {
	c.logf("handover called off; taking changes again")
	c.releaseHandover()
}

// releaseHandover 在仍是写入者的节点上撤销 PrepareHandover。
func (c *Controller) releaseHandover() {
	c.stateMu.Lock()
	if c.handoverTimer != nil {
		c.handoverTimer.Stop()
		c.handoverTimer = nil
	}
	c.stateMu.Unlock()
	if c.State().Role != "active" {
		return // 期间已降级：备机的闸门保持关闭
	}
	c.Gate.Open()
	if c.ResumeRounds != nil {
		c.ResumeRounds()
	}
}

// CommitHandover 为已追平的节点让本写入者降级。
func (c *Controller) CommitHandover(ctx context.Context, to string) error {
	if c.State().Role != "active" {
		return nil
	}
	return c.Standby(ctx, "handover to "+to)
}

func (h ControlHandler) serveHandover(w http.ResponseWriter, r *http.Request) {
	to := r.URL.Query().Get("to")
	switch {
	case strings.HasSuffix(r.URL.Path, "/handover/prepare"):
		marker, err := h.Controller.PrepareHandover(r.Context(), to)
		if err != nil {
			code := http.StatusConflict
			if errors.Is(err, ErrWriterServing) {
				code = http.StatusLocked // 与「打不出最后一轮」分开：它还活着、在服务
			}
			http.Error(w, err.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"marker": marker})
	case strings.HasSuffix(r.URL.Path, "/handover/abort"):
		h.Controller.AbortHandover()
		w.WriteHeader(http.StatusOK)
	case strings.HasSuffix(r.URL.Path, "/handover/commit"):
		// 先应答：降级会重启进程，而接任节点在等这个答复才继续。
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		go func() {
			time.Sleep(200 * time.Millisecond)
			if err := h.Controller.CommitHandover(context.Background(), to); err != nil {
				h.Controller.logf("stepping down for the handover failed", "error", err.Error())
			}
		}()
	default:
		http.NotFound(w, r)
	}
}

func (p Peer) post(ctx context.Context, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimSuffix(p.BaseURL, "/")+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.Token)
	if p.Self != "" {
		req.Header.Set(nodeHeader, p.Self)
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		err := fmt.Errorf("对端 %s 返回 %d: %s", p.BaseURL, resp.StatusCode, strings.TrimSpace(string(body)))
		if resp.StatusCode == http.StatusLocked {
			return nil, fmt.Errorf("%w（%v）", ErrWriterServing, err)
		}
		return nil, err
	}
	return resp, nil
}

// PrepareHandover、CommitHandover 和 AbortHandover 让 Peer 实现 HandoverPeer。
func (p Peer) PrepareHandover(ctx context.Context, to string) (string, error) {
	resp, err := p.post(ctx, "/internal/ha/handover/prepare?to="+url.QueryEscape(to))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Marker string `json:"marker"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	return out.Marker, nil
}

func (p Peer) CommitHandover(ctx context.Context, to string) error {
	resp, err := p.post(ctx, "/internal/ha/handover/commit?to="+url.QueryEscape(to))
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (p Peer) AbortHandover(ctx context.Context) error {
	resp, err := p.post(ctx, "/internal/ha/handover/abort")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}
