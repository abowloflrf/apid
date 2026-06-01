// Package config 负责从环境变量读取运行配置。
package config

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	// Listen 是本地监听地址，如 ":8080"。
	Listen string
	// UpstreamBaseURL 是远程 Chat Completions 服务的基础地址，
	// 例如 "https://api.openai.com/v1"。
	UpstreamBaseURL string
	// UpstreamAPIKey 是访问上游用的 API Key。
	// 若为空，则透传客户端请求里的 Authorization 头。
	UpstreamAPIKey string
	// UpstreamModel 是转发给上游时实际使用的模型名。
	// 非空时覆盖客户端请求里的 model；为空则原样透传客户端的 model。
	UpstreamModel string
	// TraceDir 非空时开启 TRACE 落盘：每条请求体落盘到该目录。
	TraceDir string
	// DB 是项目通用 SQLite 数据库文件路径；空表示不启用。
	// 当前用于 stats 收集每条请求的指标，后续可加新表（配置 / 审计 / 限流等）。
	DB string
}

// Load 从环境变量加载配置，未设置时使用默认值。
// 启动时先尝试加载工作目录下的 .env 文件（可用 APID_ENV_FILE 指定其他路径）。
// 遵循 .env 约定：已存在的真实环境变量优先，不会被 .env 覆盖。
func Load() Config {
	envFile := os.Getenv("APID_ENV_FILE")
	if envFile == "" {
		envFile = ".env"
	}
	// godotenv.Load 默认不覆盖已存在的环境变量；文件不存在时返回错误，忽略即可。
	_ = godotenv.Load(envFile)

	// TRACE 落盘目录：显式 APID_TRACE_DIR 优先；否则当 APID_TRACE 为真时用 ./logs。
	traceDir := os.Getenv("APID_TRACE_DIR")
	if traceDir == "" && truthy(os.Getenv("APID_TRACE")) {
		traceDir = "./logs"
	}

	return Config{
		Listen:          env("APID_LISTEN", ":8080"),
		UpstreamBaseURL: env("APID_UPSTREAM_BASE_URL", "https://api.openai.com/v1"),
		UpstreamAPIKey:  env("APID_UPSTREAM_API_KEY", ""),
		UpstreamModel:   env("APID_UPSTREAM_MODEL", ""),
		TraceDir:        traceDir,
		DB:              env("APID_DB", ""),
	}
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func truthy(v string) bool {
	switch v {
	case "1", "true", "TRUE", "yes", "on":
		return true
	default:
		return false
	}
}
