package main

import (
	"log"
	"net/http"
	"os"
	"path/filepath"

	"goblog/site"
)

func main() {
	// 基本配置，可通过环境变量覆盖
	configPath := getenv("CONFIG_PATH", "config.json")
	outputDir := getenv("OUTPUT_DIR", "public")
	addr := getenv("LISTEN_ADDR", ":8080")
	webhookPath := getenv("WEBHOOK_PATH", "/webhook")
	webhookToken := os.Getenv("WEBHOOK_TOKEN") // 可选

	// 首次启动时构建一次
	if err := site.BuildSite(configPath, outputDir); err != nil {
		log.Fatalf("首次构建站点失败: %v", err)
	}
	log.Printf("初始构建完成，配置文件=%s，输出目录=%s", configPath, outputDir)

	mux := http.NewServeMux()

	// 静态文件服务
	fs := http.FileServer(http.Dir(outputDir))
	mux.Handle("/", fs)

	// webhook 接口：用于 Git Hook 或 Git 托管平台的 Webhook 触发
	mux.HandleFunc(webhookPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// 简单 token 校验（可在 git hook 脚本或 webhook 配置中用 Header 传入）
		if webhookToken != "" {
			if r.Header.Get("X-Webhook-Token") != webhookToken {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
		}

		if err := site.BuildSite(configPath, outputDir); err != nil {
			log.Printf("收到 webhook 触发构建失败: %v", err)
			http.Error(w, "build failed", http.StatusInternalServerError)
			return
		}
		log.Printf("收到 webhook，重新构建完成")

		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})

	// 简单的健康检查
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})

	absOutput, _ := filepath.Abs(outputDir)
	log.Printf("服务器启动中，监听地址 http://localhost%s ，静态目录=%s", addr, absOutput)

	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatalf("服务器退出: %v", err)
	}
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

