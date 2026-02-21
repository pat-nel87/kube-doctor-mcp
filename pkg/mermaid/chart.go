package mermaid

import (
	"fmt"
	"strings"
)

// XYChart builds a Mermaid xychart-beta diagram.
type XYChart struct {
	title    string
	xLabels  []string
	yTitle   string
	yMin     float64
	yMax     float64
	datasets []chartDataset
}

type chartDataset struct {
	kind   string    // "bar" or "line"
	values []float64
}

// NewXYChart creates a new XY chart builder.
func NewXYChart(title string) *XYChart {
	return &XYChart{title: title}
}

// SetXAxis sets the x-axis labels.
func (c *XYChart) SetXAxis(labels []string) *XYChart {
	c.xLabels = labels
	return c
}

// SetYAxis sets the y-axis title and range.
func (c *XYChart) SetYAxis(title string, min, max float64) *XYChart {
	c.yTitle = title
	c.yMin = min
	c.yMax = max
	return c
}

// AddBar adds a bar dataset.
func (c *XYChart) AddBar(values []float64) *XYChart {
	c.datasets = append(c.datasets, chartDataset{kind: "bar", values: values})
	return c
}

// AddLine adds a line dataset.
func (c *XYChart) AddLine(values []float64) *XYChart {
	c.datasets = append(c.datasets, chartDataset{kind: "line", values: values})
	return c
}

// Render produces the Mermaid xychart-beta string.
func (c *XYChart) Render() string {
	var sb strings.Builder
	sb.WriteString("%%{init: {'theme':'neutral'}}%%\n")
	sb.WriteString("xychart-beta\n")
	sb.WriteString(fmt.Sprintf("    title \"%s\"\n", c.title))

	// X-axis
	if len(c.xLabels) > 0 {
		quoted := make([]string, len(c.xLabels))
		for i, l := range c.xLabels {
			quoted[i] = fmt.Sprintf(`"%s"`, l)
		}
		sb.WriteString(fmt.Sprintf("    x-axis [%s]\n", strings.Join(quoted, ", ")))
	}

	// Y-axis
	if c.yTitle != "" {
		sb.WriteString(fmt.Sprintf("    y-axis \"%s\" %.0f --> %.0f\n", c.yTitle, c.yMin, c.yMax))
	}

	// Datasets
	for _, ds := range c.datasets {
		vals := make([]string, len(ds.values))
		for i, v := range ds.values {
			vals[i] = fmt.Sprintf("%.1f", v)
		}
		sb.WriteString(fmt.Sprintf("    %s [%s]\n", ds.kind, strings.Join(vals, ", ")))
	}

	return sb.String()
}

// RenderBlock produces the chart wrapped in a fenced code block.
func (c *XYChart) RenderBlock() string {
	return WrapBlock(c.Render())
}
