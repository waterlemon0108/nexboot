package api

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
)

// exportTicketTTL 是下载票据的有效期：够页面把 URL 交给浏览器打开，又短到泄露的 URL（代理日志、历史记录）
// 被重放时已失效。下载本身可以跑几小时，票据只在打开时检查一次。
const exportTicketTTL = 60 * time.Second

// exportTickets 用登录后的 POST 换取一个短期、一次性的令牌，供普通浏览器下载携带。下载从 <a href> 发起，
// 带不了 Authorization 头，把登录 token 放进 URL 又会留在日志和历史里。故意只放内存：票据只是主节点上
// 相隔几秒的两次请求之间的凭据，不值得复制。
type exportTickets struct {
	mu      sync.Mutex
	now     func() time.Time
	tickets map[string]exportTicket
}

type exportTicket struct {
	imageID string
	// reductionID 非空时表示下载的是该还原点。
	reductionID string
	compress    bool
	expires     time.Time
}

func newExportTickets(now func() time.Time) *exportTickets {
	if now == nil {
		now = time.Now
	}
	return &exportTickets{now: now, tickets: map[string]exportTicket{}}
}

func (t *exportTickets) Issue(imageID string, compress bool) (string, error) {
	return t.issue(exportTicket{imageID: imageID, compress: compress})
}

// IssueReduction 是为还原点下载签发票据的 Issue。
func (t *exportTickets) IssueReduction(reductionID string, compress bool) (string, error) {
	return t.issue(exportTicket{reductionID: reductionID, compress: compress})
}

func (t *exportTickets) issue(ticket exportTicket) (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	// 惰性清理：每来一张新票据就顺手清掉过期的。
	for k, v := range t.tickets {
		if now.After(v.expires) {
			delete(t.tickets, k)
		}
	}
	ticket.expires = now.Add(exportTicketTTL)
	t.tickets[token] = ticket
	return token, nil
}

// Consume 兑换令牌，且只能兑换一次，返回签发时对应的镜像以及是否要求压缩流。
// 下载是不带请求体的裸 GET，这个选择只能随票据带过去。
func (t *exportTickets) Consume(token string) (exportTicket, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	ticket, ok := t.tickets[token]
	if !ok {
		return exportTicket{}, false
	}
	delete(t.tickets, token)
	if t.now().After(ticket.expires) {
		return exportTicket{}, false
	}
	return ticket, true
}

func exportTicketHandler(service ImageImportService, tickets *exportTickets) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		compress := r.URL.Query().Get("compress") == "gzip"
		// 提前执行下载时会做的同样检查，页面在生成 URL 之前就能告诉运维「仍在导入」。
		name, err := service.ExportDownloadName(r.Context(), id, compress)
		if writeError(w, err) {
			return
		}
		token, err := tickets.Issue(id, compress)
		if writeError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      token,
			"url":        "/api/images/export-download?token=" + token,
			"expires_in": int(exportTicketTTL / time.Second),
			"file_name":  name,
		})
	}
}

// reductionExportTicketHandler 是还原点版的 exportTicketHandler。
func reductionExportTicketHandler(service ImageImportService, tickets *exportTickets) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		compress := r.URL.Query().Get("compress") == "gzip"
		name, err := service.ReductionExportName(r.Context(), id, compress)
		if writeError(w, err) {
			return
		}
		token, err := tickets.IssueReduction(id, compress)
		if writeError(w, err) {
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token":      token,
			"url":        "/api/images/export-download?token=" + token,
			"expires_in": int(exportTicketTTL / time.Second),
			"file_name":  name,
		})
	}
}

func exportDownloadHandler(service ImageImportService, tickets *exportTickets, logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ticket, ok := tickets.Consume(r.URL.Query().Get("token"))
		if !ok {
			http.Error(w, "下载链接无效或已过期，请回到镜像页重新发起下载", http.StatusUnauthorized)
			return
		}
		name, err := "", error(nil)
		if ticket.reductionID != "" {
			name, err = service.ReductionExportName(r.Context(), ticket.reductionID, ticket.compress)
		} else {
			name, err = service.ExportDownloadName(r.Context(), ticket.imageID, ticket.compress)
		}
		if writeError(w, err) {
			return
		}
		// 载荷就是归档文件，按归档类型标注。用 Content-Encoding 的话浏览器会在传输中解压，存下一个不是 gzip 的 .gz 文件。
		contentType := "application/octet-stream"
		if ticket.compress {
			contentType = "application/gzip"
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Content-Disposition", contentDisposition(name))
		w.WriteHeader(http.StatusOK)
		// 从这里起状态码已发出，出错只能截断流，对端的 zfs receive 会将其识别为截断。
		stream := func() error { return service.StreamImage(r.Context(), ticket.imageID, w, ticket.compress) }
		if ticket.reductionID != "" {
			stream = func() error { return service.StreamReduction(r.Context(), ticket.reductionID, w, ticket.compress) }
		}
		if err := stream(); err != nil {
			if logger != nil {
				logger.Warn("image download aborted", "image", ticket.imageID, "reduction", ticket.reductionID, "error", fmt.Sprint(err))
			}
			// 正常返回会写出 chunked 结束块，浏览器把残缺文件当成下载完成；ErrAbortHandler 让 net/http 直接断开连接。
			panic(http.ErrAbortHandler)
		}
	}
}
