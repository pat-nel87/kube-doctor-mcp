package mermaid

import (
	"fmt"
	"regexp"
	"strings"
)

// Direction specifies flowchart direction.
type Direction string

const (
	DirectionTB Direction = "TB"
	DirectionBT Direction = "BT"
	DirectionLR Direction = "LR"
	DirectionRL Direction = "RL"
)

// Shape specifies node shape in a flowchart.
type Shape string

const (
	ShapeRect    Shape = "rect"    // [text]
	ShapeRound   Shape = "round"   // (text)
	ShapeStadium Shape = "stadium" // ([text])
	ShapeCircle  Shape = "circle"  // ((text))
	ShapeDiamond Shape = "diamond" // {text}
	ShapeHex     Shape = "hex"     // {{text}}
	ShapeTrapAlt Shape = "trapalt" // [/text/]
	ShapeCyl     Shape = "cyl"     // [(text)]
)

// EdgeStyle specifies edge line style.
type EdgeStyle string

const (
	EdgeSolid  EdgeStyle = "solid"
	EdgeDotted EdgeStyle = "dotted"
	EdgeThick  EdgeStyle = "thick"
)

// Severity level for node styling.
type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityWarning  Severity = "warning"
	SeverityHealthy  Severity = "healthy"
	SeverityInfo     Severity = "info"
)

// Pre-defined style strings for severity levels.
var severityStyles = map[Severity]string{
	SeverityCritical: "fill:#ffcccc,stroke:#cc0000,stroke-width:2px",
	SeverityWarning:  "fill:#ffffcc,stroke:#cccc00,stroke-width:2px",
	SeverityHealthy:  "fill:#ccffcc,stroke:#00cc00,stroke-width:2px",
	SeverityInfo:     "fill:#cce5ff,stroke:#4a90d9,stroke-width:2px",
}

// safeIDRegexp removes characters not allowed in Mermaid IDs.
var safeIDRegexp = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// SafeID converts a string to a safe Mermaid node ID.
func SafeID(s string) string {
	id := safeIDRegexp.ReplaceAllString(s, "_")
	if len(id) == 0 {
		return "_"
	}
	// Ensure starts with letter or underscore
	if id[0] >= '0' && id[0] <= '9' {
		id = "_" + id
	}
	return id
}

// WrapBlock wraps Mermaid code in a fenced code block.
func WrapBlock(code string) string {
	return "```mermaid\n" + code + "\n```"
}

// nodeShape wraps text in the appropriate shape delimiters.
func nodeShape(id, label string, shape Shape) string {
	switch shape {
	case ShapeRound:
		return fmt.Sprintf("%s(%s)", id, label)
	case ShapeStadium:
		return fmt.Sprintf("%s([%s])", id, label)
	case ShapeCircle:
		return fmt.Sprintf("%s((%s))", id, label)
	case ShapeDiamond:
		return fmt.Sprintf("%s{%s}", id, label)
	case ShapeHex:
		return fmt.Sprintf("%s{{%s}}", id, label)
	case ShapeTrapAlt:
		return fmt.Sprintf("%s[/%s/]", id, label)
	case ShapeCyl:
		return fmt.Sprintf("%s[(%s)]", id, label)
	default: // ShapeRect
		return fmt.Sprintf("%s[%s]", id, label)
	}
}

// edgeArrow returns the arrow string for an edge style.
func edgeArrow(style EdgeStyle, label string) string {
	switch style {
	case EdgeDotted:
		if label != "" {
			return fmt.Sprintf("-.->|%s|", label)
		}
		return "-.->"
	case EdgeThick:
		if label != "" {
			return fmt.Sprintf("==>|%s|", label)
		}
		return "==>"
	default: // EdgeSolid
		if label != "" {
			return fmt.Sprintf("-->|%s|", label)
		}
		return "-->"
	}
}

// EscapeLabel escapes special characters in Mermaid labels.
func EscapeLabel(s string) string {
	s = strings.ReplaceAll(s, `"`, `#quot;`)
	return s
}

// BR returns a Mermaid line break.
func BR() string {
	return "<br/>"
}
