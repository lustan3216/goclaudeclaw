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
	Workspace string      `json:"workspace"`
	Bots      []wizardBot `json:"bots"`
	Security  wizardSec   `json:"security"`
}

type wizardBot struct {
	Name         string  `json:"name"`
	Token        string  `json:"token"`
	AllowedUsers []int64 `json:"allowed_users"`
}

type wizardSec struct {
	Level string `json:"level"`
}

// Run runs the interactive setup wizard and writes the resulting config to configPath.
// Only asks for the bot token — all other settings can be configured via Telegram after first launch.
func Run(configPath string) error {
	r := bufio.NewReader(os.Stdin)

	fmt.Println()
	fmt.Println("⚡ Welcome to claudeclaw — one step to get started.")
	fmt.Println("   Don't have a bot? Message @BotFather on Telegram → /newbot")
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

	// ── Build minimal config ──────────────────────────────────────────────
	cfg := wizardConfig{
		Workspace: ".",
		Bots: []wizardBot{
			{
				Name:         "main",
				Token:        token,
				AllowedUsers: []int64{}, // empty — first sender becomes owner automatically
			},
		},
		Security: wizardSec{Level: "moderate"},
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
	fmt.Printf("Starting @%s — message it on Telegram to finish setup.\n", botUsername)
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
