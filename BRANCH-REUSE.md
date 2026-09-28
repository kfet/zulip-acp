# Can agent branching move to acp-kit?

Status: findings and a proposal. Nothing in `acp-kit`, `poe-acp` or
`slack-acp` is changed yet.

## What zulip-acp has now

Three entry points, one implementation (`branchOnce` in
`internal/handler/branch.go`):

- `!branch [#**channel**] <text>` (typed, human)
- the :fork_and_knife: reaction (human)
- the `branch` relay MCP tool (agent), in `internal/zulipmcp/branch.go` with
  its relay half in `internal/handler/branchtool.go`

## Generic vs Zulip-specific

| Part | Generic? | Why |
|---|---|---|
| Tool shape: `tasks[{title?, seed?, from_msg?}]` → `[{topic, link, conv_id, error?}]`, cap per call, per-task errors | Generic | Any relay with more than one conversation can fan out. Only the words "topic" and "link" are Zulip-flavoured; call them `name` and `ref`. |
| Argument validation (non-empty, cap, non-negative ids) | Generic | Pure. |
| Seed default = text of the branch-point message | Generic | Needs only "read message N of this conversation". |
| Branch point must be in the caller's conversation | Generic rule, relay-specific check | The rule is the permission model. The check needs the relay's message store. |
| Parent pointer {parent conv, branch-point msg id} + audit {by, at, seed} | Generic | acp-kit `state` already owns per-conversation state; a parent record is a small addition. zulip-acp keeps it in its own journal today. |
| One-hop `history(origin=true)` clamp | Generic rule | Relay supplies the read. |
| Root-of-tree walk | Generic | A loop over parent pointers with a depth cap. The per-hop "where is it now" resolution is relay-specific (Zulip topics move). |
| Notification text: list of links + one-line goal; one line in the root | Generic shape | The link syntax (`#**channel>topic**`) is Zulip's. |
| Creating the destination (topic, collision walk, `MAX_TOPIC_LENGTH`, seed message creates the topic) | Zulip | Pure Zulip wire. |
| Destination channel, DM refusal | Zulip | Slack threads and Poe chats have other shapes. |

## Does it fit poe-acp and slack-acp?

- **slack-acp**: yes. A thread is a conversation; a branch is a new top-level
  message in the same channel whose thread gets its own session. The
  branch-point check is "`ts` is in this thread".
- **poe-acp**: no, not now. A Poe conversation is one HTTP request per turn and
  the relay cannot open a new chat for the user (the same reason it has no
  `Poster`). The capability must be optional, as `Poster` and `Scheduler` are.

## Proposal

1. `acp-kit/command`: add an optional capability, same pattern as `Poster`:

   ```go
   type Brancher interface {
       // Branch runs the tasks out of convID. A whole-call error means
       // nothing was created; per-task failures are in the results.
       Branch(convID string, tasks []BranchTask) ([]BranchResult, error)
   }
   type BranchTask struct{ Title, Seed string; FromMsg string }
   type BranchResult struct{ Name, Ref, ConvID, Error string }
   const MaxBranchTasks = 10
   ```

   `FromMsg` is a string so Slack `ts` values fit. A `Broker.Branch` action
   validates the shape and the cap, then calls the capability.
2. `acp-kit/relaytool`: register a `branch` tool when the Controller
   implements `Brancher`, over `Broker.Branch` — the same rule as every other
   generic tool (one code path for tool and command).
3. `acp-kit/state`: optional parent record `{ParentConv, FromMsg, By, At,
   Seed}` per conversation, plus `Root(convID, resolve func)` with a depth cap.
   The relay passes `resolve` to re-locate a hop (Zulip: from the message id).
4. `acp-kit/sysprompt`: one shared paragraph on when to branch and how to get
   message ids, appended only when the tool is registered.
5. zulip-acp then keeps only: `branchOnce` (topic creation, collision walk,
   seed post), the `from_msg`-in-topic check, `#**channel>topic**` rendering,
   and `history`. `zulipmcp.ToolBranch` goes away in favour of relaytool's.

Order: 1–2 first (smallest, unblocks slack-acp), then 3 (needs a journal →
state migration in zulip-acp, which is the most work), then 4.

Risk to check first: `relaytool` identities are broker tokens, not conv-ids.
`Brancher` must take whatever the other capabilities take (`PostTo` takes the
token), so the relay resolves the conversation server-side as it does today.
