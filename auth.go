// auth.go: 鉴权中间件 + 当前用户 context 传递。
//
// 所有 /admin/*（除仪表盘 HTML）经 requireAuth 包装：取 Authorization: Bearer <token>
// → store.LookupSession → *User 塞 context → 失败 401。session 存 SQLite，
// 每请求一次查询（仿预算查询），登出/封号立即生效，重启不丢登录态。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// userKey 把当前登录用户通过 context 传给 handler。
type userKey struct{}

// requireAuth 包装一个 admin handler：校验 session，塞 *User 到 context。
func requireAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		user, err := store.LookupSession(token)
		if err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
		h(w, r)
	}
}

// currentUser 从 context 取登录用户（requireAuth 之后才有效）。
func currentUser(r *http.Request) *User {
	v, _ := r.Context().Value(userKey{}).(*User)
	return v
}

// bearerToken 从 Authorization 头取 Bearer 令牌。
func bearerToken(r *http.Request) string {
	auth := r.Header.Get("authorization")
	if !strings.HasPrefix(auth, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
}

// writeJSON 写一个 JSON 响应（小工具，auth handler 用）。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}