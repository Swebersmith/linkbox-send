package app

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	AppURL, ListenAddr, DatabasePath, StoragePath, ChunkPath           string
	AdminUsername, AdminPassword, SessionSecret, LogLevel              string
	ChunkSize, MaxFileSize, ReservedBytes                              int64
	Concurrency, MaxActiveUploads, MaxUploadsPerUser, MaxChunkRequests int
	UploadExpiry, SessionExpiry                                        time.Duration
	SecureCookies, TrustProxy                                          bool
	TrustedProxyCIDRs                                                  string
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
func LoadConfig() (Config, error) {
	c := Config{AppURL: strings.TrimRight(env("APP_URL", "https://localhost"), "/"), ListenAddr: env("LISTEN_ADDR", ":8080"),
		DatabasePath: env("DATABASE_PATH", "/data/database/linkbox.db"), StoragePath: env("STORAGE_PATH", "/data/files"), ChunkPath: env("CHUNK_PATH", "/data/chunks"),
		AdminUsername: os.Getenv("ADMIN_USERNAME"), AdminPassword: os.Getenv("ADMIN_PASSWORD"), SessionSecret: os.Getenv("SESSION_SECRET"), LogLevel: env("LOG_LEVEL", "info"),
		SecureCookies: env("COOKIE_SECURE", "true") == "true", TrustProxy: env("TRUST_PROXY", "true") == "true", TrustedProxyCIDRs: env("TRUSTED_PROXY_CIDRS", "172.30.48.0/24"), SessionExpiry: 7 * 24 * time.Hour}
	nums := []struct {
		k, d string
		p    *int64
	}{{"UPLOAD_CHUNK_SIZE", "33554432", &c.ChunkSize}, {"MAX_FILE_SIZE", "107374182400", &c.MaxFileSize}, {"STORAGE_RESERVED_BYTES", "2147483648", &c.ReservedBytes}}
	for _, n := range nums {
		v, e := strconv.ParseInt(env(n.k, n.d), 10, 64)
		if e != nil || v < 0 {
			return c, fmt.Errorf("invalid %s", n.k)
		}
		*n.p = v
	}
	for _, n := range []struct {
		k, d string
		p    *int
	}{{"UPLOAD_CONCURRENCY", "4", &c.Concurrency}, {"MAX_ACTIVE_UPLOADS", "100", &c.MaxActiveUploads}, {"MAX_UPLOADS_PER_USER", "10", &c.MaxUploadsPerUser}, {"MAX_CHUNK_REQUESTS", "16", &c.MaxChunkRequests}} {
		v, e := strconv.Atoi(env(n.k, n.d))
		if e != nil || v < 1 || v > 1000 {
			return c, fmt.Errorf("invalid %s", n.k)
		}
		*n.p = v
	}
	hours, e := strconv.Atoi(env("UPLOAD_EXPIRE_HOURS", "24"))
	if e != nil || hours < 1 || hours > 8760 {
		return c, fmt.Errorf("invalid UPLOAD_EXPIRE_HOURS")
	}
	c.UploadExpiry = time.Duration(hours) * time.Hour
	u, e := url.Parse(c.AppURL)
	if e != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return c, fmt.Errorf("APP_URL must be an origin, without path")
	}
	if !c.SecureCookies && u.Hostname() != "localhost" && u.Hostname() != "127.0.0.1" {
		return c, fmt.Errorf("COOKIE_SECURE=false is only allowed on localhost")
	}
	if c.SecureCookies && u.Scheme != "https" {
		return c, fmt.Errorf("secure cookies require HTTPS APP_URL")
	}
	if c.ChunkSize != 16<<20 && c.ChunkSize != 32<<20 && c.ChunkSize != 64<<20 {
		return c, fmt.Errorf("UPLOAD_CHUNK_SIZE must be 16, 32 or 64 MiB")
	}
	if c.Concurrency != 1 && c.Concurrency != 2 && c.Concurrency != 4 && c.Concurrency != 6 && c.Concurrency != 8 {
		return c, fmt.Errorf("invalid UPLOAD_CONCURRENCY")
	}
	if c.MaxFileSize < 1 || c.MaxFileSize > 1<<50 || c.ReservedBytes > 1<<60 {
		return c, fmt.Errorf("invalid storage limits")
	}
	if len(c.SessionSecret) < 32 {
		return c, fmt.Errorf("SESSION_SECRET must contain at least 32 random characters")
	}
	return c, nil
}
