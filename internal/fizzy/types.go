package fizzy

import "time"

type User struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	Active       bool   `json:"active"`
	EmailAddress string `json:"email_address"`
}

type Account struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
	User User   `json:"user"`
}

type Identity struct {
	ID       string    `json:"id"`
	Accounts []Account `json:"accounts"`
}

type Board struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type BoardAccess struct {
	User
	HasAccess bool `json:"has_access"`
}

type Column struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type Card struct {
	ID              string    `json:"id"`
	Number          int       `json:"number"`
	Title           string    `json:"title"`
	Status          string    `json:"status"`
	Description     string    `json:"description"`
	DescriptionHTML string    `json:"description_html"`
	Tags            []string  `json:"tags"`
	Closed          bool      `json:"closed"`
	URL             string    `json:"url"`
	Board           Board     `json:"board"`
	Creator         User      `json:"creator"`
	Column          *Column   `json:"column"`
	Assignees       []User    `json:"assignees"`
	CreatedAt       time.Time `json:"created_at"`
}

type Body struct {
	PlainText string `json:"plain_text"`
	HTML      string `json:"html"`
}

type Comment struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Body      Body      `json:"body"`
	Creator   User      `json:"creator"`
	URL       string    `json:"url"`
}

type NotificationCard struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
}

type Notification struct {
	ID         string           `json:"id"`
	Read       bool             `json:"read"`
	SourceType string           `json:"source_type"`
	Body       string           `json:"body"`
	Creator    User             `json:"creator"`
	Card       NotificationCard `json:"card"`
}

func (n Notification) IsMention() bool { return n.SourceType == "mention" }
