package proto

import "encoding/json"

// Text is a minimal chat component, encodable as JSON (login phase, status,
// chat before 1.20.3) or NBT (configuration phase and chat from 1.20.3).
type Text struct {
	Text   string
	Color  string
	Bold   bool
	Italic bool
	// Click, if set, is a command run when the component is clicked.
	Click string
	// Hover is shown as a tooltip.
	Hover *Text
	Extra []Text
}

// T is shorthand for a plain text component.
func T(s string) Text { return Text{Text: s} }

// C is shorthand for a colored text component.
func C(s, color string) Text { return Text{Text: s, Color: color} }

// Join concatenates components.
func Join(parts ...Text) Text { return Text{Extra: parts} }

type clickJSON struct {
	Action string `json:"action"`
	Value  string `json:"value"`
}

type hoverJSON struct {
	Action string `json:"action"`
	Value  any    `json:"value"` // "value" is understood by every version up to 1.21.4
}

type textJSON struct {
	Text       string     `json:"text"`
	Color      string     `json:"color,omitempty"`
	Bold       bool       `json:"bold,omitempty"`
	Italic     bool       `json:"italic,omitempty"`
	ClickEvent *clickJSON `json:"clickEvent,omitempty"`
	HoverEvent *hoverJSON `json:"hoverEvent,omitempty"`
	Extra      []any      `json:"extra,omitempty"`
}

func (t Text) jsonValue() any {
	v := textJSON{Text: t.Text, Color: t.Color, Bold: t.Bold, Italic: t.Italic}
	if t.Click != "" {
		v.ClickEvent = &clickJSON{Action: "run_command", Value: t.Click}
	}
	if t.Hover != nil {
		v.HoverEvent = &hoverJSON{Action: "show_text", Value: t.Hover.jsonValue()}
	}
	for _, e := range t.Extra {
		v.Extra = append(v.Extra, e.jsonValue())
	}
	return v
}

func (t Text) JSON() string {
	b, _ := json.Marshal(t.jsonValue())
	return string(b)
}

// NBT encodes the component in the 1.20.3-1.21.4 NBT form (camelCase events,
// hover "contents").
func (t Text) NBT() Compound {
	c := Compound{{"text", t.Text}}
	if t.Color != "" {
		c = append(c, Field{"color", t.Color})
	}
	if t.Bold {
		c = append(c, Field{"bold", true})
	}
	if t.Italic {
		c = append(c, Field{"italic", true})
	}
	if t.Click != "" {
		c = append(c, Field{"clickEvent", Compound{{"action", "run_command"}, {"value", t.Click}}})
	}
	if t.Hover != nil {
		c = append(c, Field{"hoverEvent", Compound{{"action", "show_text"}, {"contents", t.Hover.NBT()}}})
	}
	if len(t.Extra) > 0 {
		l := make(List, len(t.Extra))
		for i, e := range t.Extra {
			l[i] = e.NBT()
		}
		c = append(c, Field{"extra", l})
	}
	return c
}
