package mermaid

import (
	"fmt"
	"strings"
)

// Gantt builds a Mermaid gantt chart.
type Gantt struct {
	title      string
	dateFormat string
	axisFormat string
	lines      []string
}

// NewGantt creates a new Gantt builder.
func NewGantt(title string) *Gantt {
	return &Gantt{
		title:      title,
		dateFormat: "HH:mm",
		axisFormat: "%H:%M",
	}
}

// SetDateFormat sets the date format (default: "HH:mm").
func (g *Gantt) SetDateFormat(format string) *Gantt {
	g.dateFormat = format
	return g
}

// SetAxisFormat sets the axis format (default: "%H:%M").
func (g *Gantt) SetAxisFormat(format string) *Gantt {
	g.axisFormat = format
	return g
}

// AddSection adds a section header.
func (g *Gantt) AddSection(name string) *Gantt {
	g.lines = append(g.lines, fmt.Sprintf("    section %s", name))
	return g
}

// AddTask adds a task to the chart.
// status is "active", "done", "crit", or "crit, active", etc.
func (g *Gantt) AddTask(name, status, start, end string) *Gantt {
	g.lines = append(g.lines, fmt.Sprintf("    %s           :%s, %s, %s", name, status, start, end))
	return g
}

// AddMilestone adds a milestone to the chart.
func (g *Gantt) AddMilestone(name, date string) *Gantt {
	g.lines = append(g.lines, fmt.Sprintf("    %s           :milestone, %s, 0d", name, date))
	return g
}

// Render produces the Mermaid gantt string.
func (g *Gantt) Render() string {
	var sb strings.Builder
	sb.WriteString("gantt\n")
	sb.WriteString(fmt.Sprintf("    title %s\n", g.title))
	sb.WriteString(fmt.Sprintf("    dateFormat %s\n", g.dateFormat))
	sb.WriteString(fmt.Sprintf("    axisFormat %s\n", g.axisFormat))
	sb.WriteString("\n")
	for _, line := range g.lines {
		sb.WriteString(line + "\n")
	}
	return sb.String()
}

// RenderBlock produces the gantt chart wrapped in a fenced code block.
func (g *Gantt) RenderBlock() string {
	return WrapBlock(g.Render())
}
