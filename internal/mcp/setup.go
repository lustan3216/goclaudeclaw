// Package mcp generates or updates the .mcp.json file under the workspace
// based on the mcps config in config.json, so that `claude -p` automatically
// loads the corresponding MCP servers on startup.
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

// serverDef holds the command definition for a single MCP server.
type serverDef struct {
	Command string            `json:"command"`
	Args    []string          `json:"args"`
	Env     map[string]string `json:"env,omitempty"`
}

// mcpFile is the file structure for .mcp.json.
type mcpFile struct {
	MCPServers map[string]serverDef `json:"mcpServers"`
}

// ApplyConfig generates/updates workspace/.mcp.json based on cfg.MCPs.
// Only servers with a non-empty token (or enabled=true) are written; others are skipped.
// After writing, all npx package caches are pre-warmed in the background to avoid
// timeout on first download when Claude starts.
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

	// Puppeteer browser MCP (no token needed, just enabled)
	if mcps.Browser.Enabled {
		servers["puppeteer"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-puppeteer"},
		}
	}

	// Brave Search MCP
	if mcps.Brave.APIKey != "" {
		servers["brave-search"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "@modelcontextprotocol/server-brave-search"},
			Env:     map[string]string{"BRAVE_API_KEY": mcps.Brave.APIKey},
		}
	}

	// Gemini MCP (relies on local Gemini CLI auth, no token required)
	if mcps.Gemini.Enabled {
		servers["gemini"] = serverDef{
			Command: "npx",
			Args:    []string{"-y", "gemini-mcp-tool"},
		}
	}

	dest := filepath.Join(workspace, ".mcp.json")

	// If no servers are enabled, remove the file (if it exists)
	if len(servers) == 0 {
		if err := os.Remove(dest); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("failed to remove .mcp.json: %w", err)
		}
		slog.Info("MCP config: no enabled servers, .mcp.json removed")
		return nil
	}

	data, err := json.MarshalIndent(mcpFile{MCPServers: servers}, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to serialize .mcp.json: %w", err)
	}

	if err := os.WriteFile(dest, data, 0o644); err != nil {
		return fmt.Errorf("failed to write .mcp.json: %w", err)
	}

	slog.Info("MCP config updated", "path", dest, "servers", keys(servers))

	// Background pre-warm: download/cache all npx packages in advance to avoid
	// timeout on first download when Claude starts an MCP server
	go prewarmNpxPackages(servers)

	return nil
}

// prewarmNpxPackages concurrently runs `npx -y <package> --version` in the background
// to trigger npm download and caching, so subsequent Claude MCP server starts use the local cache.
func prewarmNpxPackages(servers map[string]serverDef) {
	for name, srv := range servers {
		if srv.Command != "npx" || len(srv.Args) < 2 {
			continue
		}
		pkg := srv.Args[1] // npx -y <package>
		go func(serverName, pkg string) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			// Use --version to trigger download; ignore output and errors (some packages don't support --version)
			cmd := exec.CommandContext(ctx, "npx", "-y", pkg, "--version")
			cmd.Env = os.Environ()
			_ = cmd.Run()
			slog.Debug("MCP package pre-warm complete", "server", serverName, "package", pkg)
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
