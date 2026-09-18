package claude

import "strings"

// price is in dollars for one million tokens. Cache writes are for the one
// hour cache that Claude Code uses.
type price struct {
	input, cacheWrite, cacheRead, output float64
}

func (p price) cost(t tokens) float64 {
	return (float64(t.InputTokens)*p.input + float64(t.CacheWriteTokens)*p.cacheWrite +
		float64(t.CacheReadTokens)*p.cacheRead + float64(t.OutputTokens)*p.output) / 1e6
}

// prices are the API prices of the model families, newest first. A model
// that is not here has no estimate: the cost then comes from Claude Code only.
var prices = []struct {
	prefix string
	price  price
}{
	{"claude-fable-5", price{10, 20, 0.25, 50}},
	{"claude-mythos-5", price{10, 20, 0.25, 50}},
	{"claude-opus-5", price{5, 10, 0.5, 25}},
	{"claude-opus-4", price{5, 10, 0.5, 25}},
	{"claude-sonnet-5", price{2, 4, 0.2, 10}},
	{"claude-sonnet-4", price{3, 6, 0.3, 15}},
	{"claude-haiku-4", price{1, 2, 0.1, 5}},
}

func priceOf(model string) price {
	for _, p := range prices {
		if strings.HasPrefix(model, p.prefix) {
			return p.price
		}
	}
	return price{}
}
