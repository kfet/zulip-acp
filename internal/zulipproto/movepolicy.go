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
)

// RealmMoveBetweenChannels is the realm setting naming the users
// allowed to move messages between channels.
const RealmMoveBetweenChannels = "realm_can_move_messages_between_channels_group"

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

// CanMoveMessagesBetweenChannels reports whether userID — in practice
// the relay's own bot — is permitted by realm policy to move messages
// from one channel to another.
//
// An error means "cannot tell", which is NOT the same as "no": the
// caller decides what to do about an unknown answer, and for a
// destructive feature the only safe choice is to stay off.
//
// It deliberately does NOT account for
// move_messages_between_streams_limit_seconds, the per-message time
// limit: that bounds how OLD a message may be, so it cannot be settled
// at startup for a topic that does not exist yet. A move refused on
// those grounds fails loudly at the time, which is the failure mode
// this check cannot remove and does not pretend to.
func (c *Client) CanMoveMessagesBetweenChannels(ctx context.Context, userID int64) (bool, error) {
	gs, err := c.RealmGroupSetting(ctx, RealmMoveBetweenChannels)
	if err != nil {
		return false, err
	}
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
