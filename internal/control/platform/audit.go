package platform

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"github.com/tianwei/diskless/internal/domain"
	"github.com/tianwei/diskless/internal/store"
)

// AuditService 记录和查询操作/登录审计日志。
type AuditService struct {
	Store store.Store
	Now   func() time.Time
}

// AuditEntry 是一条待记录的审计事件，ID 与 CreatedAt 由服务填写。
type AuditEntry struct {
	Type       string // operation | login
	Username   string
	Action     string
	Module     string
	Detail     string
	IP         string
	UserAgent  string
	Status     string // ok | err
	HTTPStatus int
	CostMs     int64
}

type AuditQuery struct {
	Type     string
	Username string
	Module   string
	Status   string
	Search   string
	From     string
	To       string
	Page     int
	Size     int
}

type AuditListResult struct {
	Items []domain.AuditLog `json:"items"`
	Total int               `json:"total"`
	Page  int               `json:"page"`
	Size  int               `json:"size"`
}

func (s AuditService) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

// Record 持久化一条审计记录，调用方按尽力而为处理失败。
func (s AuditService) Record(ctx context.Context, e AuditEntry) error {
	now := s.now()
	if e.Status == "" {
		e.Status = "ok"
	}
	log := domain.AuditLog{
		ID:         "log-" + randID(),
		Type:       e.Type,
		Username:   e.Username,
		Action:     e.Action,
		Module:     e.Module,
		Detail:     e.Detail,
		IP:         e.IP,
		UserAgent:  e.UserAgent,
		Status:     e.Status,
		HTTPStatus: e.HTTPStatus,
		CostMs:     e.CostMs,
		CreatedAt:  now,
	}
	return s.Store.AuditLogs().Create(ctx, log)
}

func (s AuditService) List(ctx context.Context, q AuditQuery) (AuditListResult, error) {
	page := q.Page
	if page < 1 {
		page = 1
	}
	size := q.Size
	if size <= 0 || size > 200 {
		size = 20
	}
	items, total, err := s.Store.AuditLogs().ListPaged(ctx, store.AuditFilter{
		Type: q.Type, Username: q.Username, Module: q.Module, Status: q.Status,
		Search: q.Search, From: q.From, To: q.To,
	}, size, (page-1)*size)
	if err != nil {
		return AuditListResult{}, err
	}
	if items == nil {
		items = []domain.AuditLog{}
	}
	return AuditListResult{Items: items, Total: total, Page: page, Size: size}, nil
}

func randID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return time.Now().Format("20060102150405.000000000")
	}
	return hex.EncodeToString(b[:])
}
