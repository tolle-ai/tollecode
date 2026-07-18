package agent

// tools_calendar.go — list_events and create_event via the Google Calendar REST API.
//
// Auth: OAuth2 refresh-token flow. The token is stored per-workspace at
// .agent/calendar_token.json after the user completes the one-time setup.
//
// One-time setup (run once per workspace):
//
//  1. Create a Google Cloud project, enable the Calendar API, create OAuth2 credentials
//     (Desktop app type), download client_secret.json.
//  2. Run: tollecode configure-calendar --workspace <path> --client-secret client_secret.json
//     (or use the /v1/calendar/auth REST endpoint — not yet wired, placeholder below).
//  3. A browser URL is printed; visit it, approve, paste the code back → token saved.
//
// The refresh token is exchanged for a fresh access token automatically on each tool call.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// calendarToken is stored at {workspace}/.agent/calendar_token.json
type calendarToken struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	AccessToken  string `json:"access_token"`
	Expiry       int64  `json:"expiry"` // Unix seconds
}

func calendarTokenPath(workspace string) string {
	return filepath.Join(workspace, ".agent", "calendar_token.json")
}

func loadCalendarToken(workspace string) (*calendarToken, error) {
	if workspace == "" {
		return nil, fmt.Errorf("workspace is required")
	}
	data, err := os.ReadFile(calendarTokenPath(workspace))
	if err != nil {
		return nil, err
	}
	var t calendarToken
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, err
	}
	if t.RefreshToken == "" {
		return nil, fmt.Errorf("calendar_token.json has no refresh_token")
	}
	return &t, nil
}

func saveCalendarToken(workspace string, t *calendarToken) {
	data, _ := json.MarshalIndent(t, "", "  ")
	_ = os.WriteFile(calendarTokenPath(workspace), data, 0o600)
}

// freshAccessToken returns a valid access token, refreshing if expired.
func freshAccessToken(workspace string, t *calendarToken) (string, error) {
	if t.AccessToken != "" && time.Now().Unix() < t.Expiry-60 {
		return t.AccessToken, nil
	}
	// Exchange refresh token for a new access token.
	params := url.Values{
		"client_id":     {t.ClientID},
		"client_secret": {t.ClientSecret},
		"refresh_token": {t.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	resp, err := http.PostForm("https://oauth2.googleapis.com/token", params)
	if err != nil {
		return "", fmt.Errorf("token refresh: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	var result struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("token refresh parse: %w", err)
	}
	if result.Error != "" {
		return "", fmt.Errorf("token refresh error: %s", result.Error)
	}
	t.AccessToken = result.AccessToken
	t.Expiry = time.Now().Unix() + int64(result.ExpiresIn)
	saveCalendarToken(workspace, t)
	return t.AccessToken, nil
}

// ── list_events ───────────────────────────────────────────────────────────────

func toolListEvents(workspace string, inp map[string]any) string {
	tok, err := loadCalendarToken(workspace)
	if err != nil {
		return "Error: calendar not configured. Run the calendar setup flow first. " + err.Error()
	}
	access, err := freshAccessToken(workspace, tok)
	if err != nil {
		return "Error getting calendar access: " + err.Error()
	}

	calID, _ := inp["calendar_id"].(string)
	if calID == "" {
		calID = "primary"
	}
	from, _ := inp["from"].(string)
	to, _ := inp["to"].(string)
	maxF, _ := inp["max_results"].(float64)
	max := int(maxF)
	if max <= 0 {
		max = 10
	}

	now := time.Now().UTC()
	if from == "" {
		from = now.Format(time.RFC3339)
	}
	if to == "" {
		to = now.Add(7 * 24 * time.Hour).Format(time.RFC3339)
	}

	q := url.Values{
		"timeMin":      {from},
		"timeMax":      {to},
		"maxResults":   {fmt.Sprintf("%d", max)},
		"singleEvents": {"true"},
		"orderBy":      {"startTime"},
	}
	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events?%s",
		url.PathEscape(calID), q.Encode())

	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, apiURL, nil)
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "Error calling Calendar API: " + err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	var result struct {
		Items []struct {
			Summary  string `json:"summary"`
			Location string `json:"location"`
			Start    struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"start"`
			End struct {
				DateTime string `json:"dateTime"`
				Date     string `json:"date"`
			} `json:"end"`
			Description string `json:"description"`
		} `json:"items"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "Error parsing calendar response: " + err.Error()
	}
	if result.Error.Message != "" {
		return "Calendar API error: " + result.Error.Message
	}
	if len(result.Items) == 0 {
		return "No events found in the specified time range."
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("%d event(s):\n\n", len(result.Items)))
	for i, ev := range result.Items {
		start := ev.Start.DateTime
		if start == "" {
			start = ev.Start.Date
		}
		end := ev.End.DateTime
		if end == "" {
			end = ev.End.Date
		}
		sb.WriteString(fmt.Sprintf("[%d] %s\n  Start: %s\n  End:   %s\n", i+1, ev.Summary, start, end))
		if ev.Location != "" {
			sb.WriteString("  Location: " + ev.Location + "\n")
		}
		if ev.Description != "" {
			desc := ev.Description
			if len(desc) > 200 {
				desc = desc[:200] + "…"
			}
			sb.WriteString("  Description: " + desc + "\n")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

// ── create_event ──────────────────────────────────────────────────────────────

func toolCreateEvent(workspace string, inp map[string]any) string {
	tok, err := loadCalendarToken(workspace)
	if err != nil {
		return "Error: calendar not configured. Run the calendar setup flow first. " + err.Error()
	}
	access, err := freshAccessToken(workspace, tok)
	if err != nil {
		return "Error getting calendar access: " + err.Error()
	}

	title, _ := inp["title"].(string)
	start, _ := inp["start"].(string)
	end, _ := inp["end"].(string)
	desc, _ := inp["description"].(string)
	location, _ := inp["location"].(string)
	calID, _ := inp["calendar_id"].(string)
	if calID == "" {
		calID = "primary"
	}

	if title == "" {
		return "Error: 'title' is required."
	}
	if start == "" {
		return "Error: 'start' is required (RFC3339 format)."
	}
	if end == "" {
		return "Error: 'end' is required (RFC3339 format)."
	}

	event := map[string]any{
		"summary": title,
		"start":   map[string]string{"dateTime": start},
		"end":     map[string]string{"dateTime": end},
	}
	if desc != "" {
		event["description"] = desc
	}
	if location != "" {
		event["location"] = location
	}

	body, _ := json.Marshal(event)
	apiURL := fmt.Sprintf("https://www.googleapis.com/calendar/v3/calendars/%s/events",
		url.PathEscape(calID))
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, apiURL, bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "Error calling Calendar API: " + err.Error()
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)

	var result struct {
		ID      string `json:"id"`
		Summary string `json:"summary"`
		HtmlLink string `json:"htmlLink"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "Error parsing response: " + err.Error()
	}
	if result.Error.Message != "" {
		return "Calendar API error: " + result.Error.Message
	}
	return fmt.Sprintf("Event created: %s\nLink: %s", result.Summary, result.HtmlLink)
}
