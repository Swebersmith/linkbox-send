package app

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
)

type User struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
}
type authState struct {
	User              User
	CSRF, SessionHash string
}
type authKey struct{}

func auth(r *http.Request) authState { return r.Context().Value(authKey{}).(authState) }

var usernameRE = regexp.MustCompile(`^[A-Za-z0-9_.-]{3,64}$`)

func hashPassword(p string) (string, error) {
	if len(p) < 12 || len(p) > 72 {
		return "", fmt.Errorf("password must contain 12 to 72 bytes")
	}
	b, e := bcrypt.GenerateFromPassword([]byte(p), 12)
	return string(b), e
}
func (a *App) bootstrap() error {
	var count int
	if e := a.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); e != nil {
		return e
	}
	if count > 0 {
		return nil
	}
	if !usernameRE.MatchString(a.cfg.AdminUsername) {
		return fmt.Errorf("initial ADMIN_USERNAME must be 3-64 letters, numbers, underscores, dots or hyphens")
	}
	hash, e := hashPassword(a.cfg.AdminPassword)
	if e != nil {
		return e
	}
	_, e = a.db.Exec("INSERT INTO users(id,username,password_hash,role,created_at) VALUES(?,?,?,'admin',?)", uuid(), a.cfg.AdminUsername, hash, unixNow())
	return e
}
func (a *App) cookie(w http.ResponseWriter, name, value, path string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: a.cfg.SecureCookies, SameSite: http.SameSiteLaxMode, MaxAge: maxAge})
}
func (a *App) sessionName() string {
	if a.cfg.SecureCookies {
		return "__Host-linkbox_session"
	}
	return "linkbox_session"
}
func (a *App) requireUser(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, e := r.Cookie(a.sessionName())
		if e != nil || !tokenRE.MatchString(c.Value) {
			problem(w, 401, "login required")
			return
		}
		st := authState{SessionHash: a.tokenHash(c.Value)}
		e = a.db.QueryRowContext(r.Context(), `SELECT u.id,u.username,u.role,s.csrf_token FROM sessions s JOIN users u ON u.id=s.user_id WHERE s.token_hash=? AND s.expires_at>?`, st.SessionHash, unixNow()).Scan(&st.User.ID, &st.User.Username, &st.User.Role, &st.CSRF)
		if errors.Is(e, sql.ErrNoRows) {
			problem(w, 401, "session expired")
			return
		}
		if e != nil {
			a.internal(w, "session lookup", e)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" && subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(st.CSRF)) != 1 {
			problem(w, 403, "invalid CSRF token")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), authKey{}, st)))
	}
}
func (a *App) passwordSlot(w http.ResponseWriter) bool {
	select {
	case a.hashSlots <- struct{}{}:
		return true
	default:
		w.Header().Set("Retry-After", "2")
		problem(w, 429, "too many password checks")
		return false
	}
}
func (a *App) login(w http.ResponseWriter, r *http.Request) {
	if !a.rates.allow("login-ip:"+a.clientIP(r), 10, 15*time.Minute) {
		w.Header().Set("Retry-After", "900")
		problem(w, 429, "too many login attempts")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &in) {
		return
	}
	if len(in.Username) > 64 || len(in.Password) > 72 {
		problem(w, 401, "invalid username or password")
		return
	}
	if !a.rates.allow("login-user:"+strings.ToLower(in.Username), 30, 15*time.Minute) {
		problem(w, 429, "too many login attempts")
		return
	}
	if !a.passwordSlot(w) {
		return
	}
	defer func() { <-a.hashSlots }()
	var u User
	var hash string
	e := a.db.QueryRow("SELECT id,username,role,password_hash FROM users WHERE username=?", in.Username).Scan(&u.ID, &u.Username, &u.Role, &hash)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		a.internal(w, "login query", e)
		return
	}
	if e != nil { // Equal-cost work for unknown accounts prevents cheap username timing probes.
		_, _ = bcrypt.GenerateFromPassword([]byte("unknown-account-dummy"), 12)
	}
	if e != nil || bcrypt.CompareHashAndPassword([]byte(hash), []byte(in.Password)) != nil {
		a.log.Info("login", "success", false, "ip", a.clientIP(r))
		problem(w, 401, "invalid username or password")
		return
	}
	token, csrf := randomToken(), randomToken()
	now := unixNow()
	tx, e := a.db.Begin()
	if e != nil {
		a.internal(w, "login transaction", e)
		return
	}
	defer tx.Rollback()
	// Bound sessions per user; each login rotates its cookie and CSRF secret.
	if _, e = tx.Exec("DELETE FROM sessions WHERE user_id=? AND token_hash NOT IN (SELECT token_hash FROM sessions WHERE user_id=? ORDER BY created_at DESC LIMIT 9)", u.ID, u.ID); e != nil {
		a.internal(w, "session prune", e)
		return
	}
	if old, e := r.Cookie(a.sessionName()); e == nil {
		if _, e = tx.Exec("DELETE FROM sessions WHERE token_hash=?", a.tokenHash(old.Value)); e != nil {
			a.internal(w, "session rotation", e)
			return
		}
	}
	if _, e = tx.Exec("INSERT INTO sessions(token_hash,user_id,csrf_token,expires_at,created_at) VALUES(?,?,?,?,?)", a.tokenHash(token), u.ID, csrf, now+int64(a.cfg.SessionExpiry.Seconds()), now); e != nil {
		a.internal(w, "create session", e)
		return
	}
	if e = tx.Commit(); e != nil {
		a.internal(w, "commit session", e)
		return
	}
	a.cookie(w, a.sessionName(), token, "/", int(a.cfg.SessionExpiry.Seconds()))
	a.log.Info("login", "success", true, "user_id", u.ID)
	jsonOut(w, 200, map[string]any{"user": u, "csrfToken": csrf})
}
func (a *App) me(w http.ResponseWriter, r *http.Request) {
	s := auth(r)
	jsonOut(w, 200, map[string]any{"user": s.User, "csrfToken": s.CSRF})
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	if _, e := a.db.Exec("DELETE FROM sessions WHERE token_hash=?", auth(r).SessionHash); e != nil {
		a.internal(w, "logout", e)
		return
	}
	a.cookie(w, a.sessionName(), "", "/", -1)
	w.WriteHeader(204)
}
func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	if auth(r).User.Role != "admin" {
		problem(w, 403, "admin required")
		return
	}
	var in struct {
		Username string `json:"username"`
		Password string `json:"password"`
		Role     string `json:"role"`
	}
	if !decode(w, r, &in) {
		return
	}
	if in.Role == "" {
		in.Role = "user"
	}
	if !usernameRE.MatchString(in.Username) || (in.Role != "user" && in.Role != "admin") {
		problem(w, 400, "invalid username or role")
		return
	}
	if !a.passwordSlot(w) {
		return
	}
	defer func() { <-a.hashSlots }()
	hash, e := hashPassword(in.Password)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	u := User{ID: uuid(), Username: in.Username, Role: in.Role}
	result, e := a.db.Exec("INSERT INTO users(id,username,password_hash,role,created_at) VALUES(?,?,?,?,?) ON CONFLICT(username) DO NOTHING", u.ID, u.Username, hash, u.Role, unixNow())
	if e != nil {
		a.internal(w, "create user", e)
		return
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		problem(w, 409, "username already exists")
		return
	}
	jsonOut(w, 201, u)
}
func (a *App) listUsers(w http.ResponseWriter, r *http.Request) {
	if auth(r).User.Role != "admin" {
		problem(w, 403, "admin required")
		return
	}
	rows, e := a.db.Query("SELECT id,username,role FROM users ORDER BY created_at DESC LIMIT 1000")
	if e != nil {
		a.internal(w, "users", e)
		return
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var u User
		if e = rows.Scan(&u.ID, &u.Username, &u.Role); e != nil {
			a.internal(w, "users scan", e)
			return
		}
		users = append(users, u)
	}
	if e = rows.Err(); e != nil {
		a.internal(w, "users rows", e)
		return
	}
	jsonOut(w, 200, users)
}
func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	if !a.rates.allow("password:"+auth(r).User.ID, 5, 15*time.Minute) {
		problem(w, 429, "too many attempts")
		return
	}
	var in struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decode(w, r, &in) {
		return
	}
	if !a.passwordSlot(w) {
		return
	}
	defer func() { <-a.hashSlots }()
	var old string
	if e := a.db.QueryRow("SELECT password_hash FROM users WHERE id=?", auth(r).User.ID).Scan(&old); e != nil {
		a.internal(w, "password lookup", e)
		return
	}
	if bcrypt.CompareHashAndPassword([]byte(old), []byte(in.CurrentPassword)) != nil {
		problem(w, 401, "incorrect current password")
		return
	}
	hash, e := hashPassword(in.NewPassword)
	if e != nil {
		problem(w, 400, e.Error())
		return
	}
	tx, e := a.db.Begin()
	if e != nil {
		a.internal(w, "password transaction", e)
		return
	}
	defer tx.Rollback()
	if _, e = tx.Exec("UPDATE users SET password_hash=? WHERE id=?", hash, auth(r).User.ID); e == nil {
		_, e = tx.Exec("DELETE FROM sessions WHERE user_id=?", auth(r).User.ID)
	}
	if e != nil {
		a.internal(w, "password change", e)
		return
	}
	if e = tx.Commit(); e != nil {
		a.internal(w, "password commit", e)
		return
	}
	a.cookie(w, a.sessionName(), "", "/", -1)
	w.WriteHeader(204)
}
