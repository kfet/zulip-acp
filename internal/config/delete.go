package config

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// DeleteScopeTopic is the only delete scope: a delete confirmation
// deletes the one topic of the archive warning.
const DeleteScopeTopic = "topic"

// Delete is the `delete` config block: a second bot whose only use is
// the delete_topic call that the delete emoji confirms.
//
// The key is read from KeyFile at startup and kept in the relay
// process only. It is never put in the agent's environment, the
// system prompt, the state dir or a log line.
type Delete struct {
	// BotEmail is the delete bot's Zulip e-mail.
	BotEmail string `json:"bot_email"`
	// KeyFile is the absolute path of a file that holds the delete
	// bot's API key. It must not be readable by group or others.
	KeyFile string `json:"key_file"`
	// Scope is what a delete may remove. Only "topic" (the default).
	Scope string `json:"scope,omitempty"`
	// OwnersOnly lets only update_owner_ids confirm a delete.
	// Unset = true.
	OwnersOnly *bool `json:"owners_only,omitempty"`
	// Emoji is the bare Zulip emoji name that confirms a delete on the
	// archive warning. Unset = DefaultDeleteEmoji.
	Emoji string `json:"emoji,omitempty"`
}

// DefaultDeleteEmoji confirms a delete when delete.emoji is unset or
// the delete block is absent.
const DefaultDeleteEmoji = "fire"

// DeleteEmoji returns the emoji that confirms a delete:
// delete.emoji, or DefaultDeleteEmoji.
func (c *Config) DeleteEmoji() string {
	if c.Delete == nil || c.Delete.Emoji == "" {
		return DefaultDeleteEmoji
	}
	return c.Delete.Emoji
}

// GetOwnersOnly reports owners_only, true when unset.
func (d *Delete) GetOwnersOnly() bool { return d.OwnersOnly == nil || *d.OwnersOnly }

func (c *Config) validateDelete() error {
	d := c.Delete
	if d == nil {
		return nil
	}
	if strings.TrimSpace(d.BotEmail) == "" {
		return fmt.Errorf("delete.bot_email is required")
	}
	if !filepath.IsAbs(d.KeyFile) {
		return fmt.Errorf("delete.key_file must be an absolute path")
	}
	if d.Scope != "" && d.Scope != DeleteScopeTopic {
		return fmt.Errorf("delete.scope %q is not supported; the only scope is %q", d.Scope, DeleteScopeTopic)
	}
	if d.GetOwnersOnly() && len(c.UpdateOwnerIDs) == 0 {
		return fmt.Errorf("delete.owners_only needs update_owner_ids: with no owner, nobody could delete")
	}
	return c.validateDeleteEmoji()
}

// validateDeleteEmoji refuses a delete.emoji that is not a bare emoji
// name or that already means something else on a relay message.
// A collision with a quick_models emoji is caught by
// validateQuickModels, which reserves the delete emoji.
func (c *Config) validateDeleteEmoji() error {
	e := c.Delete.Emoji
	if e == "" {
		return nil
	}
	if !validEmojiName(e) {
		return fmt.Errorf("delete.emoji %q must be a bare Zulip emoji name, e.g. \"bomb\"", e)
	}
	taken := append([]string{c.GetQuickModelsMenuEmoji(), c.GetQuickModelsSweepEmoji(), c.GetAckEmoji()}, reservedQuickEmojis...)
	for _, r := range taken {
		if e == r {
			return fmt.Errorf("delete.emoji :%s: is reserved for another relay control", e)
		}
	}
	return nil
}

// validEmojiName reports that s holds only the characters of a Zulip
// emoji name: lower-case letters, digits, '_', '-' and '+'.
func validEmojiName(s string) bool {
	for _, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-' || r == '+'
		if !ok {
			return false
		}
	}
	return s != ""
}

// ReadDeleteKey reads the delete bot's API key from delete.key_file.
// It refuses a file that group or others can read: the key deletes
// messages for good.
func (d *Delete) ReadDeleteKey() (string, error) {
	f, err := os.Open(d.KeyFile)
	if err != nil {
		return "", fmt.Errorf("delete.key_file: %w", err)
	}
	defer f.Close()
	fi := mustStat(f)
	if fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("delete.key_file %s has mode %#o; set it to 0600", d.KeyFile, fi.Mode().Perm())
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return "", fmt.Errorf("delete.key_file: %w", err)
	}
	key := strings.TrimSpace(string(b))
	if key == "" {
		return "", fmt.Errorf("delete.key_file %s is empty", d.KeyFile)
	}
	return key, nil
}
