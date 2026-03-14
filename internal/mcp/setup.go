// Package mcp 负责根据 config.json 中的 mcps 配置，
// 自动生成或更新 workspace 下的 .mcp.json 文件，
// 让 claude -p 启动时自动加载对应的 MCP 服务器。
package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/lustan3216/claudeclaw/internal/config"
)

// serverDef 单个 MCP 服务器的命令定义。
type serverDef struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// mcpFile 是 .mcp.json 的文件结构。
type mcpFile struct {
	MCPServers map[string]serverDef `json:"mcpServers"`
}

// ApplyConfig 根据 cfg.MCPs 生成/更新 workspace/.mcp.json。
// 只写入 token 不为空（或 enabled=true）的服务器，其余跳过。
// 写入完成后会在后台 pre-warm 所有 npx 包的缓存，避免 Claude 启动时因首次下载超时。
func ApplyConfig(workspace string, mcps config.MCPsConfig) error {
	servers := make(map[string]serverDef)

	// GitHub MCP
	if mcps.GitHub.Token != "" {
		servers["github"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-github"},
			Env:     map[string]string{"GITHUB_TOKEN": mcps.GitHub.Token},
		}
	}

	// Notion MCP
	if mcps.Notion.Token != "" {
		servers["notion"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "@notionhq/notion-mcp-server"},
			Env: map[string]string{
				"OPENAPI_MCP_HEADERS": fmt.Sprintf(
					`{"Authorization":"Bearer %s","Notion-Version":"2022-06-28"}`,
					mcps.Notion.Token,
				),
			},
		}
	}

	// Puppeteer 浏览器 MCP（不需要 token，只需 enabled）
	if mcps.Browser.Enabled {
		servers["puppeteer"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-puppeteer"},
		}
	}

	// Brave 搜索 MCP
	if mcps.Brave.APIKey != "" {
		servers["brave-search"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-brave-search"},
			Env:     map[string]string{"BRAVE_API_KEY": mcps.Brave.APIKey},
		}
	}

	// Gemini MCP（依赖本机 Gemini CLI 认证，无需 token）
	if mcps.Gemini.Enabled {
		servers["gemini"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "gemini-mcp-tool"},
		}
	}

	dest := filepath.Join(workspace, ".mcp.json")

	// 如果没有任何启用的服务器，删除文件（如果存在）
	if len(servers) == 0 {
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("删除 .mcp.json 失败: %w", err)
		}
		slog.Info("MCP 配置：无启用服务器，已清理 .mcp.json")
		return nil
	}

	data, err := json.MarshalIndent(mcpFile{MCPServers: servers}, "", "  ")
	if err != nil {
		return fmt.Errorf("序列化 .mcp.json 失败: %w", err)
	}

	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("写入 .mcp.json 失败: %w", err)
	}

	slog.Info("MCP 配置已更新", "path", dest, "servers", keys(servers))

	// 后台 pre-warm：提前下载/缓存所有 npx 包，避免 Claude 启动 MCP 时因首次下载超时
	go prewarmNpxPackages(servers)

	return nil
}

// prewarmNpxPackages 后台并发运行 `npx -y <package> --version`，
// 触发 npm 下载并缓存，后续 Claude 启动 MCP server 时直接使用本地缓存。
func prewarmNpxPackages(servers map[string]serverDef) {
	for name, srv := range servers {
		if srv.Command != "npx" || len(srv.Args) < 2 {
			continue
		}
		pkg := srv.Args[1] // npx -y <package>
		go func(serverName, pkg string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			// 用 --version 触发下载，忽略输出和错误（有些包不支持 --version 也没关系）
			cmd := exec.CommandContext(ctx, "npx", "-y", pkg, "--version")
			cmd.Env = os.Environ()
			_ = cmd.Run()
			slog.Debug("MCP package pre-warm 完成", "server", serverName, "package", pkg)
		}(name, pkg)
	}
}

func keys(m map[string]serverDef) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
