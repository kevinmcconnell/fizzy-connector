// Package store keeps the state of each card as one JSON file.
package store

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	KindComment      = "comment"
	KindDescription  = "description"
	KindAgentMessage = "agent_message"

	DescriptionTriggerID = "description"
)

// Item is one unit of work in the queue of a card.
type Item struct {
	Kind       string `json:"kind"`
	TriggerID  string `json:"trigger_id,omitempty"`
	AuthorID   string `json:"author_id,omitempty"`
	AuthorName string `json:"author_name,omitempty"`
	FromCard   int    `json:"from_card,omitempty"`
	Text       string `json:"text,omitempty"`
	Hops       int    `json:"hops,omitempty"`
}

type CardState struct {
	Number          int          `json:"number"`
	SessionID       string       `json:"session_id"`
	SessionStarted  bool         `json:"session_started"`
	Handled         []string     `json:"handled"`
	Queue           []Item       `json:"queue"`
	PendingReply    string       `json:"pending_reply,omitempty"`
	NeedsScan       bool         `json:"needs_scan,omitempty"`
	AllowedCommands []string     `json:"allowed_commands,omitempty"`
	PromptedUntil   time.Time    `json:"prompted_until"`
	LastTurnAt      time.Time    `json:"last_turn_at"`
	TurnCount       int          `json:"turn_count,omitempty"`
	TotalCostUSD    float64      `json:"total_cost_usd,omitempty"`
	Turns           []TurnRecord `json:"turns,omitempty"`
}

const maxTurnRecords = 50

// TurnRecord has the usage of one turn. The cost is an estimate at API prices.
type TurnRecord struct {
	At               time.Time `json:"at"`
	Seconds          int       `json:"seconds"`
	Resumed          bool      `json:"resumed"`
	CostUSD          float64   `json:"cost_usd"`
	APICalls         int       `json:"api_calls"`
	InputTokens      int       `json:"input_tokens"`
	CacheWriteTokens int       `json:"cache_write_tokens"`
	CacheReadTokens  int       `json:"cache_read_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	ContextTokens    int       `json:"context_tokens,omitempty"`
}

// RecordTurn adds to the totals, and keeps the newest turns in detail.
func (s *CardState) RecordTurn(record TurnRecord) {
	s.TurnCount++
	s.TotalCostUSD += record.CostUSD
	s.Turns = append(s.Turns, record)
	if len(s.Turns) > maxTurnRecords {
		s.Turns = s.Turns[len(s.Turns)-maxTurnRecords:]
	}
}

// ResetSession makes the next turn start a new session with the full card.
func (s *CardState) ResetSession() {
	s.SessionID = uuid.NewString()
	s.SessionStarted = false
	s.PromptedUntil = time.Time{}
}

func (s *CardState) IsHandled(triggerID string) bool {
	return slices.Contains(s.Handled, triggerID)
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func Open(dataDir string) (*Store, error) {
	dir := filepath.Join(dataDir, "cards")
	return &Store{dir: dir}, os.MkdirAll(dir, 0o700)
}

func (s *Store) path(number int) string {
	return filepath.Join(s.dir, strconv.Itoa(number)+".json")
}

func (s *Store) Get(number int) (*CardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read(number)
}

// Update changes the state of one card atomically.
func (s *Store) Update(number int, change func(*CardState)) (*CardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	state, err := s.read(number)
	if err != nil {
		return nil, err
	}
	change(state)
	return state, s.write(state)
}

func (s *Store) All() ([]*CardState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var states []*CardState
	for _, entry := range entries {
		number, err := strconv.Atoi(strings.TrimSuffix(entry.Name(), ".json"))
		if err != nil {
			continue
		}
		state, err := s.read(number)
		if err != nil {
			return nil, err
		}
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].Number < states[j].Number })
	return states, nil
}

func (s *Store) read(number int) (*CardState, error) {
	state := &CardState{Number: number}
	data, err := os.ReadFile(s.path(number))
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return nil, err
	}
	return state, json.Unmarshal(data, state)
}

func (s *Store) write(state *CardState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	temp := s.path(state.Number) + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, s.path(state.Number))
}
