package fizzy

import (
	"bytes"
	"html"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
)

var markdown = goldmark.New(goldmark.WithExtensions(extension.GFM))

func MarkdownToHTML(source string) string {
	var out bytes.Buffer
	if err := markdown.Convert([]byte(source), &out); err != nil {
		return "<pre>" + html.EscapeString(source) + "</pre>"
	}
	return out.String()
}
