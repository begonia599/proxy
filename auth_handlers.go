// auth_handlers.go: /auth/* 端点 — 登录 / 登出 / 当前用户 / 改密。
package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// authLoginHandler POST /auth/login {username, password} → {token, user}
func authLoginHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		req.Username = strings.TrimSpace(req.Username)
		if req.Username == "" || req.Password == "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "username and password required"})
			return
		}
		user, err := store.GetUserByUsername(req.Username)
		if err != nil || !verifyPassword(req.Password, user.PasswordHash) {
			// 用户不存在或密码错都回 401，不泄露用户名是否存在
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid credentials"})
			return
		}
		token, err := store.CreateSession(user.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "session create failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"token": token,
			"user":  map[string]any{"id": user.ID, "username": user.Username},
		})
	}
}

// authLogoutHandler POST /auth/logout → 删当前 session
func authLogoutHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		token := bearerToken(r)
		if token != "" {
			_ = store.DeleteSession(token)
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}

// authMeHandler GET /auth/me → 当前登录用户（前端校验登录态用）
func authMeHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		token := bearerToken(r)
		if token == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		user, err := store.LookupSession(token)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id":       user.ID,
			"username": user.Username,
		})
	}
}

// authPasswordHandler POST /auth/password {old, new} → 改自己的密码
func authPasswordHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		user := currentUser(r)
		if user == nil {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		var req struct {
			Old string `json:"old"`
			New string `json:"new"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid body"})
			return
		}
		if len(req.New) < 6 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "new password too short (min 6)"})
			return
		}
		if !verifyPassword(req.Old, user.PasswordHash) {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "old password incorrect"})
			return
		}
		hash, err := hashPassword(req.New)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "hash failed"})
			return
		}
		if err := store.UpdateUserPassword(user.ID, hash); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "update failed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	}
}