package conversation

import (
	"fmt"
	"strings"
)

const maxChatServerToolProgress = 64

type serverToolProgress struct {
	Kind     string
	Detail   string
	Started  bool
	Finished bool
}

// isChatServerToolItem identifies tools that the upstream executes itself.
// They are represented as reasoning_content progress instead of tool_calls,
// because emitting tool_calls would incorrectly ask the downstream client to
// execute an already completed server-side operation.
func isChatServerToolItem(item responseItem) bool {
	switch strings.ToLower(strings.TrimSpace(item.Type)) {
	case "web_search_call", "x_search_call", "code_interpreter_call":
		return true
	default:
		return false
	}
}

func (c *streamConverter) chatServerToolUpdate(item responseItem, outputIndex int, terminal bool) error {
	kind := strings.ToLower(strings.TrimSpace(item.Type))
	if !isChatServerToolItem(item) {
		return nil
	}
	key := strings.TrimSpace(item.ID)
	if key == "" {
		key = fmt.Sprintf("%s:%d", kind, outputIndex)
	}
	state, exists := c.serverToolProgress[key]
	if !exists && len(c.serverToolProgress) >= maxChatServerToolProgress {
		return nil
	}
	if state.Kind == "" {
		state.Kind = kind
	}
	if detail := chatServerToolDetail(item); detail != "" {
		state.Detail = detail
	}

	if !state.Started {
		state.Started = true
		c.serverToolProgress[key] = state
		if err := c.chatDelta(map[string]any{"reasoning_content": formatChatServerToolProgress(state, false, false)}); err != nil {
			return err
		}
	}
	if !terminal || state.Finished {
		return nil
	}
	state.Finished = true
	c.serverToolProgress[key] = state
	status := strings.ToLower(strings.TrimSpace(item.Status))
	failed := status == "failed" || status == "incomplete"
	return c.chatDelta(map[string]any{"reasoning_content": formatChatServerToolProgress(state, true, failed)})
}

func chatServerToolDetail(item responseItem) string {
	if item.Action == nil {
		return ""
	}
	query, _ := item.Action["query"].(string)
	query = strings.Join(strings.Fields(query), " ")
	return truncateRunes(query, 512)
}

func formatChatServerToolProgress(state serverToolProgress, terminal, failed bool) string {
	label := "Server tool"
	switch state.Kind {
	case "web_search_call":
		label = "Web search"
	case "x_search_call":
		label = "X search"
	case "code_interpreter_call":
		label = "Code interpreter"
	}
	if !terminal {
		if state.Detail != "" {
			return fmt.Sprintf("🔎 %s: %s\n", label, state.Detail)
		}
		return fmt.Sprintf("🔎 %s started\n", label)
	}
	if failed {
		return fmt.Sprintf("⚠ %s failed\n", label)
	}
	return fmt.Sprintf("✓ %s completed\n", label)
}
