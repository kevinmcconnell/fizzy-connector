package fizzy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strings"
)

type MagicLink struct {
	PendingToken string

	// DevelopmentCode is set only by a Fizzy development server.
	DevelopmentCode string
}

// RequestMagicLink asks Fizzy to email a login code.
func (c *Client) RequestMagicLink(ctx context.Context, email string) (*MagicLink, error) {
	var result struct {
		Token string `json:"pending_authentication_token"`
	}
	header, err := c.sessionPost(ctx, "/session", map[string]string{"email_address": email}, "", &result)
	if err != nil {
		return nil, err
	}
	return &MagicLink{PendingToken: result.Token, DevelopmentCode: header.Get("X-Magic-Link-Code")}, nil
}

// Join adds the email address to the account of an invite link.
func (c *Client) Join(ctx context.Context, inviteCode, email string) error {
	_, err := c.sessionPost(ctx, "/"+c.slug+"/join/"+inviteCode, map[string]string{"email_address": email}, "", nil)
	return err
}

// VerifyUser has no JSON response: Fizzy answers with a redirect to the next
// page of the join flow.
func (c *Client) VerifyUser(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPost, c.accountURL("/users/verifications"), nil, "")
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusFound {
		return nil
	}
	return err
}

func (c *Client) RenameUser(ctx context.Context, userID, name string) error {
	payload := map[string]any{"user": map[string]string{"name": name}}
	_, err := c.do(ctx, http.MethodPut, c.accountURL("/users/"+userID), payload, "")
	return err
}

func (c *Client) CreateAccessToken(ctx context.Context, description string) (string, error) {
	payload := map[string]any{"access_token": map[string]string{"description": description, "permission": "write"}}
	res, err := c.do(ctx, http.MethodPost, c.accountURL("/my/access_tokens"), payload, "")
	if err != nil {
		return "", err
	}
	var result struct {
		Token string `json:"token"`
	}
	return result.Token, json.Unmarshal(res.body, &result)
}

func (c *Client) SubmitMagicLink(ctx context.Context, pendingToken, code string) (string, error) {
	var result struct {
		Token string `json:"session_token"`
	}
	cookie := "pending_authentication_token=" + pendingToken
	_, err := c.sessionPost(ctx, "/session/magic_link", map[string]string{"code": code}, cookie, &result)
	return result.Token, err
}

func (c *Client) sessionPost(ctx context.Context, path string, payload any, cookie string, into any) (http.Header, error) {
	encoded, _ := json.Marshal(payload)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if res.StatusCode < 200 || res.StatusCode > 299 {
		return nil, &APIError{Method: http.MethodPost, Path: path, Status: res.StatusCode, Body: string(data)}
	}
	if into == nil {
		return res.Header, nil
	}
	return res.Header, json.Unmarshal(data, into)
}

var streamNamePattern = regexp.MustCompile(`signed-stream-name="([^"]+)"`)

// NotificationStreamName reads the signed Turbo stream name for the
// notifications of the session user from the Fizzy HTML.
func (c *Client) NotificationStreamName(ctx context.Context, sessionToken string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.accountURL(""), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Cookie", "session_token="+sessionToken)

	following := *c.http
	following.CheckRedirect = nil
	res, err := following.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK || strings.Contains(res.Request.URL.Path, "/session") {
		return "", fmt.Errorf("session cookie not accepted (status %d): run `fizzy-connector login`", res.StatusCode)
	}

	page, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return "", err
	}
	for _, match := range streamNamePattern.FindAllSubmatch(page, -1) {
		name := html.UnescapeString(string(match[1]))
		payload, _, _ := strings.Cut(name, "--")
		if strings.HasSuffix(strings.Trim(string(decodeBase64(payload)), `"`), ":notifications") {
			return name, nil
		}
	}
	return "", errors.New("notification stream name not found in the Fizzy page")
}
