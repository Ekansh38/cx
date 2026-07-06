package ui

// Input soft-wrap row counting.
//
// The height of the prompt box must agree EXACTLY with how bubbles/textarea
// soft-wraps its content, or rows get clipped while typing. There is no
// exported API for the wrapped height, so wrap() and repeatSpaces() below are
// copied verbatim from github.com/charmbracelet/bubbles@v0.20.0/textarea
// (MIT License, Copyright (c) 2020-2023 Charmbracelet, Inc). If the bubbles
// dependency is upgraded, re-copy these.

import (
	"strings"
	"unicode"

	rw "github.com/mattn/go-runewidth"
	"github.com/rivo/uniseg"
)

// wrappedRows returns the number of display rows value occupies in a textarea
// of the given width.
func wrappedRows(value string, width int) int {
	if width <= 0 {
		return 1
	}
	rows := 0
	for line := range strings.SplitSeq(value, "\n") {
		rows += len(wrap([]rune(line), width))
	}
	if rows < 1 {
		rows = 1
	}
	return rows
}

func wrap(runes []rune, width int) [][]rune {
	var (
		lines  = [][]rune{{}}
		word   = []rune{}
		row    int
		spaces int
	)

	// Word wrap the runes
	for _, r := range runes {
		if unicode.IsSpace(r) {
			spaces++
		} else {
			word = append(word, r)
		}

		if spaces > 0 {
			if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces > width {
				row++
				lines = append(lines, []rune{})
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], repeatSpaces(spaces)...)
				spaces = 0
				word = nil
			} else {
				lines[row] = append(lines[row], word...)
				lines[row] = append(lines[row], repeatSpaces(spaces)...)
				spaces = 0
				word = nil
			}
		} else {
			// If the last character is a double-width rune, then we may not be able to add it to this line
			// as it might cause us to go past the width.
			lastCharLen := rw.RuneWidth(word[len(word)-1])
			if uniseg.StringWidth(string(word))+lastCharLen > width {
				// If the current line has any content, let's move to the next
				// line because the current word fills up the entire line.
				if len(lines[row]) > 0 {
					row++
					lines = append(lines, []rune{})
				}
				lines[row] = append(lines[row], word...)
				word = nil
			}
		}
	}

	if uniseg.StringWidth(string(lines[row]))+uniseg.StringWidth(string(word))+spaces >= width {
		lines = append(lines, []rune{})
		lines[row+1] = append(lines[row+1], word...)
		// We add an extra space at the end of the line to account for the
		// trailing space at the end of the previous soft-wrapped lines so that
		// behaviour when navigating is consistent and so that we don't need to
		// continually add edges to handle the last line of the wrapped input.
		spaces++
		lines[row+1] = append(lines[row+1], repeatSpaces(spaces)...)
	} else {
		lines[row] = append(lines[row], word...)
		spaces++
		lines[row] = append(lines[row], repeatSpaces(spaces)...)
	}

	return lines
}

func repeatSpaces(n int) []rune {
	return []rune(strings.Repeat(string(' '), n))
}
