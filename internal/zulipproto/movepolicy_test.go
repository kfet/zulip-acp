package zulipproto

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestMoveMessageToChannel pins the wire shape of a cross-channel move:
// stream_id (never content, which the server refuses alongside it),
// change_all for a whole topic, and BOTH Notification Bot notices
// suppressed — a notice in the source topic would resurrect the very
// topic that was just archived.
func TestMoveMessageToChannel(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON("") })
	if err := newClient(t, ts).MoveMessageToChannel(context.Background(), 7, 42, "", "change_all"); err != nil {
		t.Fatalf("move: %v", err)
	}
	req := ts.requests()[0]
	if req.method != "PATCH" || req.path != "/api/v1/messages/7" {
		t.Fatalf("%s %s", req.method, req.path)
	}
	want := map[string]string{
		"stream_id":                       "42",
		"propagate_mode":                  "change_all",
		"send_notification_to_old_thread": "false",
		"send_notification_to_new_thread": "false",
	}
	for k, v := range want {
		if got := req.form.Get(k); got != v {
			t.Fatalf("form[%s] = %q, want %q", k, got, v)
		}
	}
	if _, ok := req.form["topic"]; ok {
		t.Fatalf("an unrequested topic rename was sent: %v", req.form)
	}
	if _, ok := req.form["content"]; ok {
		t.Fatalf("content must never travel with a channel move: %v", req.form)
	}
}

// TestMoveMessageToChannelRefusals: the two things the client refuses
// before spending a request.
func TestMoveMessageToChannelRefusals(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) { return 200, okJSON("") })
	c := newClient(t, ts)
	if err := c.MoveMessageToChannel(context.Background(), 7, 0, "", "change_all"); err == nil {
		t.Fatal("want an error with no destination channel")
	}
	long := strings.Repeat("x", MaxTopicLength+1)
	if err := c.MoveMessageToChannel(context.Background(), 7, 42, long, "change_all"); err == nil {
		t.Fatal("want an error on an over-long topic")
	}
	if n := len(ts.requests()); n != 0 {
		t.Fatalf("a refused move still made %d request(s)", n)
	}
	// A topic given alongside the channel IS sent: moving and renaming
	// in one edit is legal, it is only content that is not.
	if err := c.MoveMessageToChannel(context.Background(), 7, 42, "renamed", "change_all"); err != nil {
		t.Fatalf("move: %v", err)
	}
	if got := ts.requests()[0].form.Get("topic"); got != "renamed" {
		t.Fatalf("topic = %q", got)
	}
}

// TestGroupSettingShapes: a group-setting value is either a group id or
// an anonymous group object, under the same JSON key. Both must decode,
// and anything else must fail loudly rather than silently read as "no
// permission".
func TestGroupSettingShapes(t *testing.T) {
	var named GroupSetting
	if err := named.UnmarshalJSON([]byte("11")); err != nil || named.GroupID != 11 {
		t.Fatalf("named = %+v, err = %v", named, err)
	}
	var anon GroupSetting
	if err := anon.UnmarshalJSON([]byte(`{"direct_members":[3,4],"direct_subgroups":[7]}`)); err != nil {
		t.Fatalf("anon: %v", err)
	}
	if len(anon.DirectMembers) != 2 || len(anon.DirectSubgroups) != 1 || anon.GroupID != 0 {
		t.Fatalf("anon = %+v", anon)
	}
	if err := (&GroupSetting{}).UnmarshalJSON([]byte(`"nonsense"`)); err == nil {
		t.Fatal("want an error on an unknown shape")
	}
	// A null must not read as "an empty group", which would silently
	// mean "no permission" instead of "cannot tell".
	if err := (&GroupSetting{}).UnmarshalJSON([]byte(`null`)); err == nil {
		t.Fatal("want an error on a null setting")
	}
}

// TestRealmGroupSetting: reading a realm setting costs one /register
// with fetch_event_types, and the throwaway queue is deleted again.
func TestRealmGroupSetting(t *testing.T) {
	ts := newServer(t, func(r recordedReq) (int, string) {
		if r.path == "/api/v1/register" {
			return 200, okJSON(`"queue_id":"q1","` + RealmMoveBetweenChannels + `":11`)
		}
		return 200, okJSON("")
	})
	gs, err := newClient(t, ts).RealmGroupSetting(context.Background(), RealmMoveBetweenChannels)
	if err != nil || gs.GroupID != 11 {
		t.Fatalf("gs = %+v, err = %v", gs, err)
	}
	reqs := ts.requests()
	if len(reqs) != 2 || reqs[1].method != "DELETE" || reqs[1].form.Get("queue_id") != "q1" {
		t.Fatalf("the probe queue was not cleaned up: %+v", reqs)
	}
	if reqs[0].form.Get("fetch_event_types") != `["realm"]` || reqs[0].form.Get("event_types") != `[]` {
		t.Fatalf("register form = %v", reqs[0].form)
	}
}

// TestRealmGroupSettingFailures: every way the answer can be "cannot
// tell", which callers must treat as a refusal to enable anything.
func TestRealmGroupSettingFailures(t *testing.T) {
	cases := []struct {
		name string
		body string
		code int
	}{
		{name: "server error", body: `{"result":"error","msg":"nope"}`, code: 400},
		{name: "setting absent", body: okJSON(`"queue_id":"q1"`), code: 200},
		{name: "unreadable setting", body: okJSON(`"queue_id":"q1","` + RealmMoveBetweenChannels + `":"nope"`), code: 200},
		{name: "unusable queue id", body: okJSON(`"queue_id":7,"` + RealmMoveBetweenChannels + `":11`), code: 200},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newServer(t, func(r recordedReq) (int, string) {
				if r.path == "/api/v1/register" {
					return tc.code, tc.body
				}
				return 200, okJSON("")
			})
			_, err := newClient(t, ts).RealmGroupSetting(context.Background(), RealmMoveBetweenChannels)
			if tc.name == "unusable queue id" {
				// A queue id we cannot read is only litter, not a
				// failure: the setting itself came back fine.
				if err != nil {
					t.Fatalf("err = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

// TestIsUserGroupMember pins the endpoint and the parameter that makes
// it count membership through subgroups.
func TestIsUserGroupMember(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 200, okJSON(`"is_user_group_member":true`)
	})
	ok, err := newClient(t, ts).IsUserGroupMember(context.Background(), 11, 9)
	if err != nil || !ok {
		t.Fatalf("ok = %v, err = %v", ok, err)
	}
	req := ts.requests()[0]
	if req.method != "GET" || req.path != "/api/v1/user_groups/11/members/9" {
		t.Fatalf("%s %s", req.method, req.path)
	}
	if got := req.query.Get("direct_member_only"); got != "false" {
		t.Fatalf("direct_member_only = %q", got)
	}
}

// TestChannelMovePolicy drives both shapes of the group setting, every
// answer the membership lookup can give, and every shape of the time
// limit that rides along with it.
func TestChannelMovePolicy(t *testing.T) {
	const limitKey = RealmMoveBetweenChannelsLimit
	cases := []struct {
		name    string
		setting string
		// limit is the raw JSON for the limit setting, or "" to leave
		// the setting out of the snapshot entirely.
		limit  string
		member string
		// memberStatus applies to the membership endpoint only.
		memberStatus int
		role         int64
		want         bool
		wantLimit    time.Duration
		wantErr      bool
	}{
		{name: "named group, member", setting: `11`, member: okJSON(`"is_user_group_member":true`), want: true, wantLimit: 7 * 24 * time.Hour},
		{name: "named group, not a member", setting: `11`, member: okJSON(`"is_user_group_member":false`), wantLimit: 7 * 24 * time.Hour},
		{
			name:         "named group, server refuses to say",
			setting:      `11`,
			member:       `{"result":"error","msg":"unsupported for bots"}`,
			memberStatus: 400,
			wantErr:      true,
		},
		{name: "anonymous group, direct member", setting: `{"direct_members":[9],"direct_subgroups":[]}`, want: true, wantLimit: 7 * 24 * time.Hour},
		{
			name:    "anonymous group, member of a subgroup",
			setting: `{"direct_members":[1],"direct_subgroups":[7]}`,
			member:  okJSON(`"is_user_group_member":true`),
			want:    true, wantLimit: 7 * 24 * time.Hour,
		},
		{
			name:      "anonymous group, in nothing",
			setting:   `{"direct_members":[1],"direct_subgroups":[7]}`,
			member:    okJSON(`"is_user_group_member":false`),
			wantLimit: 7 * 24 * time.Hour,
		},
		{
			name:         "anonymous group, subgroup lookup fails",
			setting:      `{"direct_members":[1],"direct_subgroups":[7]}`,
			member:       `{"result":"error","msg":"boom"}`,
			memberStatus: 500,
			wantErr:      true,
		},
		{name: "no such setting", setting: "", wantErr: true},
		{name: "the group setting is unreadable", setting: `"everyone"`, wantErr: true},
		// "any time" arrives as JSON null and means unlimited — NOT
		// "cannot tell" and not zero seconds.
		{name: "no limit at all", setting: `{"direct_members":[9]}`, limit: `null`, want: true},
		{name: "the limit is not reported", setting: `{"direct_members":[9]}`, limit: "-", wantErr: true},
		{name: "the limit is not a number", setting: `{"direct_members":[9]}`, limit: `"soon"`, wantErr: true},
		{name: "the limit is negative", setting: `{"direct_members":[9]}`, limit: `-1`, wantErr: true},
		// Moderators and above are exempt from the realm's move time
		// limits, so the same 7 days reads as unlimited for them.
		{name: "a moderator is exempt", setting: `{"direct_members":[9]}`, role: RoleModerator, want: true},
		{name: "a plain member is not", setting: `{"direct_members":[9]}`, role: RoleMember, want: true, wantLimit: 7 * 24 * time.Hour},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := newServer(t, func(r recordedReq) (int, string) {
				switch {
				case r.path == "/api/v1/register":
					if tc.setting == "" {
						return 200, okJSON(`"queue_id":"q1"`)
					}
					body := `"queue_id":"q1","` + RealmMoveBetweenChannels + `":` + tc.setting
					switch tc.limit {
					case "-":
					case "":
						body += `,"` + limitKey + `":604800`
					default:
						body += `,"` + limitKey + `":` + tc.limit
					}
					return 200, okJSON(body)
				case strings.Contains(r.path, "/members/"):
					status := tc.memberStatus
					if status == 0 {
						status = 200
					}
					return status, tc.member
				}
				return 200, okJSON("")
			})
			got, err := newClient(t, ts).ChannelMovePolicy(context.Background(), User{UserID: 9, Role: tc.role})
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got.Allowed != tc.want {
				t.Fatalf("allowed = %v, want %v", got.Allowed, tc.want)
			}
			if got.Limit != tc.wantLimit {
				t.Fatalf("limit = %s, want %s", got.Limit, tc.wantLimit)
			}
		})
	}
}

// TestChannelMovePolicyUnreadableRealm: a realm snapshot that cannot
// be fetched at all is "cannot tell", which the caller turns into a
// disabled feature rather than a guess.
func TestChannelMovePolicyUnreadableRealm(t *testing.T) {
	ts := newServer(t, func(recordedReq) (int, string) {
		return 500, `{"result":"error","msg":"down"}`
	})
	if _, err := newClient(t, ts).ChannelMovePolicy(context.Background(), User{UserID: 9}); err == nil {
		t.Fatal("want an error")
	}
}

// TestMovePolicyTooOld pins the one decision the archive preflight
// makes, INCLUDING the slack held back for clock skew: a message right
// on the boundary counts as too old, because the server's clock is the
// one that decides and it is not ours.
func TestMovePolicyTooOld(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	unlimited := MovePolicy{Allowed: true}
	week := MovePolicy{Allowed: true, Limit: 7 * 24 * time.Hour}
	cases := []struct {
		name   string
		policy MovePolicy
		sent   time.Time
		want   bool
	}{
		{name: "no limit, ancient message", policy: unlimited, sent: now.Add(-10 * 365 * 24 * time.Hour)},
		{name: "well inside the limit", policy: week, sent: now.Add(-time.Hour)},
		{name: "just inside", policy: week, sent: now.Add(-(7*24*time.Hour - 2*time.Minute))},
		{name: "inside, but within the skew slack", policy: week, sent: now.Add(-(7*24*time.Hour - 30*time.Second)), want: true},
		{name: "beyond it", policy: week, sent: now.Add(-8 * 24 * time.Hour), want: true},
		// A limit tighter than the slack must not read as "nothing is
		// movable": the window clamps at zero, so a message sent right
		// now is still young enough.
		{name: "a limit inside the slack", policy: MovePolicy{Allowed: true, Limit: 30 * time.Second}, sent: now},
		{name: "a limit inside the slack, just inside it", policy: MovePolicy{Allowed: true, Limit: 30 * time.Second}, sent: now.Add(-20 * time.Second)},
		{name: "a limit inside the slack, beyond it", policy: MovePolicy{Allowed: true, Limit: 30 * time.Second}, sent: now.Add(-40 * time.Second), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.policy.TooOld(tc.sent, now); got != tc.want {
				t.Fatalf("TooOld = %v, want %v", got, tc.want)
			}
		})
	}
}
