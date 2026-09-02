// This file is the relay's only coupling to Zulip's WIDGET surface.
//
// A bot attaches a `widget_content` payload to a message and the Zulip
// WEB app renders it as an interactive form. The only widget shape the
// relay uses is `zform` / `choices`: a heading plus buttons, where each
// button carries a `reply` string that the web client sends as an
// ORDINARY message from the clicking user. That is the whole trick —
// a button is sugar over the existing `!command` parser, so nothing
// new lives behind one.
//
// Two facts bound how much of this package may grow:
//
//  1. Widgets are a dev-docs SUBSYSTEM
//     (zulip.readthedocs.io/en/stable/subsystems/widgets.html), not a
//     stable, versioned API. So the coupling stays thin: a struct, a
//     marshaller, and one extra form field. Nothing here may become
//     load-bearing for correctness.
//  2. Only the web app renders it. Every other client — the phone app
//     included — shows the message's ordinary markdown CONTENT. The
//     caller must therefore always send a body that is complete and
//     usable on its own; the widget is decoration on top of it, never
//     the payload.
//
// # A widget message can never be edited
//
// MEASURED on Zulip 12.2: PATCH /messages/<id> on a message carrying
// widget_content is refused with HTTP 400 "Widgets cannot be edited."
// A zform is stored as a submessage, and Zulip forbids content edits on
// any message that has one. There is no flag to opt out.
//
// This is not a detail — it decides the shape of anything built on
// widgets. A self-updating control message is IMPOSSIBLE to do by
// editing: it must be re-posted, and the old one deleted (see
// Client.DeleteMessage) or left behind. Do not "fix" a caller by making
// it edit; the server will refuse.
package zulipproto

// WidgetTypeZForm is the only widget_type the relay emits.
const WidgetTypeZForm = "zform"

// zformChoices is the `choices` flavour of zform's extra_data.
const zformChoices = "choices"

// choiceType is the per-button type string zform expects. Zulip only
// defines the one.
const choiceType = "multiple_choice"

// ZFormChoice is one button.
//
// Reply is what makes the button harmless: clicking it sends that
// exact text as a normal message from the user, through every gate the
// relay already applies to typed text. A button can therefore never
// reach anything a human could not have typed.
type ZFormChoice struct {
	Type      string `json:"type"`
	ShortName string `json:"short_name"`
	LongName  string `json:"long_name"`
	Reply     string `json:"reply"`
}

// Choice builds one button. shortName is the compact label, longName
// the fuller one Zulip shows beside it, reply the message the click
// sends.
func Choice(shortName, longName, reply string) ZFormChoice {
	return ZFormChoice{Type: choiceType, ShortName: shortName, LongName: longName, Reply: reply}
}

// zform is the whole widget_content document.
type zform struct {
	WidgetType string    `json:"widget_type"`
	ExtraData  extraData `json:"extra_data"`
}

type extraData struct {
	Type    string        `json:"type"`
	Heading string        `json:"heading"`
	Choices []ZFormChoice `json:"choices"`
}

// ZForm renders a choices widget as the JSON string to pass as
// widget_content. It returns "" for an empty choice list: a widget
// with no buttons is not a degraded widget, it is a broken one, and
// sending it would only risk the server rejecting a message whose
// markdown was fine.
func ZForm(heading string, choices []ZFormChoice) string {
	if len(choices) == 0 {
		return ""
	}
	return mustJSON(zform{
		WidgetType: WidgetTypeZForm,
		ExtraData:  extraData{Type: zformChoices, Heading: heading, Choices: choices},
	})
}
