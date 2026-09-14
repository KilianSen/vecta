package proto

import "encoding/json"

// Text is a minimal chat component, encodable as JSON (login phase, status)
// or NBT (configuration phase, 1.20.3+).
type Text struct {
	Text   string `json:"text"`
	Color  string `json:"color,omitempty"`
	Bold   bool   `json:"bold,omitempty"`
	Italic bool   `json:"italic,omitempty"`
	Extra  []Text `json:"extra,omitempty"`
}

// T is shorthand for a plain text component.
func T(s string) Text { return Text{Text: s} }

// C is shorthand for a colored text component.
func C(s, color string) Text { return Text{Text: s, Color: color} }

// Join concatenates components.
func Join(parts ...Text) Text { return Text{Extra: parts} }

func (t Text) JSON() string {
	b, _ := json.Marshal(t)
	return string(b)
}

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
	if len(t.Extra) > 0 {
		l := make(List, len(t.Extra))
		for i, e := range t.Extra {
			l[i] = e.NBT()
		}
		c = append(c, Field{"extra", l})
	}
	return c
}
