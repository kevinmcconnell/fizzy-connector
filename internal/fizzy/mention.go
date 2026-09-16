package fizzy

import (
	"encoding/base64"
	"encoding/json"
	"regexp"
	"slices"
	"strings"
)

const mentionContentType = "application/vnd.actiontext.mention"

var (
	attachmentPattern = regexp.MustCompile(`<action-text-attachment\b[^>]*>`)
	attributePattern  = regexp.MustCompile(`([a-zA-Z-]+)="([^"]*)"`)
	userGIDPattern    = regexp.MustCompile(`gid://[^/]+/User/([A-Za-z0-9_-]+)`)
)

// MentionedUserIDs returns the ids of the users that the rich text mentions.
func MentionedUserIDs(html string) []string {
	var ids []string
	for _, tag := range attachmentPattern.FindAllString(html, -1) {
		attributes := map[string]string{}
		for _, match := range attributePattern.FindAllStringSubmatch(tag, -1) {
			attributes[match[1]] = match[2]
		}
		if attributes["content-type"] != mentionContentType {
			continue
		}
		if id := userIDFromSGID(attributes["sgid"]); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

func Mentions(html, userID string) bool {
	return slices.Contains(MentionedUserIDs(html), userID)
}

// userIDFromSGID reads the payload of a Rails signed global id. It does not
// verify the signature: Fizzy made the HTML, and we fetch it with our token.
func userIDFromSGID(sgid string) string {
	payload, _, _ := strings.Cut(sgid, "--")
	decoded := decodeBase64(payload)
	if decoded == nil {
		return ""
	}
	if match := userGIDPattern.FindSubmatch(decoded); match != nil {
		return string(match[1])
	}

	var envelope struct {
		Rails struct {
			Message string `json:"message"`
			Data    string `json:"data"`
		} `json:"_rails"`
	}
	if json.Unmarshal(decoded, &envelope) != nil {
		return ""
	}
	for _, inner := range []string{envelope.Rails.Message, envelope.Rails.Data} {
		if match := userGIDPattern.FindSubmatch(decodeBase64(inner)); match != nil {
			return string(match[1])
		}
	}
	return ""
}

func decodeBase64(text string) []byte {
	text = strings.TrimRight(text, "=")
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(text); err == nil {
			return decoded
		}
	}
	return nil
}
