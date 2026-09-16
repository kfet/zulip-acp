package zulipproto

import "testing"

// TestPollContentIsTheSlashCommand pins the MEASURED creation path. A
// bot CANNOT attach a poll as widget_content — POST /messages with
// widget_type "poll" is refused 400 "Widgets: unknown widget type:
// poll" — so a poll is made the way a human makes one, by sending the
// `/poll` slash command as the message body and letting Zulip's own
// markdown processor build the widget.
func TestPollContentIsTheSlashCommand(t *testing.T) {
	got := PollContent("Model?", []string{"one", "two"})
	want := "/poll Model?\none\ntwo"
	if got != want {
		t.Fatalf("PollContent = %q, want %q", got, want)
	}
}

// TestPollContentFlattensNewlines: a newline is what separates the
// question from the options, so one embedded in either would silently
// split into a poll entry nobody wrote.
func TestPollContentFlattensNewlines(t *testing.T) {
	got := PollContent("a\nb", []string{"c\r\nd"})
	if want := "/poll a b\nc  d"; got != want {
		t.Fatalf("PollContent = %q, want %q", got, want)
	}
}

// TestAnEmptyPollIsNotSent: a poll nobody can vote in is not a
// degraded control, it is a broken one — and sending it would leave a
// literal "/poll" line in the topic for no gain.
func TestAnEmptyPollIsNotSent(t *testing.T) {
	if got := PollContent("Model?", nil); got != "" {
		t.Fatalf("PollContent with no options = %q", got)
	}
}

// TestParseVote pins the MEASURED wire shape. The key for an option
// the poll SHIPPED WITH is "canned,<index>" — not "<sender_id>,<index>",
// which is what a participant-ADDED option is keyed by and what an
// earlier note in this repo wrongly claimed, having derived it from a
// synthetic POST /api/v1/submessage that the server echoed untouched.
func TestParseVote(t *testing.T) {
	for _, tc := range []struct {
		name    string
		msgType string
		content string
		want    Vote
		ok      bool
	}{
		{"a vote", WidgetMsgType, `{"type":"vote","key":"canned,0","vote":1}`, Vote{Option: 0, Up: true}, true},
		{"a later option", WidgetMsgType, `{"type":"vote","key":"canned,12","vote":1}`, Vote{Option: 12, Up: true}, true},
		{"an un-vote", WidgetMsgType, `{"type":"vote","key":"canned,1","vote":-1}`, Vote{Option: 1}, true},
		{"a zero vote", WidgetMsgType, `{"type":"vote","key":"canned,1","vote":0}`, Vote{Option: 1}, true},
		// A participant added this option, so it is keyed by their
		// user id and names no index the relay posted.
		{"a participant's own option", WidgetMsgType, `{"type":"vote","key":"9,1","vote":1}`, Vote{}, false},
		{"adding an option", WidgetMsgType, `{"type":"new_option","idx":1,"option":"mine"}`, Vote{}, false},
		{"editing the question", WidgetMsgType, `{"type":"question","question":"why"}`, Vote{}, false},
		{"another widget", "todo", `{"type":"vote","key":"canned,0","vote":1}`, Vote{}, false},
		{"not JSON", WidgetMsgType, `nonsense`, Vote{}, false},
		{"no key", WidgetMsgType, `{"type":"vote","vote":1}`, Vote{}, false},
		{"an empty index", WidgetMsgType, `{"type":"vote","key":"canned,","vote":1}`, Vote{}, false},
		// strconv.Atoi would take all three of these. A real client
		// sends a run of ASCII digits and nothing else, and an index
		// derived from anything but what was on the wire is a vote for
		// a model the voter did not pick.
		{"a signed index", WidgetMsgType, `{"type":"vote","key":"canned,+1","vote":1}`, Vote{}, false},
		{"a negative index", WidgetMsgType, `{"type":"vote","key":"canned,-1","vote":1}`, Vote{}, false},
		{"a padded index", WidgetMsgType, `{"type":"vote","key":"canned, 1","vote":1}`, Vote{}, false},
		// Nothing the relay posts has an index this large, and the
		// bound is also what stops a hostile key overflowing the
		// accumulator.
		{"an absurd index", WidgetMsgType, `{"type":"vote","key":"canned,99999999999999999999","vote":1}`, Vote{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := ParseVote(tc.msgType, tc.content)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("ParseVote(%q, %q) = %+v,%v, want %+v,%v", tc.msgType, tc.content, got, ok, tc.want, tc.ok)
			}
		})
	}
}
