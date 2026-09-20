package daemon

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/kevinmcconnell/fizzy-connector/internal/claude"
)

func TestTheWarningIsATenthOfTheTimeoutWithinLimits(t *testing.T) {
	assert.Equal(t, 2*time.Minute, warningBefore(10*time.Minute))
	assert.Equal(t, 3*time.Minute, warningBefore(30*time.Minute))
	assert.Equal(t, 10*time.Minute, warningBefore(2*time.Hour))
	assert.Equal(t, 10*time.Minute, warningBefore(5*time.Hour))
}

func TestAStoppedTurnGetsAReplyThatSaysSo(t *testing.T) {
	result := claude.Result{Usage: claude.Usage{APICalls: 54}}
	err := fmt.Errorf("turn stopped: %w", context.DeadlineExceeded)

	reply := fallbackReply(result, err, 30*time.Minute, 100)

	assert.Contains(t, reply, "turn timeout of 30m0s")
	assert.Contains(t, reply, "54 API calls")
}

func TestATurnOverTheCostLimitGetsAReplyThatSaysSo(t *testing.T) {
	result := claude.Result{Usage: claude.Usage{APICalls: 54, CostUSD: 101.5}}
	err := fmt.Errorf("turn stopped: %w", claude.ErrCostLimit)

	reply := fallbackReply(result, err, 30*time.Minute, 100)

	assert.Contains(t, reply, "cost limit of $100.00")
	assert.Contains(t, reply, "54 API calls")
	assert.Contains(t, reply, "estimated $101.50")
}
