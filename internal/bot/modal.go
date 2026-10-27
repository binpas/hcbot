package bot

import (
	"encoding/json"
	"log"

	"github.com/bwmarrin/discordgo"
)

// Modal components that discordgo v0.29 does not have: a Label holds one
// input (a text input or a string select), with a title and a description.
// discordgo cannot read a modal submit with Labels, so onRawInteraction
// reads it from the raw gateway event.

const labelComponent discordgo.ComponentType = 18

// modalLabel is a Label (type 18) around one modal input.
type modalLabel struct {
	Label       string
	Description string
	Component   discordgo.MessageComponent
}

func (l modalLabel) Type() discordgo.ComponentType { return labelComponent }

func (l modalLabel) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type        discordgo.ComponentType    `json:"type"`
		Label       string                     `json:"label"`
		Description string                     `json:"description,omitempty"`
		Component   discordgo.MessageComponent `json:"component"`
	}{l.Type(), l.Label, l.Description, l.Component})
}

// modalText is a text input in a Label. It has no label of its own.
type modalText struct {
	CustomID  string
	Style     discordgo.TextInputStyle
	MinLength int
	MaxLength int
	Value     string
}

func (t modalText) Type() discordgo.ComponentType { return discordgo.TextInputComponent }

func (t modalText) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type      discordgo.ComponentType  `json:"type"`
		CustomID  string                   `json:"custom_id"`
		Style     discordgo.TextInputStyle `json:"style"`
		MinLength int                      `json:"min_length,omitempty"`
		MaxLength int                      `json:"max_length,omitempty"`
		Value     string                   `json:"value,omitempty"`
		Required  bool                     `json:"required"`
	}{t.Type(), t.CustomID, t.Style, t.MinLength, t.MaxLength, t.Value, true})
}

// modalSelect is a required string select (one value) in a Label.
type modalSelect struct {
	CustomID string
	Options  []discordgo.SelectMenuOption
}

func (s modalSelect) Type() discordgo.ComponentType { return discordgo.SelectMenuComponent }

func (s modalSelect) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Type     discordgo.ComponentType      `json:"type"`
		CustomID string                       `json:"custom_id"`
		Options  []discordgo.SelectMenuOption `json:"options"`
		Min      int                          `json:"min_values"`
		Max      int                          `json:"max_values"`
		Required bool                         `json:"required"`
	}{s.Type(), s.CustomID, s.Options, 1, 1, true})
}

// rawComponent is a submitted modal component, at any depth.
type rawComponent struct {
	Type       discordgo.ComponentType `json:"type"`
	CustomID   string                  `json:"custom_id"`
	Value      *string                 `json:"value"`
	Values     []string                `json:"values"`
	Component  *rawComponent           `json:"component"`
	Components []rawComponent          `json:"components"`
}

// collect puts each input's values in out, by custom ID: a select's
// values, or a text input's text.
func (r *rawComponent) collect(out map[string][]string) {
	if r.CustomID != "" {
		switch {
		case r.Values != nil:
			out[r.CustomID] = r.Values
		case r.Value != nil:
			out[r.CustomID] = []string{*r.Value}
		}
	}
	if r.Component != nil {
		r.Component.collect(out)
	}
	for i := range r.Components {
		r.Components[i].collect(out)
	}
}

// onRawInteraction handles a modal submit that discordgo could not read
// (one with Labels). onInteraction ignores such a submit,
// because its Data is nil.
func (b *Bot) onRawInteraction(s *discordgo.Session, e *discordgo.Event) {
	if e.Type != "INTERACTION_CREATE" {
		return
	}
	var head struct {
		Type discordgo.InteractionType `json:"type"`
		Data struct {
			CustomID   string         `json:"custom_id"`
			Components []rawComponent `json:"components"`
		} `json:"data"`
	}
	if json.Unmarshal(e.RawData, &head) != nil || head.Type != discordgo.InteractionModalSubmit {
		return
	}
	var typed discordgo.Interaction
	if json.Unmarshal(e.RawData, &typed) == nil {
		return // discordgo read it; onInteraction handles it
	}
	// Read the interaction again without its components.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(e.RawData, &raw); err != nil {
		return
	}
	raw["data"], _ = json.Marshal(map[string]any{"custom_id": head.Data.CustomID, "components": []any{}})
	stripped, _ := json.Marshal(raw)
	var i discordgo.Interaction
	if err := json.Unmarshal(stripped, &i); err != nil {
		log.Printf("modal submit %s: %v", head.Data.CustomID, err)
		return
	}
	fields := map[string][]string{}
	for k := range head.Data.Components {
		head.Data.Components[k].collect(fields)
	}
	b.handleInteraction(s, &discordgo.InteractionCreate{Interaction: &i}, fields)
}
