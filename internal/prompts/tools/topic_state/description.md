Record this topic's state when it changes. The state lasts across turns and feeds the user's !inbox, where they pick which of many topics to open. A new topic has no state. Call this tool only when this turn changes what the user must do with the topic:
- needs_you: you wait for their answer or decision.
- fyi: you delivered a result or answer; they should read it, but you need nothing from them.
- waiting: work goes on without them (a build, a branched agent, a schedule).
- blocked: you cannot continue, and an answer alone will not fix it (for example, an expired credential only they can renew).
- done: nothing is left to read or do.
If the state is the same as before, do not call it. The user can override it with !state.
The reason is shown only in the inbox, never in the topic. Write your answer, plan or question in your reply text; this tool does not deliver it.
