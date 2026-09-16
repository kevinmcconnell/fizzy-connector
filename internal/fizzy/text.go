package fizzy

import (
	"html"
	"regexp"
	"strings"
)

var (
	linkPattern      = regexp.MustCompile(`(?is)<a\b[^>]*\bhref="([^"]*)"[^>]*>(.*?)</a>`)
	lineBreakPattern = regexp.MustCompile(`(?i)<br\s*/?>|</(p|div|li|h[1-6]|pre|blockquote|tr)>`)
	listItemPattern  = regexp.MustCompile(`(?i)<li\b[^>]*>`)
	tagPattern       = regexp.MustCompile(`(?s)<[^>]+>`)
	blankRunPattern  = regexp.MustCompile(`\n\s*\n\s*`)
)

// PlainText converts Fizzy rich text to text, and keeps the link targets so
// that card references stay usable.
func PlainText(richText string) string {
	text := linkPattern.ReplaceAllStringFunc(richText, func(link string) string {
		match := linkPattern.FindStringSubmatch(link)
		href, label := match[1], strings.TrimSpace(tagPattern.ReplaceAllString(match[2], ""))
		if label == "" || label == href {
			return href
		}
		return label + " (" + href + ")"
	})
	text = listItemPattern.ReplaceAllString(text, "- ")
	text = lineBreakPattern.ReplaceAllString(text, "\n")
	text = tagPattern.ReplaceAllString(text, "")
	text = html.UnescapeString(text)
	text = blankRunPattern.ReplaceAllString(text, "\n\n")
	return strings.TrimSpace(text)
}
