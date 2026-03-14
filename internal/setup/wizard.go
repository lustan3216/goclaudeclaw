// Package setup provides an interactive first-run wizard that walks the user
// through creating a config.json before the daemon starts.
package setup

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// telegramMeResponse is a minimal subset of the Telegram getMe response.
type telegramMeResponse struct {
	OK     bool `json:"ok"`
	Result struct {
		Username  string `json:"username"`
		FirstName string `json:"first_name"`
	} `json:"result"`
}

// wizardConfig is the shape written to config.json.
type wizardConfig struct {
	Workspace string        `json:"workspace"`
	Bots      []wizardBot   `json:"bots"`
	Security  wizardSec     `json:"security"`
	MCPs      *wizardMCPs   `json:"mcps,omitempty"`
}

type wizardBot struct {
	Name         string  `json:"name"`
	Token        string  `json:"token"`
	AllowedUsers []int64 `json:"allowed_users"`
	DebounceMs   int     `json:"debounce_ms"`
	OpenAIAPIKey string  `json:"openai_api_key,omitempty"`
}

type wizardSec struct {
	Level string `json:"level"`
}

type wizardMCPs struct {
	GitHub  *wizardGitHub  `json:"github,omitempty"`
	Notion  *wizardNotion  `json:"notion,omitempty"`
}

type wizardGitHub struct {
	Token string `json:"token"`
}

type wizardNotion struct {
	Token string `json:"token"`
}

// Run runs the interactive setup wizard and writes the resulting config to configPath.
// Returns the path written so main can pass it to the daemon.
func Run(configPath string) error {
	r := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("⚡ Welcome to claudeclaw — let's get you set up.")
	fmt.Println("   This will create your config.json. Takes about a minute.")
	fmt.Println()

	// ── Step 1: Telegram Bot Token ────────────────────────────────────────
	fmt.Println("Step 1/4 — Telegram Bot Token")
	fmt.Println("  Don't have a bot? Message @BotFather on Telegram → /newbot")
	fmt.Println()

	var token string
	var botUsername string
	for {
		token = prompt(r, "  Bot token: ")
		if token == "" {
			fmt.Println("  Token is required.")
			continue
		}
		fmt.Print("  Verifying token... ")
		username, err := validateToken(token)
		if err != nil {
			fmt.Printf("✗ %v\n  Please check the token and try again.\n\n", err)
			continue
		}
		botUsername = username
		fmt.Printf("✓ Connected as @%s\n\n", botUsername)
		break
	}

	// ── Step 2: Workspace ─────────────────────────────────────────────────
	fmt.Println("Step 2/4 — Workspace path")
	fmt.Println("  The directory Claude Code will have access to.")
	cwd, _ := os.Getwd()
	fmt.Printf("  Press Enter to use the current directory: %s\n", cwd)
	fmt.Println()

	workspace := prompt(r, "  Workspace [.]: ")
	if workspace == "" {
		workspace = "."
	}
	// Expand ~ if provided
	if strings.HasPrefix(workspace, "~/") {
		home, _ := os.UserHomeDir()
		workspace = filepath.Join(home, workspace[2:])
	}
	fmt.Println()

	// ── Step 3: Security level ────────────────────────────────────────────
	fmt.Println("Step 3/4 — Security level")
	fmt.Println("  moderate     — most operations auto-approved (recommended)")
	fmt.Println("  strict       — confirms every tool call")
	fmt.Println("  unrestricted — no confirmations, full access")
	fmt.Println()

	secLevel := promptDefault(r, "  Security level [moderate]: ", "moderate")
	secLevel = strings.ToLower(strings.TrimSpace(secLevel))
	if secLevel != "moderate" && secLevel != "strict" && secLevel != "unrestricted" && secLevel != "locked" {
		fmt.Printf("  Unknown level %q, defaulting to moderate.\n", secLevel)
		secLevel = "moderate"
	}
	fmt.Println()

	// ── Step 4: Optional tokens ───────────────────────────────────────────
	fmt.Println("Step 4/4 — Optional integrations (press Enter to skip any)")
	fmt.Println()

	githubToken := prompt(r, "  GitHub token (for repo/PR/issue access): ")
	notionToken := prompt(r, "  Notion token (for reading/writing pages): ")
	openaiKey   := prompt(r, "  OpenAI API key (for voice message transcription): ")
	fmt.Println()

	// ── Build config ──────────────────────────────────────────────────────
	cfg := wizardConfig{
		Workspace: workspace,
		Bots: []wizardBot{
			{
				Name:         "main",
				Token:        token,
				AllowedUsers: []int64{}, // 空列表 — 第一个发消息的 Telegram 用户自动成为 owner
				DebounceMs:   1500,
				OpenAIAPIKey: openaiKey,
			},
		},
		Security: wizardSec{Level: secLevel},
	}

	if githubToken != "" || notionToken != "" {
		cfg.MCPs = &wizardMCPs{}
		if githubToken != "" {
			cfg.MCPs.GitHub = &wizardGitHub{Token: githubToken}
		}
		if notionToken != "" {
			cfg.MCPs.Notion = &wizardNotion{Token: notionToken}
		}
	}

	// ── Write config.json ─────────────────────────────────────────────────
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to encode config: %w", err)
	}
	if err := os.WriteFile(configPath, data, 0600); err != nil {
		return fmt.Errorf("failed to write %s: %w", configPath, err)
	}

	fmt.Printf("✓ Config saved to %s\n", configPath)
	fmt.Println()
	fmt.Printf("Starting claudeclaw with @%s...\n", botUsername)
	fmt.Println()

	return nil
}

// NeedsSetup returns true if configPath does not exist.
func NeedsSetup(configPath string) bool {
	_, err := os.Stat(configPath)
	return os.IsNotExist(err)
}

// validateToken calls the Telegram Bot API getMe endpoint and returns the bot's username.
func validateToken(token string) (string, error) {
	url := "https://api.telegram.org/bot" + token + "/getMe"
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("could not reach Telegram API: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var me telegramMeResponse
	if err := json.Unmarshal(body, &me); err != nil || !me.OK {
		return "", fmt.Errorf("invalid token")
	}
	return me.Result.Username, nil
}

func prompt(r *bufio.Reader, label string) string {
	fmt.Print(label)
	line, _ := r.ReadString('\n')
	return strings.TrimSpace(line)
}

func promptDefault(r *bufio.Reader, label, def string) string {
	v := prompt(r, label)
	if v == "" {
		return def
	}
	return v
}
