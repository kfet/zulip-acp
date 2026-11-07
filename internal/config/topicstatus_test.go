package config

import (
	"strings"
	"testing"
)

// TestTopicStatusParsing covers the three modes and the old boolean
// form, which must keep loading.
func TestTopicStatusParsing(t *testing.T) {
	for body, want := range map[string]TopicStatusMode{
		`{}`:                         TopicStatusEmoji,
		`{"topic_status":true}`:      TopicStatusEmoji,
		`{"topic_status":false}`:     TopicStatusOff,
		`{"topic_status":"emoji"}`:   TopicStatusEmoji,
		`{"topic_status":"resolve"}`: TopicStatusResolve,
		`{"topic_status":"off"}`:     TopicStatusOff,
	} {
		cfg, err := Load(writeConfig(t, body))
		if err != nil {
			t.Fatalf("%s: %v", body, err)
		}
		if got := cfg.GetTopicStatus(); got != want {
			t.Errorf("%s: mode = %q, want %q", body, got, want)
		}
	}
}

func TestTopicStatusRefusesJunk(t *testing.T) {
	for _, body := range []string{
		`{"topic_status":"loud"}`,
		`{"topic_status":3}`,
		`{"status_busy":""}`,
		`{"status_busy":"a b"}`,
		`{"status_busy":"abcde"}`,
		`{"status_question":"✔"}`,
		`{"status_busy":"x","status_question":"x"}`,
	} {
		if _, err := Load(writeConfig(t, body)); err == nil {
			t.Errorf("%s: want an error", body)
		}
	}
	_, err := Load(writeConfig(t, `{"topic_status":"loud"}`))
	if err == nil || !strings.Contains(err.Error(), "loud") {
		t.Fatalf("err = %v", err)
	}
}

func TestStatusGlyphs(t *testing.T) {
	c := &Config{}
	if c.GetStatusBusy() != DefaultStatusBusy || c.GetStatusQuestion() != DefaultStatusQuestion {
		t.Fatal("defaults")
	}
	cfg, err := Load(writeConfig(t, `{"status_busy":"⏳","status_question":"?"}`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.GetStatusBusy() != "⏳" || cfg.GetStatusQuestion() != "?" {
		t.Fatalf("glyphs = %q %q", cfg.GetStatusBusy(), cfg.GetStatusQuestion())
	}
}
