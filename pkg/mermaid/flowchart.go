package mermaid

import (
	"fmt"
	"strings"
)

// Flowchart builds a Mermaid flowchart diagram.
type Flowchart struct {
	direction Direction
	lines     []string
	styles    []string
}

// NewFlowchart creates a new Flowchart builder.
func NewFlowchart(dir Direction) *Flowchart {
	return &Flowchart{direction: dir}
}

// AddNode adds a node to the flowchart.
func (f *Flowchart) AddNode(id, label string, shape Shape) *Flowchart {
	f.lines = append(f.lines, "    "+nodeShape(id, EscapeLabel(label), shape))
	return f
}

// AddEdge adds an edge between two nodes.
func (f *Flowchart) AddEdge(from, to, label string, style EdgeStyle) *Flowchart {
	f.lines = append(f.lines, fmt.Sprintf("    %s %s %s", from, edgeArrow(style, EscapeLabel(label)), to))
	return f
}

// AddSubgraph adds a subgraph with a callback to populate it.
func (f *Flowchart) AddSubgraph(id, label string, fn func(sg *Subgraph)) *Flowchart {
	sg := &Subgraph{}
	fn(sg)
	f.lines = append(f.lines, fmt.Sprintf("    subgraph %s[\"%s\"]", id, EscapeLabel(label)))
	for _, line := range sg.lines {
		f.lines = append(f.lines, "    "+line)
	}
	f.lines = append(f.lines, "    end")
	return f
}

// AddStyle applies a severity-based style to a node.
func (f *Flowchart) AddStyle(nodeID string, sev Severity) *Flowchart {
	if style, ok := severityStyles[sev]; ok {
		f.styles = append(f.styles, fmt.Sprintf("    style %s %s", nodeID, style))
	}
	return f
}

// AddRawStyle applies a raw CSS-like style string to a node.
func (f *Flowchart) AddRawStyle(nodeID, style string) *Flowchart {
	f.styles = append(f.styles, fmt.Sprintf("    style %s %s", nodeID, style))
	return f
}

// AddRaw adds a raw line to the flowchart.
func (f *Flowchart) AddRaw(line string) *Flowchart {
	f.lines = append(f.lines, "    "+line)
	return f
}

// Render produces the Mermaid flowchart string (without fenced block).
func (f *Flowchart) Render() string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("flowchart %s\n", f.direction))
	for _, line := range f.lines {
		sb.WriteString(line + "\n")
	}
	for _, style := range f.styles {
		sb.WriteString(style + "\n")
	}
	return sb.String()
}

// RenderBlock produces the Mermaid flowchart wrapped in a fenced code block.
func (f *Flowchart) RenderBlock() string {
	return WrapBlock(f.Render())
}

// Subgraph collects nodes and edges inside a subgraph.
type Subgraph struct {
	lines []string
}

// AddNode adds a node inside the subgraph.
func (sg *Subgraph) AddNode(id, label string, shape Shape) *Subgraph {
	sg.lines = append(sg.lines, "    "+nodeShape(id, EscapeLabel(label), shape))
	return sg
}

// AddEdge adds an edge inside the subgraph.
func (sg *Subgraph) AddEdge(from, to, label string, style EdgeStyle) *Subgraph {
	sg.lines = append(sg.lines, fmt.Sprintf("    %s %s %s", from, edgeArrow(style, EscapeLabel(label)), to))
	return sg
}

// AddRaw adds a raw line inside the subgraph.
func (sg *Subgraph) AddRaw(line string) *Subgraph {
	sg.lines = append(sg.lines, "    "+line)
	return sg
}

// AddNestedSubgraph adds a nested subgraph.
func (sg *Subgraph) AddNestedSubgraph(id, label string, fn func(nested *Subgraph)) *Subgraph {
	nested := &Subgraph{}
	fn(nested)
	sg.lines = append(sg.lines, fmt.Sprintf("    subgraph %s[\"%s\"]", id, EscapeLabel(label)))
	for _, line := range nested.lines {
		sg.lines = append(sg.lines, "    "+line)
	}
	sg.lines = append(sg.lines, "    end")
	return sg
}
