package cli

import (
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func bortCommand(args string) string {
	command := "bort"
	if strings.TrimSpace(os.Getenv("SUDO_UID")) != "" {
		command = "sudo bort"
	}
	if strings.TrimSpace(args) == "" {
		return command
	}
	return command + " " + args
}

type styler struct {
	color bool

	dim    lipgloss.Style
	bold   lipgloss.Style
	good   lipgloss.Style
	warn   lipgloss.Style
	bad    lipgloss.Style
	fixCmd lipgloss.Style
	badge  lipgloss.Style
}

func newStyler(w io.Writer) *styler {
	s := &styler{color: shouldColor(w)}
	if !s.color {
		return s
	}
	s.dim = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "240", Dark: "245"})
	s.bold = lipgloss.NewStyle().Bold(true)
	s.good = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "28", Dark: "42"})
	s.warn = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "166", Dark: "214"})
	s.bad = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "124", Dark: "203"}).Bold(true)
	s.fixCmd = lipgloss.NewStyle().Foreground(lipgloss.AdaptiveColor{Light: "27", Dark: "87"}).Bold(true)
	s.badge = lipgloss.NewStyle().
		Foreground(lipgloss.Color("232")).
		Background(lipgloss.Color("214")).
		Padding(0, 1).
		Bold(true)
	return s
}

func shouldColor(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	file, ok := w.(*os.File)
	if !ok {
		return false
	}
	return isInteractiveTerminal(file)
}

func (s *styler) glyph(g string, level severity) string {
	if !s.color {
		return g
	}
	switch level {
	case sevGood:
		return s.good.Render(g)
	case sevWarn:
		return s.warn.Render(g)
	case sevBad:
		return s.bad.Render(g)
	default:
		return s.dim.Render(g)
	}
}

func (s *styler) pill(label string, level severity) string {
	if !s.color {
		return "[" + label + "]"
	}
	style := s.badge
	switch level {
	case sevGood:
		style = style.Background(lipgloss.Color("42")).Foreground(lipgloss.Color("232"))
	case sevWarn:
		style = style.Background(lipgloss.Color("214")).Foreground(lipgloss.Color("232"))
	case sevBad:
		style = style.Background(lipgloss.Color("203")).Foreground(lipgloss.Color("231"))
	}
	return style.Render(label)
}

func (s *styler) fix(command string) string {
	if !s.color {
		return "fix: " + command
	}
	return s.fixCmd.Render("fix:") + " " + command
}

func (s *styler) muted(text string) string {
	if !s.color {
		return text
	}
	return s.dim.Render(text)
}

func (s *styler) emph(text string) string {
	if !s.color {
		return text
	}
	return s.bold.Render(text)
}

type severity int

const (
	sevDim severity = iota
	sevGood
	sevWarn
	sevBad
)
