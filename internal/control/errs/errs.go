package errs

import (
	"database/sql"
	"errors"

	"github.com/tianwei/diskless/internal/store"
)

// 错误类别对控制层错误分类，传输层无需认识具体哨兵错误即可把任意服务错误映射为状态码。
var (
	ErrNotFound     = errors.New("not found")
	ErrConflict     = errors.New("conflict")
	ErrInvalid      = errors.New("invalid request")
	ErrUnauthorized = errors.New("unauthorized")
	ErrForbidden    = errors.New("forbidden")
)

// kindError 是带类别、消息不变的哨兵错误：errors.Is 既能匹配哨兵值本身（可比较），也能匹配其类别（经 Is 方法）。
type kindError struct {
	msg  string
	kind error
}

func (e kindError) Error() string        { return e.msg }
func (e kindError) Is(target error) bool { return target == e.kind }

func NotFound(msg string) error     { return kindError{msg, ErrNotFound} }
func Conflict(msg string) error     { return kindError{msg, ErrConflict} }
func Invalid(msg string) error      { return kindError{msg, ErrInvalid} }
func Unauthorized(msg string) error { return kindError{msg, ErrUnauthorized} }

// IsNotFound 判断 err 是否表示「引用的实体不存在」，不论来自哪一层：控制层哨兵、store 哨兵，或漏掉包装的 sql.ErrNoRows。
func IsNotFound(err error) bool {
	return errors.Is(err, ErrNotFound) || errors.Is(err, store.ErrNotFound) || errors.Is(err, sql.ErrNoRows)
}
