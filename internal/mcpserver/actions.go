package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
)

type createCardInput struct {
	Title       string `json:"title" jsonschema:"the card title"`
	Description string `json:"description,omitempty" jsonschema:"the card description, in Markdown"`
	Board       string `json:"board,omitempty" jsonschema:"a board name; empty means the board of the card of this conversation"`
}

type cardInput struct {
	Card string `json:"card,omitempty" jsonschema:"a card number or card URL; empty means the card of this conversation"`
}

type moveCardInput struct {
	Card   string `json:"card,omitempty" jsonschema:"a card number or card URL; empty means the card of this conversation"`
	Column string `json:"column" jsonschema:"the name of a column on the board of the card"`
}

type assignCardInput struct {
	Card string `json:"card,omitempty" jsonschema:"a card number or card URL; empty means the card of this conversation"`
	User string `json:"user" jsonschema:"the name or the email address of a Fizzy user"`
}

type moveToBoardInput struct {
	Card  string `json:"card,omitempty" jsonschema:"a card number or card URL; empty means the card of this conversation"`
	Board string `json:"board" jsonschema:"the name of the target board"`
}

type tagInput struct {
	Card string `json:"card,omitempty" jsonschema:"a card number or card URL; empty means the card of this conversation"`
	Tag  string `json:"tag" jsonschema:"the tag title"`
}

type commentInput struct {
	Card     string `json:"card" jsonschema:"a card number or card URL"`
	Markdown string `json:"markdown" jsonschema:"the comment, in Markdown"`
}

func (s *Server) addActionTools(server *mcp.Server) {
	mcp.AddTool(server, &mcp.Tool{
		Name:        "create_card",
		Description: "Make a new Fizzy card. Returns the number and the URL of the card.",
	}, s.createCard)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "close_card",
		Description: "Close a card: the work on it is done.",
	}, s.closeCard)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "reopen_card",
		Description: "Open a closed card again.",
	}, s.reopenCard)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_columns",
		Description: "List the columns of the board of a card.",
	}, s.listColumns)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "move_card",
		Description: "Move a card to a column of its board.",
	}, s.moveCard)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_boards",
		Description: "List the boards that you have access to.",
	}, s.listBoards)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "move_card_to_board",
		Description: "Move a card to a different board.",
	}, s.moveCardToBoard)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "assign_card",
		Description: "Assign a card to a Fizzy user.",
	}, s.assignCard)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "add_tag",
		Description: "Add a tag to a card. Fizzy makes the tag if it does not exist.",
	}, s.addTag)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "remove_tag",
		Description: "Remove a tag from a card.",
	}, s.removeTag)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "comment_on_card",
		Description: "Post a comment on a different card. For the answer on the card of this conversation, use reply.",
	}, s.commentOnCard)
}

func (s *Server) resolveCard(reference string) (int, error) {
	if strings.TrimSpace(reference) == "" {
		return s.card, nil
	}
	return s.parseCardReference(reference)
}

func (s *Server) createCard(ctx context.Context, _ *mcp.CallToolRequest, in createCardInput) (*mcp.CallToolResult, any, error) {
	boardID, err := s.resolveBoard(ctx, in.Board)
	if err != nil {
		return nil, nil, err
	}
	number, err := s.client.CreateCard(ctx, boardID, in.Title, fizzy.MarkdownToHTML(in.Description))
	if err != nil {
		return nil, nil, err
	}
	return text("Made card #%d: %s", number, s.client.CardURL(number)), nil, nil
}

func (s *Server) resolveBoard(ctx context.Context, name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		card, err := s.client.Card(ctx, s.card)
		if err != nil {
			return "", err
		}
		return card.Board.ID, nil
	}

	boards, err := s.client.Boards(ctx)
	if err != nil {
		return "", err
	}
	var names, matches []string
	for _, board := range boards {
		if strings.EqualFold(board.Name, strings.TrimSpace(name)) {
			matches = append(matches, board.ID)
		}
		names = append(names, board.Name)
	}
	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return "", fmt.Errorf("no board with the name %q; the boards are: %s", name, strings.Join(names, ", "))
	}
	return "", fmt.Errorf("%d boards have the name %q, so the target is not clear: ask the person to rename one of them", len(matches), name)
}

func (s *Server) closeCard(ctx context.Context, _ *mcp.CallToolRequest, in cardInput) (*mcp.CallToolResult, any, error) {
	number, err := s.resolveCard(in.Card)
	if err != nil {
		return nil, nil, err
	}
	if err := s.client.CloseCard(ctx, number); err != nil {
		return nil, nil, err
	}
	return text("Closed card #%d.", number), nil, nil
}

func (s *Server) reopenCard(ctx context.Context, _ *mcp.CallToolRequest, in cardInput) (*mcp.CallToolResult, any, error) {
	number, err := s.resolveCard(in.Card)
	if err != nil {
		return nil, nil, err
	}
	if err := s.client.ReopenCard(ctx, number); err != nil {
		return nil, nil, err
	}
	return text("Opened card #%d again.", number), nil, nil
}

func (s *Server) columnsOf(ctx context.Context, reference string) (int, []fizzy.Column, error) {
	number, err := s.resolveCard(reference)
	if err != nil {
		return 0, nil, err
	}
	card, err := s.client.Card(ctx, number)
	if err != nil {
		return 0, nil, err
	}
	columns, err := s.client.Columns(ctx, card.Board.ID)
	return number, columns, err
}

func columnNames(columns []fizzy.Column) string {
	names := make([]string, len(columns))
	for i, column := range columns {
		names[i] = column.Name
	}
	return strings.Join(names, ", ")
}

func (s *Server) listColumns(ctx context.Context, _ *mcp.CallToolRequest, in cardInput) (*mcp.CallToolResult, any, error) {
	_, columns, err := s.columnsOf(ctx, in.Card)
	if err != nil {
		return nil, nil, err
	}
	if len(columns) == 0 {
		return text("The board has no columns."), nil, nil
	}
	return text("%s", columnNames(columns)), nil, nil
}

func (s *Server) moveCard(ctx context.Context, _ *mcp.CallToolRequest, in moveCardInput) (*mcp.CallToolResult, any, error) {
	number, columns, err := s.columnsOf(ctx, in.Card)
	if err != nil {
		return nil, nil, err
	}
	var matches []fizzy.Column
	for _, column := range columns {
		if strings.EqualFold(column.Name, strings.TrimSpace(in.Column)) {
			matches = append(matches, column)
		}
	}
	switch len(matches) {
	case 0:
		return nil, nil, fmt.Errorf("no column with the name %q; the columns are: %s", in.Column, columnNames(columns))
	case 1:
	default:
		return nil, nil, fmt.Errorf("%d columns have the name %q, so the target is not clear", len(matches), in.Column)
	}
	if err := s.client.MoveCardToColumn(ctx, number, matches[0].ID); err != nil {
		return nil, nil, err
	}
	return text("Moved card #%d to %q.", number, matches[0].Name), nil, nil
}

func (s *Server) assignCard(ctx context.Context, _ *mcp.CallToolRequest, in assignCardInput) (*mcp.CallToolResult, any, error) {
	number, err := s.resolveCard(in.Card)
	if err != nil {
		return nil, nil, err
	}
	users, err := s.client.Users(ctx)
	if err != nil {
		return nil, nil, err
	}

	// An exact email address wins, so that a name that contains the email
	// address of a different person cannot make the target unclear.
	wanted := strings.ToLower(strings.TrimSpace(in.User))
	var matches, byName []fizzy.User
	for _, user := range users {
		switch {
		case !user.Active:
		case strings.ToLower(user.EmailAddress) == wanted:
			matches = append(matches, user)
		case strings.Contains(strings.ToLower(user.Name), wanted):
			byName = append(byName, user)
		}
	}
	if len(matches) == 0 {
		matches = byName
	}
	if len(matches) != 1 {
		return nil, nil, fmt.Errorf("%d users match %q; give the full name or the email address", len(matches), in.User)
	}

	card, err := s.client.Card(ctx, number)
	if err != nil {
		return nil, nil, err
	}
	for _, assignee := range card.Assignees {
		if assignee.ID == matches[0].ID {
			return text("Card #%d is already assigned to %s.", number, matches[0].Name), nil, nil
		}
	}
	if err := s.client.ToggleAssignment(ctx, number, matches[0].ID); err != nil {
		return nil, nil, err
	}
	return text("Assigned card #%d to %s.", number, matches[0].Name), nil, nil
}

func (s *Server) commentOnCard(ctx context.Context, _ *mcp.CallToolRequest, in commentInput) (*mcp.CallToolResult, any, error) {
	number, err := s.parseCardReference(in.Card)
	if err != nil {
		return nil, nil, err
	}
	if number == s.card {
		return nil, nil, fmt.Errorf("card #%d is the card of this conversation: use reply", number)
	}
	if _, err := s.client.CreateComment(ctx, number, fizzy.MarkdownToHTML(in.Markdown)); err != nil {
		return nil, nil, err
	}
	return text("Comment posted on card #%d.", number), nil, nil
}

func (s *Server) addTag(ctx context.Context, _ *mcp.CallToolRequest, in tagInput) (*mcp.CallToolResult, any, error) {
	return s.setTag(ctx, in, true)
}

func (s *Server) removeTag(ctx context.Context, _ *mcp.CallToolRequest, in tagInput) (*mcp.CallToolResult, any, error) {
	return s.setTag(ctx, in, false)
}

func (s *Server) setTag(ctx context.Context, in tagInput, wanted bool) (*mcp.CallToolResult, any, error) {
	number, err := s.resolveCard(in.Card)
	if err != nil {
		return nil, nil, err
	}
	title := strings.TrimPrefix(strings.TrimSpace(in.Tag), "#")
	if title == "" {
		return nil, nil, errors.New("the tag is empty")
	}
	card, err := s.client.Card(ctx, number)
	if err != nil {
		return nil, nil, err
	}

	tagged := false
	for _, tag := range card.Tags {
		if strings.EqualFold(tag, title) {
			tagged = true
		}
	}
	if tagged == wanted {
		if wanted {
			return text("No change: card #%d already had the tag %q.", number, title), nil, nil
		}
		return text("No change: card #%d did not have the tag %q.", number, title), nil, nil
	}
	if err := s.client.ToggleTag(ctx, number, title); err != nil {
		return nil, nil, err
	}
	if wanted {
		return text("Added the tag %q to card #%d.", title, number), nil, nil
	}
	return text("Removed the tag %q from card #%d.", title, number), nil, nil
}

func (s *Server) listBoards(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
	boards, err := s.client.Boards(ctx)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, len(boards))
	for i, board := range boards {
		names[i] = board.Name
	}
	return text("%s", strings.Join(names, "\n")), nil, nil
}

func (s *Server) moveCardToBoard(ctx context.Context, _ *mcp.CallToolRequest, in moveToBoardInput) (*mcp.CallToolResult, any, error) {
	number, err := s.resolveCard(in.Card)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.Board) == "" {
		return nil, nil, errors.New("the board name is empty")
	}
	boardID, err := s.resolveBoard(ctx, in.Board)
	if err != nil {
		return nil, nil, err
	}
	if err := s.client.MoveCardToBoard(ctx, number, boardID); err != nil {
		return nil, nil, err
	}
	return text("Moved card #%d to the board %q.", number, strings.TrimSpace(in.Board)), nil, nil
}
