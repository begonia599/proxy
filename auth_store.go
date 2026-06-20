// auth_store.go: 用户系统数据层 — users / sessions 的 SQLite CRUD
// + 密码 hash/校验 + session 管理 + 启动引导。
//
// 设计与 keys.go 同风格：DB 方法挂在 *Store 上。鉴权缓存（用户校验）走
// 每请求一次 DB 查询（仿预算查询，单 SQL <1ms），不在内存缓存 session——
// 这样登出/封号立即生效，重启不丢登录态。
package main

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"fmt"
	"log"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// User 一个登录账号。当前单角色，将来分发模式可在此加 role 列。
type User struct {
	ID           int64     `json:"id"`
	Username     string    `json:"username"`
	CreatedAt    time.Time `json:"created_at"`
	PasswordHash string    `json:"-"` // 永不序列化
}

const sessionTTL = 30 * 24 * time.Hour // session 默认 30 天

// ────────────────────── 密码 ──────────────────────

func hashPassword(pw string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func verifyPassword(pw, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(pw)) == nil
}

// ────────────────────── users ──────────────────────

func (s *Store) GetUserByUsername(name string) (*User, error) {
	row := s.db.QueryRow(
		"SELECT id, username, password_hash, created_at FROM users WHERE username = ?", name)
	var u User
	var createdMs int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdMs); err != nil {
		return nil, err
	}
	u.CreatedAt = time.UnixMilli(createdMs)
	return &u, nil
}

func (s *Store) GetUserByID(id int64) (*User, error) {
	row := s.db.QueryRow(
		"SELECT id, username, password_hash, created_at FROM users WHERE id = ?", id)
	var u User
	var createdMs int64
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &createdMs); err != nil {
		return nil, err
	}
	u.CreatedAt = time.UnixMilli(createdMs)
	return &u, nil
}

func (s *Store) CountUsers() (int64, error) {
	row := s.db.QueryRow("SELECT COUNT(*) FROM users")
	var n int64
	if err := row.Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

func (s *Store) CreateUser(username, passwordHash string) (int64, error) {
	res, err := s.db.Exec(
		"INSERT INTO users (username, password_hash, created_at) VALUES (?, ?, ?)",
		username, passwordHash, time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) UpdateUserPassword(id int64, passwordHash string) error {
	_, err := s.db.Exec("UPDATE users SET password_hash = ? WHERE id = ?", passwordHash, id)
	return err
}

// ────────────────────── sessions ──────────────────────

// hashToken 返回 session token 的 SHA-256 十六进制摘要。
// sessions 表只存摘要，不存明文令牌。
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return fmt.Sprintf("%x", sum[:])
}

// CreateSession 签发一个新 session：生成随机令牌，存其 hash，返回明文令牌（仅此一次）。
func (s *Store) CreateSession(userID int64) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	now := time.Now()
	_, err := s.db.Exec(
		"INSERT INTO sessions (token_hash, user_id, created_at, expires_at) VALUES (?, ?, ?, ?)",
		hashToken(token), userID, now.UnixMilli(), now.Add(sessionTTL).UnixMilli())
	if err != nil {
		return "", err
	}
	return token, nil
}

// LookupSession 由明文令牌查有效 session → 返回所属用户。过期/不存在返回 error。
func (s *Store) LookupSession(token string) (*User, error) {
	var userID int64
	var expiresMs int64
	err := s.db.QueryRow(
		"SELECT user_id, expires_at FROM sessions WHERE token_hash = ?", hashToken(token)).
		Scan(&userID, &expiresMs)
	if err != nil {
		return nil, err
	}
	if time.Now().UnixMilli() >= expiresMs {
		_, _ = s.db.Exec("DELETE FROM sessions WHERE token_hash = ?", hashToken(token))
		return nil, fmt.Errorf("session expired")
	}
	return s.GetUserByID(userID)
}

// DeleteSession 删除指定令牌的 session（登出）。
func (s *Store) DeleteSession(token string) error {
	_, err := s.db.Exec("DELETE FROM sessions WHERE token_hash = ?", hashToken(token))
	return err
}

// PurgeExpiredSessions 清掉所有已过期 session。启动时调一次。
func (s *Store) PurgeExpiredSessions() (int64, error) {
	res, err := s.db.Exec("DELETE FROM sessions WHERE expires_at < ?", time.Now().UnixMilli())
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ────────────────────── 启动引导 ──────────────────────

const bootstrapAdminName = "admin"

// bootStrapAdmin 启动时若 users 表为空，建一个 admin 账号 + 随机密码，
// 明文密码打印到日志（一次性），提示首登后改密。
func bootStrapAdmin() {
	n, err := store.CountUsers()
	if err != nil {
		log.Printf("auth bootstrap: count users: %v", err)
		return
	}
	if n > 0 {
		return
	}
	pw := randomPassword(16)
	hash, err := hashPassword(pw)
	if err != nil {
		log.Printf("auth bootstrap: hash failed: %v", err)
		return
	}
	if _, err := store.CreateUser(bootstrapAdminName, hash); err != nil {
		log.Printf("auth bootstrap: create admin failed: %v", err)
		return
	}
	log.Printf("========================================================")
	log.Printf("auth bootstrap: created initial admin account")
	log.Printf("  username: %s", bootstrapAdminName)
	log.Printf("  password: %s   (首次登录后请尽快修改)", pw)
	log.Printf("========================================================")
}

// randomPassword 生成 n 字节 base64url 随机密码。
func randomPassword(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// rand 失败几乎不可能；真发生了用一个弱兜底也比启动崩溃强
		return "change-me-now-" + time.Now().Format("150405")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// migrateCreators 用户系统引入前的密钥没有 creator。启动时把它们都归到 admin
// （迁移期这些密钥都是运营者个人所建）。
func migrateCreators() {
	res, err := store.db.Exec(
		"UPDATE proxy_keys SET creator = ? WHERE creator IS NULL", bootstrapAdminName)
	if err != nil {
		log.Printf("auth migration: backfill creator: %v", err)
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("auth migration: backfilled creator=%s on %d legacy keys", bootstrapAdminName, n)
	}
}

// checkErrNoRows 区分"查不到"（正常 401）和真错误。
func isNoRowsErr(err error) bool {
	return err == sql.ErrNoRows
}