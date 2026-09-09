// This file answers ONE question: may this bot move a topic from one
// channel to another?
//
// It exists because the alternative is finding out at the moment a
// human taps the archive emoji — when the answer is already a posted
// warning, a cancelled turn and a retired conversation. A permission
// that gates a destructive action must be checked when the relay
// starts, not when the action runs.
//
// The answer lives in a realm setting, and since Zulip 10.0 (feature
// level 310) that setting is a GROUP-SETTING VALUE — either a user
// group id, or an anonymous group of direct members and subgroups —
// replacing the old move_messages_between_streams_policy integer. So
// reading it is two steps: fetch the realm setting, then ask whether
// the bot is in the group it names.
//
// Both steps can fail on a server that is too old or too locked down
// (querying a BOT's group membership needs Zulip 12.0 / feature level
// 458). That is reported as an error rather than guessed at: the
// caller's correct response to "I cannot tell" is to disable the
// feature and say so in the log.
package zulipproto

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// RealmMoveBetweenChannels is the realm setting naming the users
// allowed to move messages between channels.
const RealmMoveBetweenChannels = "realm_can_move_messages_between_channels_group"

// RealmMoveBetweenChannelsLimit is the realm setting bounding how OLD
// a message may be and still be moved to another channel. It arrives
// as a number of seconds, or JSON null for "any time".
//
// It is the second half of the same permission: the group setting says
// who may move, this says how far back they may reach. A relay that
// reads only the first one discovers the second by half-completing an
// archive.
const RealmMoveBetweenChannelsLimit = "realm_move_messages_between_streams_limit_seconds"

// GroupSetting is Zulip's group-setting value: a permission expressed
// either as a single user-group id, or inline as an anonymous group of
// direct members and direct subgroups.
//
// The two shapes arrive under the same JSON key, which is why this
// needs a custom unmarshaller rather than a typed field: a struct would
// fail to decode the integer form and take the whole response with it.
type GroupSetting struct {
	// GroupID is the named group's id, or 0 for an anonymous group.
	GroupID int64
	// DirectMembers and DirectSubgroups are populated for the
	// anonymous form only.
	DirectMembers   []int64
	DirectSubgroups []int64
}

// UnmarshalJSON accepts both shapes of a group-setting value.
//
// A null is refused rather than read as an empty group: "the server
// said nothing" and "the group is empty" are the same bytes to a
// zero value, and only one of them means the caller may proceed.
func (g *GroupSetting) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return fmt.Errorf("zulip: group-setting value is null")
	}
	var id int64
	if err := json.Unmarshal(b, &id); err == nil {
		g.GroupID = id
		return nil
	}
	var anon struct {
		DirectMembers   []int64 `json:"direct_members"`
		DirectSubgroups []int64 `json:"direct_subgroups"`
	}
	if err := json.Unmarshal(b, &anon); err != nil {
		return fmt.Errorf("zulip: group-setting value is neither a group id nor a group object: %w", err)
	}
	g.DirectMembers, g.DirectSubgroups = anon.DirectMembers, anon.DirectSubgroups
	return nil
}

// realmSnapshot fetches the realm's settings as raw JSON.
//
// Zulip has no "GET /realm" for non-administrators, so the only way to
// read a realm setting is POST /register with `fetch_event_types`.
// That creates an event queue as a side effect, which is deleted
// immediately — the server would garbage-collect it in ten minutes
// anyway, but leaving one behind for a startup probe is litter.
func (c *Client) realmSnapshot(ctx context.Context) (map[string]json.RawMessage, error) {
	form := url.Values{
		// No live event types: this queue exists only to carry the
		// one-shot realm snapshot back.
		"event_types":       {mustJSON([]string{})},
		"fetch_event_types": {mustJSON([]string{"realm"})},
	}
	var resp map[string]json.RawMessage
	if err := c.do(ctx, http.MethodPost, "/register", nil, form, &resp); err != nil {
		return nil, fmt.Errorf("zulip: reading realm settings: %w", err)
	}
	if raw, ok := resp["queue_id"]; ok {
		var qid string
		if err := json.Unmarshal(raw, &qid); err == nil && qid != "" {
			// Best effort: a probe that cannot tidy up is still a
			// successful probe.
			_ = c.DeleteQueue(ctx, qid)
		}
	}
	return resp, nil
}

// RealmGroupSetting reads one group-setting realm setting by its
// register-response name (e.g. RealmMoveBetweenChannels), out of the
// realm snapshot.
func (c *Client) RealmGroupSetting(ctx context.Context, name string) (GroupSetting, error) {
	resp, err := c.realmSnapshot(ctx)
	if err != nil {
		return GroupSetting{}, err
	}
	raw, ok := resp[name]
	if !ok {
		return GroupSetting{}, fmt.Errorf("zulip: this server does not report %s", name)
	}
	var gs GroupSetting
	if err := json.Unmarshal(raw, &gs); err != nil {
		return GroupSetting{}, err
	}
	return gs, nil
}

// groupSetting decodes one group-setting value out of a realm
// snapshot, for a caller that already holds one. A setting the server
// does not report is an error: this package never guesses at a
// permission.
func groupSetting(snap map[string]json.RawMessage, name string) (GroupSetting, error) {
	raw, ok := snap[name]
	if !ok {
		return GroupSetting{}, fmt.Errorf("zulip: this server does not report %s", name)
	}
	var gs GroupSetting
	if err := json.Unmarshal(raw, &gs); err != nil {
		return GroupSetting{}, err
	}
	return gs, nil
}

// IsUserGroupMember reports whether userID is in the user group with
// the given id, counting membership through subgroups.
//
// Bot users are only queryable here from Zulip 12.0 (feature level
// 458); an older server answers with an error, which is returned
// rather than converted into "no".
func (c *Client) IsUserGroupMember(ctx context.Context, groupID, userID int64) (bool, error) {
	q := url.Values{"direct_member_only": {"false"}}
	path := "/user_groups/" + strconv.FormatInt(groupID, 10) + "/members/" + strconv.FormatInt(userID, 10)
	var resp struct {
		IsMember bool `json:"is_user_group_member"`
	}
	if err := c.do(ctx, http.MethodGet, path, q, nil, &resp); err != nil {
		return false, err
	}
	return resp.IsMember, nil
}

// MovePolicy is the realm's complete answer about moving a topic to
// another channel: WHETHER a user may, and how far BACK.
type MovePolicy struct {
	// Allowed reports membership of
	// can_move_messages_between_channels_group.
	Allowed bool
	// Limit is how old the oldest message being moved may be. Zero
	// means UNLIMITED — either the realm sets no limit, or the user is
	// exempt from it. A caller must not read zero as "nothing may be
	// moved"; see MovePolicy.TooOld.
	Limit time.Duration
}

// TooOld reports whether a message sent at `sent` is beyond the move
// limit as of `now`.
//
// The slack is deliberate: the limit is enforced by the SERVER against
// its own clock, so a decision made on ours has to leave room for the
// two disagreeing. Erring towards "too old" costs a refusal the user
// can act on; erring the other way costs a half-completed archive,
// which is the bug this exists to prevent.
func (p MovePolicy) TooOld(sent, now time.Time) bool {
	if p.Limit <= 0 {
		return false
	}
	return now.Sub(sent) >= p.Limit-moveLimitSlack
}

// moveLimitSlack is the margin held back from the realm's limit to
// absorb clock skew between this host and the Zulip server.
const moveLimitSlack = time.Minute

// ChannelMovePolicy reports whether user — in practice the relay's own
// bot — may move messages between channels, and the age limit that
// applies when it does.
//
// An error means "cannot tell", which is NOT the same as "no": the
// caller decides what to do about an unknown answer, and for a
// destructive feature the only safe choice is to stay off.
//
// The time limit is read here rather than discovered mid-action for a
// concrete reason: a move refused by
// move_messages_between_streams_limit_seconds fails at the LAST step
// of the archive, after the conversation has already been ended and
// retired. Knowing the limit up front lets the caller settle the
// question against a topic's oldest message BEFORE it touches
// anything.
//
// Two ways the limit does not apply: the realm sets none (the setting
// arrives as JSON null), or the user is a moderator or above, who are
// exempt from message-move time limits. Both are reported as a zero
// Limit.
func (c *Client) ChannelMovePolicy(ctx context.Context, user User) (MovePolicy, error) {
	snap, err := c.realmSnapshot(ctx)
	if err != nil {
		return MovePolicy{}, err
	}
	gs, err := groupSetting(snap, RealmMoveBetweenChannels)
	if err != nil {
		return MovePolicy{}, err
	}
	allowed, err := c.inGroup(ctx, gs, user.UserID)
	if err != nil {
		return MovePolicy{}, err
	}
	limit, err := moveLimit(snap, RealmMoveBetweenChannelsLimit)
	if err != nil {
		return MovePolicy{}, err
	}
	if user.Role != 0 && user.Role <= RoleModerator {
		// Moderators, administrators and owners are exempt. A zero
		// Role is a server that did not report one, which is not
		// evidence of privilege.
		limit = 0
	}
	return MovePolicy{Allowed: allowed, Limit: limit}, nil
}

// inGroup resolves a group-setting value to a yes/no for one user,
// following subgroups.
func (c *Client) inGroup(ctx context.Context, gs GroupSetting, userID int64) (bool, error) {
	if gs.GroupID != 0 {
		return c.IsUserGroupMember(ctx, gs.GroupID, userID)
	}
	for _, id := range gs.DirectMembers {
		if id == userID {
			return true, nil
		}
	}
	for _, sub := range gs.DirectSubgroups {
		ok, err := c.IsUserGroupMember(ctx, sub, userID)
		if err != nil {
			return false, err
		}
		if ok {
			return true, nil
		}
	}
	return false, nil
}

// moveLimit reads a *_limit_seconds realm setting as a duration.
//
// JSON null is Zulip's "any time" and is returned as zero. A setting
// the server does not report at all is an ERROR rather than zero: an
// absent limit and an unlimited one look identical to a zero value,
// and only one of them means the caller may proceed.
func moveLimit(snap map[string]json.RawMessage, name string) (time.Duration, error) {
	raw, ok := snap[name]
	if !ok {
		return 0, fmt.Errorf("zulip: this server does not report %s", name)
	}
	if string(raw) == "null" {
		return 0, nil
	}
	var secs int64
	if err := json.Unmarshal(raw, &secs); err != nil {
		return 0, fmt.Errorf("zulip: %s is not a number of seconds: %w", name, err)
	}
	if secs < 0 {
		return 0, fmt.Errorf("zulip: %s is negative (%d)", name, secs)
	}
	return time.Duration(secs) * time.Second, nil
}
