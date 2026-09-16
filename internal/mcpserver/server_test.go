package mcpserver

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/kevinmcconnell/fizzy-connector/internal/config"
)

func TestParseCardReference(t *testing.T) {
	server := &Server{cfg: &config.Config{BaseURL: "https://fizzy.example", AccountSlug: "123"}}

	accepted := map[string]int{
		"42":                                 42,
		"#42":                                42,
		" 7 ":                                7,
		"https://fizzy.example/123/cards/42": 42,
		"https://fizzy.example/123/cards/42#comment_abc": 42,
		"https://fizzy.example/123/cards/42/comments/9":  42,
	}
	for reference, want := range accepted {
		got, err := server.parseCardReference(reference)
		assert.NoError(t, err, reference)
		assert.Equal(t, want, got, reference)
	}

	refused := []string{
		"42garbage",
		"card 42",
		"https://other.example/123/cards/42",
		"https://fizzy.example/999/cards/42",
		"https://fizzy.example/123/cards/42x",
		"https://fizzy.example.evil.test/123/cards/42",
	}
	for _, reference := range refused {
		_, err := server.parseCardReference(reference)
		assert.Error(t, err, reference)
	}
}
