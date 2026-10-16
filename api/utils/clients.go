package utils

import (
	"context"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/Arkel-ai/nomos/api/config"
)

var clientsLog = NewLogger("utils.clients")

// Package-level clients. Each is nil when its credentials aren't configured;
// handlers MUST guard with a nil check and return 503.
var (
	// S3 is the object-storage client (S3-compatible). nil when S3 isn't
	// configured.
	S3 *minio.Client
	// S3Bucket is the bucket every app shares; S3Prefix is this app's
	// private "folder" inside it (<app-name>/). All object keys for this
	// app MUST be built with S3Prefix so apps never see each other's files.
	S3Bucket string
	S3Prefix string
)

// InitClients populates the package-level clients/handles from the loaded
// config. Missing credentials log a friendly notice and leave the var nil.
func InitClients(cfg config.Config) {
	// Postgres first — Casbin and the auth layer both build on top of it.
	initDB(cfg.DatabaseURL)
	initCasbin()
	initS3(cfg)
}

// initS3 builds the S3 client and ensures this app's folder exists. The
// bucket is shared across every app, so each app is namespaced to the
// S3Prefix ("<app-name>/") — never write outside it.
func initS3(cfg config.Config) {
	if cfg.S3Endpoint == "" || cfg.S3Bucket == "" || cfg.S3AccessKey == "" || cfg.S3SecretKey == "" {
		clientsLog.Info("S3 not configured — file endpoints will return 503")
		return
	}

	appName := cfg.AppName
	if appName == "" {
		appName = "app" // local-dev fallback when APP_NAME is unset
	}
	S3Prefix = appName + "/"
	S3Bucket = cfg.S3Bucket

	client, err := minio.New(cfg.S3Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.S3AccessKey, cfg.S3SecretKey, ""),
		Secure: true, // Scaleway endpoints are HTTPS-only
		Region: cfg.S3Region,
	})
	if err != nil {
		clientsLog.Warnf("S3 init failed: %v — file endpoints will return 503", err)
		return
	}
	S3 = client
	clientsLog.Infof("S3 client initialized (bucket=%s prefix=%s)", S3Bucket, S3Prefix)

	ensureAppFolder()
}

// ensureAppFolder creates the app's "folder" if it doesn't exist yet. S3 has
// no real directories, so we write a zero-byte marker object at
// "<prefix>.keep". Best-effort: a failure here just logs — uploads recreate
// the prefix implicitly.
func ensureAppFolder() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	marker := S3Prefix + ".keep"
	if _, err := S3.StatObject(ctx, S3Bucket, marker, minio.StatObjectOptions{}); err == nil {
		return // folder already exists
	}
	if _, err := S3.PutObject(ctx, S3Bucket, marker, strings.NewReader(""), 0,
		minio.PutObjectOptions{ContentType: "application/octet-stream"}); err != nil {
		clientsLog.Warnf("Could not create S3 app folder %q: %v", S3Prefix, err)
		return
	}
	clientsLog.Infof("Created S3 app folder %q", S3Prefix)
}
