package daemon

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"github.com/kevinmcconnell/fizzy-connector/internal/config"
	"github.com/kevinmcconnell/fizzy-connector/internal/fizzy"
	"github.com/kevinmcconnell/fizzy-connector/internal/store"
)

// Fizzy cuts the body of a notification to this number of characters.
const notificationBodyLimit = 200

type trigger struct {
	item    store.Item
	author  fizzy.User
	trusted bool
}

// mentionEvidence comes from a mention notification: the person who made the
// mention, and the text that has the mention, cut to 200 characters.
type mentionEvidence struct {
	mentioner fizzy.User
	body      string
}

func (e *mentionEvidence) isComplete() bool {
	return len([]rune(e.body)) < notificationBodyLimit
}

// provesAll reports that the complete text is what the mentioner wrote.
func (e *mentionEvidence) provesAll(plainText string) bool {
	body := strings.TrimSpace(e.body)
	return e.isComplete() && body != "" && body == strings.TrimSpace(plainText)
}

// provesStartOf reports a text that is too long to prove: its start is what
// the mentioner wrote, but all people with board access can change the rest.
func (e *mentionEvidence) provesStartOf(plainText string) bool {
	return !e.isComplete() && strings.HasPrefix(strings.TrimSpace(plainText), strings.TrimSuffix(strings.TrimSpace(e.body), "..."))
}

// descriptionID is different for each text of a description, so that a
// mention which is refused does not block a later one.
func descriptionID(prefix, plainText string) string {
	digest := sha256.Sum256([]byte(plainText))
	return prefix + ":" + hex.EncodeToString(digest[:6])
}

// findTriggers returns the mentions of the bot on a card that are not
// handled yet.
//
// The author of a comment comes from the API. A description has no author:
// all people with board access can edit it. So a mention in a description
// counts only when the notification proves the complete text. The queue item
// keeps that text, and the turn uses it and not a later state of the card.
// While a permission question is open, a mention that answers it is not a
// trigger.
func findTriggers(cfg *config.Config, state *store.CardState, card *fizzy.Card, comments []fizzy.Comment, evidence *mentionEvidence, approvalPending bool) []trigger {
	var triggers []trigger

	add := func(item store.Item, html string, author fizzy.User) {
		if state.IsHandled(item.TriggerID) || author.ID == cfg.BotUserID || !fizzy.Mentions(html, cfg.BotUserID) {
			return
		}
		item.AuthorID, item.AuthorName = author.ID, author.Name
		triggers = append(triggers, trigger{item: item, author: author, trusted: cfg.IsTrusted(author.ID)})
	}

	if evidence != nil && evidence.provesAll(card.Description) {
		add(store.Item{
			Kind:      store.KindDescription,
			TriggerID: descriptionID(store.DescriptionTriggerID, card.Description),
			Text:      card.Description,
		}, card.DescriptionHTML, evidence.mentioner)
	}
	for _, comment := range comments {
		if approvalPending && isApprovalAnswer(fizzy.PlainText(comment.Body.HTML)) {
			continue
		}
		add(store.Item{Kind: store.KindComment, TriggerID: comment.ID}, comment.Body.HTML, comment.Creator)
	}
	return triggers
}

// unprovableDescription returns an id when a trusted person mentioned the bot
// in a description that is too long to prove. The daemon then tells the
// person to use a comment.
func unprovableDescription(cfg *config.Config, state *store.CardState, card *fizzy.Card, evidence *mentionEvidence) (string, bool) {
	if evidence == nil || !cfg.IsTrusted(evidence.mentioner.ID) || !evidence.provesStartOf(card.Description) ||
		!fizzy.Mentions(card.DescriptionHTML, cfg.BotUserID) {
		return "", false
	}
	id := descriptionID("long-description", card.Description)
	return id, !state.IsHandled(id)
}
