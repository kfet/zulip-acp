package zulipmcp

import (
	"strings"
	"testing"
)

// Regression: an agent put its question only in the reason and the user
// never saw it. The prompt must say the reason is not posted to the topic.
func TestTopicStateSaysReasonIsInboxOnly(t *testing.T) {
	if !strings.Contains(toolText(ToolTopicState), "never in the topic") {
		t.Fatal("description must say the reason is not shown in the topic")
	}
	tool := (&Tools{}).topicStateTool()
	props := tool.Schema["properties"].(map[string]any)
	d := props["reason"].(map[string]any)["description"].(string)
	if !strings.Contains(d, "does not see it in the topic") {
		t.Fatalf("reason description: %q", d)
	}
}
