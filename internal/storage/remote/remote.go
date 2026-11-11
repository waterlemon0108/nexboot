// Package remote 让放置接缝跨节点：Handler 在 /internal/storage/ 下提供本节点存储代理，Client 调用它。
// 每个方法一个 POST，JSON 进出，凭集群令牌鉴权。传输必须忠实：参数和结果原样传递，
// 调用方据以分支的类型化错误（SuperSessionMissing）也要原样还原。
package remote

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/tianwei/diskless/internal/storage"
)

// Handler 是节点侧服务端。每个集群节点不论角色都要运行：服务放置在本节点的客户机是本机工作，不只是主节点的事。
type Handler struct {
	Agent               storage.ClientHostAgent
	Token               string
	NodeID              string
	CleanupInstallation func(context.Context, storage.InstallationCleanupReq) error
}

// wireError 承载跨节点的类型化错误：kind 告诉客户端还原成哪种错误，其余信息放在字段里。
type wireError struct {
	Error string `json:"error"`
	Kind  string `json:"kind,omitempty"`
	LUN   int    `json:"lun,omitempty"`
	// super_hive_unsafe 的拒绝原因，见 storage.SuperHiveUnsafe。
	FastStartup bool   `json:"fast_startup,omitempty"`
	Reason      string `json:"reason,omitempty"`
}

func (h Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.Token == "" || subtle.ConstantTimeCompare([]byte(bearer(r)), []byte(h.Token)) != 1 {
		http.Error(w, "cluster token required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	ctx := r.Context()
	var (
		out any
		err error
	)
	switch method {
	case "cleanup-installation-resources":
		var req storage.InstallationCleanupReq
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&req); err != nil || h.NodeID == "" || req.NodeID != h.NodeID ||
			!regexp.MustCompile(`^install-[0-9a-f]{16}$`).MatchString(req.ID) ||
			!regexp.MustCompile(`^[0-9A-F]{12}$`).MatchString(req.MAC) ||
			(req.ImageID != "" && req.ImageID != "iso_"+strings.TrimPrefix(req.ID, "install-")) {
			http.Error(w, "invalid installation cleanup target", http.StatusBadRequest)
			return
		}
		if h.CleanupInstallation == nil {
			err = storage.ErrNotImplemented
		} else {
			err = h.CleanupInstallation(ctx, req)
			out = struct{}{}
		}
	case "create-client-lun":
		var req storage.ClientReq
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			out, err = h.Agent.CreateClientLUN(ctx, req)
		}
	case "cleanup-client-clones":
		var req struct {
			Pool string   `json:"pool"`
			MACs []string `json:"macs"`
		}
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			err = h.Agent.CleanupClientClones(ctx, req.Pool, req.MACs)
			out = struct{}{}
		}
	case "reclaim-idle-client-clones":
		// 传「决定过去多久」而非时间点，两台机器的时钟不必一致。
		var req struct {
			MAC string        `json:"mac"`
			Age time.Duration `json:"age_ns"`
		}
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			out, err = h.Agent.ReclaimIdleClientClones(ctx, req.MAC, time.Now().Add(-req.Age))
		}
	case "active-client-macs":
		out, err = h.Agent.ActiveClientMACs(ctx)
	case "client-clone-macs":
		out, err = h.Agent.ClientCloneMACs(ctx)
	case "super-start":
		var req storage.ClientReq
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			out, err = h.Agent.SuperStart(ctx, req)
		}
	case "super-stop":
		var req storage.SuperStopReq
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			out, err = h.Agent.SuperStop(ctx, req)
		}
	case "create-identity-layer":
		var req storage.IdentityLayerReq
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			if agent, ok := h.Agent.(storage.IdentityHostAgent); ok {
				err = agent.CreateIdentityLayer(ctx, req)
				out = struct{}{}
			} else {
				err = storage.ErrNotImplemented
			}
		}
	case "delete-identity-layer":
		var req struct {
			MAC string `json:"mac"`
		}
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			if agent, ok := h.Agent.(storage.IdentityHostAgent); ok {
				err = agent.DeleteIdentityLayer(ctx, req.MAC)
				out = struct{}{}
			} else {
				err = storage.ErrNotImplemented
			}
		}
	case "release-identity-layers":
		var req struct {
			Sources []string `json:"sources"`
		}
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			if agent, ok := h.Agent.(storage.IdentityLayerReleaser); ok {
				out, err = agent.ReleaseIdentityLayers(ctx, req.Sources)
			} else {
				err = storage.ErrNotImplemented
			}
		}
	case "delete-aside-image":
		var req struct {
			ImageID   string   `json:"image_id"`
			ConfigIDs []string `json:"config_ids"`
		}
		if err = json.NewDecoder(r.Body).Decode(&req); err == nil {
			if agent, ok := h.Agent.(storage.AsideHostAgent); ok {
				err = agent.DeleteAsideImage(ctx, req.ImageID, req.ConfigIDs)
				out = struct{}{}
			} else {
				err = storage.ErrNotImplemented
			}
		}
	case "space-usage":
		out, err = h.Agent.SpaceUsage(ctx)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		we := wireError{Error: err.Error()}
		var missing storage.SuperSessionMissing
		if errors.As(err, &missing) {
			we.Kind = "super_session_missing"
			we.LUN = missing.LUN
		}
		var unsafe storage.SuperHiveUnsafe
		if errors.As(err, &unsafe) {
			we.Kind = "super_hive_unsafe"
			we.FastStartup, we.Reason = unsafe.FastStartup, unsafe.Reason
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(we)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func bearer(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// Client 调用某节点的 Handler，零值设好 BaseURL 和 Token 即可用。
type Client struct {
	BaseURL string
	Token   string
	// Timeout 限定单次调用，零值为 60s；建克隆和超管保存要在对端做实际的 ZFS 操作。
	Timeout time.Duration
}

func (c Client) call(ctx context.Context, method string, in, dst any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/internal/storage/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Content-Type", "application/json")
	timeout := c.Timeout
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	resp, err := (&http.Client{Timeout: timeout}).Do(req)
	if err != nil {
		return storage.NodeUnreachable{Node: c.BaseURL, Err: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var we wireError
		if json.NewDecoder(resp.Body).Decode(&we) == nil && we.Error != "" {
			if we.Kind == "super_session_missing" {
				return storage.SuperSessionMissing{LUN: we.LUN}
			}
			if we.Kind == "super_hive_unsafe" {
				return storage.SuperHiveUnsafe{FastStartup: we.FastStartup, Reason: we.Reason}
			}
			return errors.New(we.Error)
		}
		return fmt.Errorf("storage node %s: %s", c.BaseURL, resp.Status)
	}
	if dst == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(dst)
}

func (c Client) CreateClientLUN(ctx context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	var out storage.LUNInfo
	err := c.call(ctx, "create-client-lun", req, &out)
	return out, err
}

func (c Client) CleanupClientClones(ctx context.Context, pool string, macs []string) error {
	in := struct {
		Pool string   `json:"pool"`
		MACs []string `json:"macs"`
	}{pool, macs}
	return c.call(ctx, "cleanup-client-clones", in, nil)
}

func (c Client) CleanupInstallationResources(ctx context.Context, req storage.InstallationCleanupReq) error {
	return c.call(ctx, "cleanup-installation-resources", req, nil)
}

func (c Client) ReclaimIdleClientClones(ctx context.Context, mac string, decidedAt time.Time) (bool, error) {
	in := struct {
		MAC string        `json:"mac"`
		Age time.Duration `json:"age_ns"`
	}{mac, time.Since(decidedAt)}
	var out bool
	err := c.call(ctx, "reclaim-idle-client-clones", in, &out)
	return out, err
}

func (c Client) ActiveClientMACs(ctx context.Context) ([]string, error) {
	var out []string
	err := c.call(ctx, "active-client-macs", struct{}{}, &out)
	return out, err
}

func (c Client) ClientCloneMACs(ctx context.Context) ([]string, error) {
	var out []string
	err := c.call(ctx, "client-clone-macs", struct{}{}, &out)
	return out, err
}

func (c Client) SuperStart(ctx context.Context, req storage.ClientReq) (storage.LUNInfo, error) {
	var out storage.LUNInfo
	err := c.call(ctx, "super-start", req, &out)
	return out, err
}

func (c Client) SuperStop(ctx context.Context, req storage.SuperStopReq) (storage.SuperStopResult, error) {
	var out storage.SuperStopResult
	err := c.call(ctx, "super-stop", req, &out)
	return out, err
}

func (c Client) SpaceUsage(ctx context.Context) (storage.SpaceUsage, error) {
	var out storage.SpaceUsage
	err := c.call(ctx, "space-usage", struct{}{}, &out)
	return out, err
}

func (c Client) CreateIdentityLayer(ctx context.Context, req storage.IdentityLayerReq) error {
	return c.call(ctx, "create-identity-layer", req, nil)
}
func (c Client) DeleteIdentityLayer(ctx context.Context, mac string) error {
	return c.call(ctx, "delete-identity-layer", struct {
		MAC string `json:"mac"`
	}{mac}, nil)
}

func (c Client) ReleaseIdentityLayers(ctx context.Context, sources []string) ([]string, error) {
	var released []string
	err := c.call(ctx, "release-identity-layers", struct {
		Sources []string `json:"sources"`
	}{sources}, &released)
	return released, err
}

func (c Client) DeleteAsideImage(ctx context.Context, imageID string, configIDs []string) error {
	return c.call(ctx, "delete-aside-image", struct {
		ImageID   string   `json:"image_id"`
		ConfigIDs []string `json:"config_ids"`
	}{imageID, configIDs}, nil)
}
