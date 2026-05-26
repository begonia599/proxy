// config.go: .env 解析 + 配置默认值
package main

import (
	"log"
	"os"
	"strings"
)

type Config struct {
	RealKey    string
	AdminToken string // /admin/* 接口的 bearer
	HideCC     bool
}

func loadConfig() *Config {
	cfg := &Config{
		AdminToken: "admin-secret-change-me",
		HideCC:     false,
	}
	// 先找当前目录的 .env（标准位置）；找不到再退到上一级（兼容旧布局）
	data, err := os.ReadFile(".env")
	if err != nil {
		data, err = os.ReadFile("../.env")
		if err != nil {
			log.Fatalf("read .env: %v (looked in . and ..)", err)
		}
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "key=") {
			cfg.RealKey = strings.TrimPrefix(line, "key=")
		}
		if strings.HasPrefix(line, "admin_token=") {
			cfg.AdminToken = strings.TrimPrefix(line, "admin_token=")
		}
	}
	if cfg.RealKey == "" {
		log.Fatal("real key not found in .env")
	}
	return cfg
}
