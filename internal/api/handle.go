package api

// handle.go 是 JSON 端点适配器族。机械的 JSON 路由一行注册即可：按服务方法形状选适配器，
// 解码、错误映射、编码都已完成；所有错误都经 writeError 出去，状态码映射不会再按 handler 各自分叉。
//
//	handle(svc.List)                          func(ctx) (Res, error)
//	handleIn(svc.Create, status(201))         func(ctx, Req) (Res, error)
//	handleID(svc.Get, notFound("…不存在"))     func(ctx, id) (Res, error)
//	handleIDIn(svc.Update)                    func(ctx, id, Req) (Res, error)
//	handleErrID(svc.Delete)                   func(ctx, id) error        → 204
//	handleErrIDIn(svc.ChangePassword)         func(ctx, id, Req) error   → 204
//	handleReq(fn)                             func(*http.Request) (Res, error)
//
// id 总是取自路径参数 {id}，单参数路由统一用这个名字。需要更多请求信息（查询参数、多个路径参数）的走 handleReq，同样统一出错。

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/tianwei/diskless/internal/control/errs"
)

// responder 承载每条路由的响应策略。
type responder struct {
	status   int
	notFound string
}

type respondOpt func(*responder)

// status 设置成功状态码（默认 200；204 不发响应体）。
func status(code int) respondOpt {
	return func(rp *responder) { rp.status = code }
}

// notFound 覆盖未找到错误的提示。store 自带的文本（`groups "x": not found`）是给开发者看的，
// 以前显示中文提示的路由靠它保留。
func notFound(msg string) respondOpt {
	return func(rp *responder) { rp.notFound = msg }
}

func newResponder(defaultStatus int, opts []respondOpt) responder {
	rp := responder{status: defaultStatus}
	for _, opt := range opts {
		opt(&rp)
	}
	return rp
}

// funnel 是所有适配器共用的唯一错误出口：先应用 notFound 覆盖，再 writeError。返回是否已写响应。
func funnel(w http.ResponseWriter, rp responder, err error) bool {
	if err != nil && rp.notFound != "" && errs.IsNotFound(err) {
		http.Error(w, rp.notFound, http.StatusNotFound)
		return true
	}
	return writeError(w, err)
}

// finish 是所有适配器的唯一出口：出错走 funnel，否则编码结果。
func finish[Res any](w http.ResponseWriter, rp responder, res Res, err error) {
	if funnel(w, rp, err) {
		return
	}
	writeJSON(w, rp.status, res)
}

// finishEmpty 是只返回 error 的服务方法用的 finish：204 不发响应体，其余回 {"status":"ok"}（ack/action 路由一贯的形状）。
func finishEmpty(w http.ResponseWriter, rp responder, err error) {
	if funnel(w, rp, err) {
		return
	}
	if rp.status == http.StatusNoContent {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, rp.status, map[string]string{"status": "ok"})
}

// decodeBody 读取 JSON 请求体，失败时给出标准的 400 提示。
func decodeBody[Req any](w http.ResponseWriter, r *http.Request) (Req, bool) {
	var req Req
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return req, false
	}
	return req, true
}

func handle[Res any](fn func(context.Context) (Res, error), opts ...respondOpt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := fn(r.Context())
		finish(w, newResponder(http.StatusOK, opts), res, err)
	}
}

func handleIn[Req, Res any](fn func(context.Context, Req) (Res, error), opts ...respondOpt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeBody[Req](w, r)
		if !ok {
			return
		}
		res, err := fn(r.Context(), req)
		finish(w, newResponder(http.StatusOK, opts), res, err)
	}
}

func handleID[Res any](fn func(context.Context, string) (Res, error), opts ...respondOpt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := fn(r.Context(), chi.URLParam(r, "id"))
		finish(w, newResponder(http.StatusOK, opts), res, err)
	}
}

func handleIDIn[Req, Res any](fn func(context.Context, string, Req) (Res, error), opts ...respondOpt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeBody[Req](w, r)
		if !ok {
			return
		}
		res, err := fn(r.Context(), chi.URLParam(r, "id"), req)
		finish(w, newResponder(http.StatusOK, opts), res, err)
	}
}

func handleErrID(fn func(context.Context, string) error, opts ...respondOpt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		err := fn(r.Context(), chi.URLParam(r, "id"))
		finishEmpty(w, newResponder(http.StatusNoContent, opts), err)
	}
}

func handleErrIDIn[Req any](fn func(context.Context, string, Req) error, opts ...respondOpt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		req, ok := decodeBody[Req](w, r)
		if !ok {
			return
		}
		err := fn(r.Context(), chi.URLParam(r, "id"), req)
		finishEmpty(w, newResponder(http.StatusNoContent, opts), err)
	}
}

// handleReq 是需要更多请求信息（查询参数、多个路径值）的路由的出口，错误同样统一处理。
func handleReq[Res any](fn func(*http.Request) (Res, error), opts ...respondOpt) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		res, err := fn(r)
		finish(w, newResponder(http.StatusOK, opts), res, err)
	}
}
