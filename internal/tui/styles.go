package tui

import (
	"image/color"

	"charm.land/bubbles/v2/help"
	"charm.land/lipgloss/v2"
)

type palette struct {
	text, muted, subtle, divider       color.Color
	accent                             color.Color
	success, danger, warning, code     color.Color
	badgeInfoBg, badgeWarnBg, onStrong color.Color
}

func newPalette(dark bool) palette {
	ld := lipgloss.LightDark(dark)
	c := lipgloss.Color
	return palette{
		text:        ld(c("#050B24"), c("#EDEFF3")),
		muted:       ld(c("#828592"), c("#B4B6BD")),
		subtle:      ld(c("#B4B6BD"), c("#5C6070")),
		divider:     ld(c("#E6E7E9"), c("#2B3048")),
		accent:      ld(c("#FF6600"), c("#FF8433")),
		success:     ld(c("#1A7F37"), c("#3FB950")),
		danger:      ld(c("#CF222E"), c("#F85149")),
		warning:     ld(c("#9A6700"), c("#D29922")),
		code:        ld(c("#0B6BCB"), c("#48B5FF")),
		badgeInfoBg: ld(c("#E6E7E9"), c("#2B3048")),
		badgeWarnBg: ld(c("#FFE0CC"), c("#5C2A00")),
		onStrong:    c("#FFFFFF"),
	}
}

type styles struct {
	p palette

	text, bold, muted, subtle, divider, code, draft lipgloss.Style
	accent, accentBold, tabActive                   lipgloss.Style
	success, danger, dangerBold, warning            lipgloss.Style
	badgeInfo, badgeWarn, badgeCrit                 lipgloss.Style

	help help.Styles
}

func newStyles(dark bool) styles {
	p := newPalette(dark)
	s := lipgloss.NewStyle
	pill := s().Padding(0, 1)
	st := styles{
		p: p,

		text:    s().Foreground(p.text),
		bold:    s().Foreground(p.text).Bold(true),
		muted:   s().Foreground(p.muted),
		subtle:  s().Foreground(p.subtle),
		divider: s().Foreground(p.divider),
		code:    s().Foreground(p.code),
		draft:   s().Foreground(p.muted).Italic(true),

		accent:     s().Foreground(p.accent),
		accentBold: s().Foreground(p.accent).Bold(true),
		tabActive:  s().Foreground(p.accent).Bold(true).Underline(true),

		success:    s().Foreground(p.success),
		danger:     s().Foreground(p.danger),
		dangerBold: s().Foreground(p.danger).Bold(true),
		warning:    s().Foreground(p.warning),

		badgeInfo: pill.Foreground(p.text).Background(p.badgeInfoBg),
		badgeWarn: pill.Foreground(p.accent).Background(p.badgeWarnBg).Bold(true),
		badgeCrit: pill.Foreground(p.onStrong).Background(p.danger).Bold(true),

		help: help.DefaultStyles(dark),
	}
	st.help.ShortKey = st.help.ShortKey.Foreground(p.accent)
	st.help.FullKey = st.help.FullKey.Foreground(p.accent)
	return st
}
