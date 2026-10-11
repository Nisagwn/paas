package notify

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Message formats: Slack blocks with a colored attachment bar, Discord
// embeds, the generic webhook JSON and the email text.

// Status colors (Slack's palette, readable on light and dark themes).
var severityColors = map[string]int{
	SeveritySuccess: 0x2EB67D,
	SeverityInfo:    0x1D9BD1,
	SeverityWarning: 0xECB22E,
	SeverityError:   0xE01E5A,
}

func colorOf(severity string) int {
	if c, ok := severityColors[severity]; ok {
		return c
	}
	return severityColors[SeverityInfo]
}

// footer names where the message came from.
func footer(e Event) string {
	if e.Team != "" {
		return "paas · " + e.Team + " ekibi · " + EventLabels[e.Type]
	}
	return "paas · " + EventLabels[e.Type]
}

// slackEscape escapes the three characters Slack's mrkdwn treats as markup.
func slackEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// slackURL keeps a URL from breaking a <url|text> link.
func slackURL(u string) string {
	return strings.NewReplacer("|", "%7C", ">", "%3E", "<", "%3C", " ", "%20").Replace(u)
}

// SlackPayload renders e for a Slack incoming webhook.
func SlackPayload(e Event) ([]byte, error) {
	title := slackEscape(e.Title)
	if e.URL != "" {
		title = "<" + slackURL(e.URL) + "|" + title + ">"
	}
	main := "*" + title + "*"
	if e.Text != "" {
		main += "\n" + slackEscape(clip(e.Text, 2500))
	}
	blocks := []map[string]any{
		{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": main}},
	}
	if len(e.Fields) > 0 {
		var fields []map[string]any
		for i, f := range e.Fields {
			if i == 10 { // Slack's limit per section
				break
			}
			fields = append(fields, map[string]any{"type": "mrkdwn",
				"text": "*" + slackEscape(f.Name) + "*\n" + slackEscape(clip(f.Value, 1900))})
		}
		blocks = append(blocks, map[string]any{"type": "section", "fields": fields})
	}
	if e.URL != "" {
		label := e.LinkText
		if label == "" {
			label = "Aç"
		}
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []map[string]any{{
			"type": "button", "text": map[string]any{"type": "plain_text", "text": clip(label, 75)}, "url": e.URL,
		}}})
	}
	blocks = append(blocks, map[string]any{"type": "context", "elements": []map[string]any{
		{"type": "mrkdwn", "text": slackEscape(footer(e))},
	}})
	return json.Marshal(map[string]any{
		// text is the notification preview and the fallback.
		"text": clip(e.Title, 300),
		"attachments": []map[string]any{{
			"color":  fmt.Sprintf("#%06X", colorOf(e.Severity)),
			"blocks": blocks,
		}},
	})
}

// DiscordPayload renders e for a Discord webhook. Mentions are disabled,
// so a commit message cannot ping @everyone.
func DiscordPayload(e Event) ([]byte, error) {
	embed := map[string]any{
		"title":     clip(e.Title, 256),
		"color":     colorOf(e.Severity),
		"timestamp": e.At.UTC().Format(time.RFC3339),
		"footer":    map[string]any{"text": clip(footer(e), 2048)},
	}
	if e.Text != "" {
		embed["description"] = clip(e.Text, 4000)
	}
	if e.URL != "" {
		embed["url"] = e.URL
	}
	var fields []map[string]any
	for i, f := range e.Fields {
		if i == 25 {
			break
		}
		v := f.Value
		if v == "" {
			v = "-"
		}
		fields = append(fields, map[string]any{"name": clip(f.Name, 256), "value": clip(v, 1024), "inline": len(v) <= 40})
	}
	if fields != nil {
		embed["fields"] = fields
	}
	return json.Marshal(map[string]any{
		"username":         "paas",
		"embeds":           []any{embed},
		"allowed_mentions": map[string]any{"parse": []string{}},
	})
}

// WebhookBody is the JSON a generic webhook receives.
type WebhookBody struct {
	// ID is the delivery's idempotency key (also X-Paas-Delivery).
	ID           string         `json:"id"`
	Type         string         `json:"type"`
	CreatedAt    time.Time      `json:"created_at"`
	Team         string         `json:"team,omitempty"`
	App          string         `json:"app,omitempty"`
	DeploymentID int64          `json:"deployment_id,omitempty"`
	Severity     string         `json:"severity"`
	Title        string         `json:"title"`
	Text         string         `json:"text,omitempty"`
	URL          string         `json:"url,omitempty"`
	Fields       []Field        `json:"fields,omitempty"`
	Data         map[string]any `json:"data,omitempty"`
}

// WebhookPayload renders e for a generic webhook.
func WebhookPayload(e Event, deliveryID string) ([]byte, error) {
	return json.Marshal(WebhookBody{
		ID: deliveryID, Type: e.Type, CreatedAt: e.At.UTC(), Team: e.Team, App: e.App, DeploymentID: e.DeploymentID,
		Severity: e.Severity, Title: e.Title, Text: e.Text, URL: e.URL, Fields: e.Fields, Data: e.Data,
	})
}

// EmailSubject is the subject line of e.
func EmailSubject(e Event) string {
	return "[paas] " + oneLine(e.Title)
}

// EmailBody is the plain text of e; channel is the sending channel's name.
func EmailBody(e Event, channel string) string {
	var b strings.Builder
	b.WriteString(e.Title + "\n")
	b.WriteString(strings.Repeat("=", min(len([]rune(e.Title)), 72)) + "\n\n")
	if e.Text != "" {
		b.WriteString(e.Text + "\n\n")
	}
	width := 0
	for _, f := range e.Fields {
		width = max(width, len([]rune(f.Name)))
	}
	for _, f := range e.Fields {
		fmt.Fprintf(&b, "%s:%s %s\n", f.Name, strings.Repeat(" ", width-len([]rune(f.Name))), oneLine(f.Value))
	}
	if len(e.Fields) > 0 {
		b.WriteString("\n")
	}
	if e.URL != "" {
		label := e.LinkText
		if label == "" {
			label = "Aç"
		}
		fmt.Fprintf(&b, "%s: %s\n\n", label, e.URL)
	}
	b.WriteString("-- \n")
	fmt.Fprintf(&b, "Bu e-postayı %q bildirim kanalı gönderdi (%s).\n", channel, footer(e))
	if e.ManageURL != "" {
		fmt.Fprintf(&b, "Bildirim tercihlerini değiştirmek için: %s\n", e.ManageURL)
	}
	return b.String()
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
