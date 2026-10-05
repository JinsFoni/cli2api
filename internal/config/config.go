package config

import (
	"os"
	"strconv"
	"strings"
)

type Config struct {
	Host              string
	Port              int
	ProxyAPIKey       string
	ProxyURL          string
	MaxRetryAccounts  int
	QoderHome         string
	DataDir           string
	RuntimeDir        string
	UpdateSocketPath  string
	UpdateAgentURL    string
	UpdateAgentToken  string
	UpdateGitHubToken string
}

func Load() (Config, error) {
	port := 3010
	if v := strings.TrimSpace(os.Getenv("PORT")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	home := strings.TrimSpace(os.Getenv("QODER_HOME"))
	if home == "" {
		home = "/root/.qoder"
	}
	host := strings.TrimSpace(os.Getenv("HOST"))
	if host == "" {
		host = "127.0.0.1"
	}
	maxRetryAccounts := 4
	if v := strings.TrimSpace(os.Getenv("QODER_MAX_RETRY_ACCOUNTS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			maxRetryAccounts = n
		}
	}
	if maxRetryAccounts > 64 {
		maxRetryAccounts = 64
	}
	dataDir := strings.TrimSpace(os.Getenv("QODER_DATA_DIR"))
	if dataDir == "" {
		if userHome, err := os.UserHomeDir(); err == nil {
			dataDir = userHome + "/.qoder-api-proxy"
		} else {
			dataDir = "data"
		}
	}
	runtimeDir := strings.TrimSpace(os.Getenv("QODER_RUNTIME_DIR"))
	if runtimeDir == "" {
		runtimeDir = "/tmp/cli2api-runtime"
	}
	return Config{
		Host:              host,
		Port:              port,
		ProxyAPIKey:       "",
		ProxyURL:          strings.TrimSpace(os.Getenv("QODER_PROXY_URL")),
		MaxRetryAccounts:  maxRetryAccounts,
		QoderHome:         home,
		DataDir:           dataDir,
		RuntimeDir:        runtimeDir,
		UpdateSocketPath:  firstNonEmpty(os.Getenv("UPDATE_SOCKET_PATH"), "/run/cli2api-updater/updater.sock"),
		UpdateAgentURL:    strings.TrimSpace(os.Getenv("UPDATE_AGENT_URL")),
		UpdateAgentToken:  strings.TrimSpace(os.Getenv("UPDATE_AGENT_TOKEN")),
		UpdateGitHubToken: strings.TrimSpace(os.Getenv("UPDATE_GITHUB_TOKEN")),
	}, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
