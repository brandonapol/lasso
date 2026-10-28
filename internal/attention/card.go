package attention

import (
	"errors"
	"fmt"
)

// DecisionCard is the shape of every ask (ADR 0001 §5): enough to decide in
// one read, in plain words.
type DecisionCard struct {
	// Wants is what the bot wants to do.
	Wants string `json:"wants"`
	// WhyAsking says which rule it falls outside, i.e. why lasso didn't just
	// decide.
	WhyAsking string `json:"why_asking"`
	// Plain is the whole thing in one or two everyday sentences.
	Plain string `json:"plain"`
	// IfYes and IfNo say what happens after each answer.
	IfYes string `json:"if_yes"`
	IfNo  string `json:"if_no"`
	// Recommendation is lasso's suggested answer and why.
	Recommendation string `json:"recommendation"`
}

// Fields returns the card's text fields by name, in display order.
func (c DecisionCard) Fields() [][2]string {
	return [][2]string{
		{"wants", c.Wants},
		{"why_asking", c.WhyAsking},
		{"plain", c.Plain},
		{"if_yes", c.IfYes},
		{"if_no", c.IfNo},
		{"recommendation", c.Recommendation},
	}
}

// Validate checks that every field is filled in and free of bot jargon.
func (c DecisionCard) Validate() error {
	var errs []error
	for _, f := range c.Fields() {
		if f[1] == "" {
			errs = append(errs, fmt.Errorf("decision card: %s is empty", f[0]))
			continue
		}
		if terms := Unexplained(f[1]); len(terms) > 0 {
			errs = append(errs, fmt.Errorf("decision card: %s uses jargon %q", f[0], terms))
		}
	}
	return errors.Join(errs...)
}

// PlainCard returns the card with every field passed through Plain.
func PlainCard(c DecisionCard) DecisionCard {
	return DecisionCard{
		Wants:          Plain(c.Wants),
		WhyAsking:      Plain(c.WhyAsking),
		Plain:          Plain(c.Plain),
		IfYes:          Plain(c.IfYes),
		IfNo:           Plain(c.IfNo),
		Recommendation: Plain(c.Recommendation),
	}
}
