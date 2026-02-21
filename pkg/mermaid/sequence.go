package mermaid

import (
	"fmt"
	"strings"
)

// MessageStyle specifies arrow type in sequence diagrams.
type MessageStyle string

const (
	MsgSolid      MessageStyle = "solid"      // ->>
	MsgDotted     MessageStyle = "dotted"      // -->>
	MsgSolidOpen  MessageStyle = "solid_open"  // -)
	MsgDottedOpen MessageStyle = "dotted_open" // --)
)

// NotePosition specifies where a note appears.
type NotePosition string

const (
	NoteOver  NotePosition = "over"
	NoteRight NotePosition = "right of"
	NoteLeft  NotePosition = "left of"
)

// Sequence builds a Mermaid sequence diagram.
type Sequence struct {
	lines []string
}

// NewSequence creates a new Sequence builder.
func NewSequence() *Sequence {
	return &Sequence{}
}

// AddParticipant adds a participant (actor) to the diagram.
func (s *Sequence) AddParticipant(id, label string) *Sequence {
	s.lines = append(s.lines, fmt.Sprintf("    participant %s as %s", id, label))
	return s
}

// AddActor adds an actor (stick figure) to the diagram.
func (s *Sequence) AddActor(id, label string) *Sequence {
	s.lines = append(s.lines, fmt.Sprintf("    actor %s as %s", id, label))
	return s
}

// AddMessage adds a message arrow between participants.
func (s *Sequence) AddMessage(from, to, text string, style MessageStyle) *Sequence {
	arrow := s.arrowStyle(style)
	s.lines = append(s.lines, fmt.Sprintf("    %s%s%s: %s", from, arrow, to, text))
	return s
}

// AddNote adds a note in the diagram.
func (s *Sequence) AddNote(participant, text string, pos NotePosition) *Sequence {
	s.lines = append(s.lines, fmt.Sprintf("    Note %s %s: %s", pos, participant, text))
	return s
}

// AddNoteSpanning adds a note spanning multiple participants.
func (s *Sequence) AddNoteSpanning(from, to, text string) *Sequence {
	s.lines = append(s.lines, fmt.Sprintf("    Note over %s,%s: %s", from, to, text))
	return s
}

// AddActivate marks a participant as active.
func (s *Sequence) AddActivate(participant string) *Sequence {
	s.lines = append(s.lines, fmt.Sprintf("    activate %s", participant))
	return s
}

// AddDeactivate marks a participant as deactivated.
func (s *Sequence) AddDeactivate(participant string) *Sequence {
	s.lines = append(s.lines, fmt.Sprintf("    deactivate %s", participant))
	return s
}

// AddRect adds a colored background rectangle.
func (s *Sequence) AddRect(color string, fn func(inner *Sequence)) *Sequence {
	inner := &Sequence{}
	fn(inner)
	s.lines = append(s.lines, fmt.Sprintf("    rect %s", color))
	s.lines = append(s.lines, inner.lines...)
	s.lines = append(s.lines, "    end")
	return s
}

// AddRaw adds a raw line.
func (s *Sequence) AddRaw(line string) *Sequence {
	s.lines = append(s.lines, "    "+line)
	return s
}

func (s *Sequence) arrowStyle(style MessageStyle) string {
	switch style {
	case MsgDotted:
		return "-->>"
	case MsgSolidOpen:
		return "-)"
	case MsgDottedOpen:
		return "--)"
	default:
		return "->>"
	}
}

// Render produces the Mermaid sequence diagram string.
func (s *Sequence) Render() string {
	var sb strings.Builder
	sb.WriteString("sequenceDiagram\n")
	for _, line := range s.lines {
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

// RenderBlock produces the sequence diagram wrapped in a fenced code block.
func (s *Sequence) RenderBlock() string {
	return WrapBlock(s.Render())
}
