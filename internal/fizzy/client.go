package fizzy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"
)

var ErrNotModified = errors.New("not modified")

type Client struct {
	baseURL      string
	slug         string
	token        string
	sessionToken string
	http         *http.Client
}

func NewClient(baseURL, slug, token string) *Client {
	return &Client{
		baseURL: baseURL,
		slug:    slug,
		token:   token,
		http: &http.Client{
			Timeout:       30 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

// NewSessionClient authenticates with the cookie of a magic-link login.
func NewSessionClient(baseURL, slug, sessionToken string) *Client {
	client := NewClient(baseURL, slug, "")
	client.sessionToken = sessionToken
	return client
}

type APIError struct {
	Method string
	Path   string
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("fizzy: %s %s: status %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (c *Client) accountURL(path string) string {
	return c.baseURL + "/" + c.slug + path
}

func (c *Client) CardURL(number int) string {
	return c.accountURL("/cards/" + strconv.Itoa(number))
}

type response struct {
	header http.Header
	body   []byte
}

func (c *Client) do(ctx context.Context, method, fullURL string, payload any, etag string) (*response, error) {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(encoded)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "fizzy-connector")
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if c.sessionToken != "" {
		req.Header.Set("Cookie", "session_token="+c.sessionToken)
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	data, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if res.StatusCode == http.StatusNotModified {
		return nil, ErrNotModified
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		snippet := string(data)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return nil, &APIError{Method: method, Path: req.URL.Path, Status: res.StatusCode, Body: snippet}
	}
	return &response{header: res.Header, body: data}, nil
}

func (c *Client) get(ctx context.Context, fullURL string, into any) error {
	res, err := c.do(ctx, http.MethodGet, fullURL, nil, "")
	if err != nil {
		return err
	}
	return json.Unmarshal(res.body, into)
}

var nextLinkPattern = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func getAll[T any](ctx context.Context, c *Client, fullURL string) ([]T, error) {
	var all []T
	for fullURL != "" {
		res, err := c.do(ctx, http.MethodGet, fullURL, nil, "")
		if err != nil {
			return nil, err
		}
		var page []T
		if err := json.Unmarshal(res.body, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)

		fullURL = ""
		if match := nextLinkPattern.FindStringSubmatch(res.header.Get("Link")); match != nil {
			fullURL = match[1]
		}
	}
	return all, nil
}

func (c *Client) Identity(ctx context.Context) (*Identity, error) {
	var identity Identity
	return &identity, c.get(ctx, c.baseURL+"/my/identity", &identity)
}

func (c *Client) Users(ctx context.Context) ([]User, error) {
	return getAll[User](ctx, c, c.accountURL("/users"))
}

func (c *Client) Boards(ctx context.Context) ([]Board, error) {
	return getAll[Board](ctx, c, c.accountURL("/boards"))
}

// Notifications returns the first page, which has all unread items first.
// It returns ErrNotModified when the ETag still matches.
func (c *Client) Notifications(ctx context.Context, etag string) ([]Notification, string, error) {
	res, err := c.do(ctx, http.MethodGet, c.accountURL("/notifications"), nil, etag)
	if err != nil {
		return nil, etag, err
	}
	var notifications []Notification
	if err := json.Unmarshal(res.body, &notifications); err != nil {
		return nil, etag, err
	}
	return notifications, res.header.Get("ETag"), nil
}

func (c *Client) MarkNotificationRead(ctx context.Context, id string) error {
	_, err := c.do(ctx, http.MethodPost, c.accountURL("/notifications/"+id+"/reading"), nil, "")
	return err
}

func (c *Client) Card(ctx context.Context, number int) (*Card, error) {
	var card Card
	return &card, c.get(ctx, c.CardURL(number), &card)
}

func (c *Client) Comments(ctx context.Context, number int) ([]Comment, error) {
	return getAll[Comment](ctx, c, c.CardURL(number)+"/comments")
}

func (c *Client) SearchCards(ctx context.Context, terms []string) ([]Card, error) {
	query := url.Values{}
	for _, term := range terms {
		query.Add("terms[]", term)
	}
	var cards []Card
	return cards, c.get(ctx, c.accountURL("/cards")+"?"+query.Encode(), &cards)
}

var commentLocationPattern = regexp.MustCompile(`/comments/([A-Za-z0-9_-]+)`)

// CreateComment returns the id of the new comment.
func (c *Client) CreateComment(ctx context.Context, number int, html string) (string, error) {
	payload := map[string]any{"comment": map[string]string{"body": html}}
	res, err := c.do(ctx, http.MethodPost, c.CardURL(number)+"/comments", payload, "")
	if err != nil {
		return "", err
	}
	if match := commentLocationPattern.FindStringSubmatch(res.header.Get("Location")); match != nil {
		return match[1], nil
	}
	var created Comment
	if json.Unmarshal(res.body, &created) == nil && created.ID != "" {
		return created.ID, nil
	}
	return "", errors.New("fizzy: the new comment has no id")
}

func (c *Client) ReactToComment(ctx context.Context, number int, commentID, content string) error {
	payload := map[string]any{"reaction": map[string]string{"content": content}}
	_, err := c.do(ctx, http.MethodPost, c.CardURL(number)+"/comments/"+commentID+"/reactions", payload, "")
	return err
}

func (c *Client) ReactToCard(ctx context.Context, number int, content string) error {
	payload := map[string]any{"reaction": map[string]string{"content": content}}
	_, err := c.do(ctx, http.MethodPost, c.CardURL(number)+"/reactions", payload, "")
	return err
}

var cardLocationPattern = regexp.MustCompile(`/cards/(\d+)`)

// CreateCard returns the number of the new card.
func (c *Client) CreateCard(ctx context.Context, boardID, title, html string) (int, error) {
	payload := map[string]any{"card": map[string]string{"title": title, "description": html}}
	res, err := c.do(ctx, http.MethodPost, c.accountURL("/boards/"+boardID+"/cards"), payload, "")
	if err != nil {
		return 0, err
	}
	match := cardLocationPattern.FindStringSubmatch(res.header.Get("Location"))
	if match == nil {
		return 0, errors.New("fizzy: the new card has no Location header")
	}
	return strconv.Atoi(match[1])
}

func (c *Client) CloseCard(ctx context.Context, number int) error {
	_, err := c.do(ctx, http.MethodPost, c.CardURL(number)+"/closure", nil, "")
	return err
}

func (c *Client) ReopenCard(ctx context.Context, number int) error {
	_, err := c.do(ctx, http.MethodDelete, c.CardURL(number)+"/closure", nil, "")
	return err
}

func (c *Client) Columns(ctx context.Context, boardID string) ([]Column, error) {
	return getAll[Column](ctx, c, c.accountURL("/boards/"+boardID+"/columns"))
}

func (c *Client) MoveCardToColumn(ctx context.Context, number int, columnID string) error {
	payload := map[string]string{"column_id": columnID}
	_, err := c.do(ctx, http.MethodPost, c.CardURL(number)+"/triage", payload, "")
	return err
}

// ToggleAssignment assigns the user, or removes the user when already assigned.
func (c *Client) ToggleAssignment(ctx context.Context, number int, userID string) error {
	payload := map[string]string{"assignee_id": userID}
	_, err := c.do(ctx, http.MethodPost, c.CardURL(number)+"/assignments", payload, "")
	return err
}

// ToggleTag adds the tag, or removes the tag when the card already has it.
func (c *Client) ToggleTag(ctx context.Context, number int, title string) error {
	payload := map[string]string{"tag_title": title}
	_, err := c.do(ctx, http.MethodPost, c.CardURL(number)+"/taggings", payload, "")
	return err
}

func (c *Client) MoveCardToBoard(ctx context.Context, number int, boardID string) error {
	payload := map[string]string{"board_id": boardID}
	_, err := c.do(ctx, http.MethodPut, c.CardURL(number)+"/board", payload, "")
	return err
}
