package agent

// tools_email.go — read_email (IMAP4/TLS) and send_email (SMTP/STARTTLS) tools.
//
// Credentials are stored per-workspace in .agent/email_config.json.
// Tools are only registered when that file exists and is valid.
//
// IMAP implementation is intentionally minimal: LOGIN → SELECT → SEARCH → FETCH → LOGOUT.
// Supports Gmail, Outlook, Fastmail and any RFC 3501-compliant server.

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// emailConfig is stored at {workspace}/.agent/email_config.json
type emailConfig struct {
	IMAPHost string `json:"imap_host"`
	IMAPPort int    `json:"imap_port"`
	SMTPHost string `json:"smtp_host"`
	SMTPPort int    `json:"smtp_port"`
	Username string `json:"username"`
	Password string `json:"password"`
}

func loadEmailConfig(workspace string) (*emailConfig, error) {
	if workspace == "" {
		return nil, fmt.Errorf("workspace is required")
	}
	p := filepath.Join(workspace, ".agent", "email_config.json")
	data, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var cfg emailConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	if cfg.IMAPHost == "" || cfg.Username == "" {
		return nil, fmt.Errorf("email_config.json is incomplete")
	}
	if cfg.IMAPPort == 0 {
		cfg.IMAPPort = 993
	}
	if cfg.SMTPPort == 0 {
		cfg.SMTPPort = 587
	}
	return &cfg, nil
}

// ── read_email ────────────────────────────────────────────────────────────────

func toolReadEmail(workspace string, inp map[string]any) string {
	cfg, err := loadEmailConfig(workspace)
	if err != nil {
		return "Error: email not configured. Create .agent/email_config.json first. " + err.Error()
	}

	folder, _ := inp["folder"].(string)
	if folder == "" {
		folder = "INBOX"
	}
	limitF, _ := inp["limit"].(float64)
	limit := int(limitF)
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	unreadOnly, _ := inp["unread_only"].(bool)
	search, _ := inp["search"].(string)

	client, err := imapDial(cfg)
	if err != nil {
		return "Error connecting to IMAP: " + err.Error()
	}
	defer client.logout()

	if err := client.login(cfg.Username, cfg.Password); err != nil {
		return "Error authenticating: " + err.Error()
	}
	count, err := client.selectFolder(folder)
	if err != nil {
		return "Error selecting folder: " + err.Error()
	}
	if count == 0 {
		return fmt.Sprintf("Folder %s is empty.", folder)
	}

	criteria := "ALL"
	if unreadOnly {
		criteria = "UNSEEN"
	}
	if search != "" {
		criteria = fmt.Sprintf(`%s TEXT "%s"`, criteria, strings.ReplaceAll(search, `"`, `\"`))
	}

	ids, err := client.search(criteria)
	if err != nil {
		return "Error searching: " + err.Error()
	}
	if len(ids) == 0 {
		return "No messages found matching the criteria."
	}

	// Fetch the most recent `limit` message IDs.
	if len(ids) > limit {
		ids = ids[len(ids)-limit:]
	}

	msgs, err := client.fetchEnvelopes(ids)
	if err != nil {
		return "Error fetching messages: " + err.Error()
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d message(s) in %s:\n\n", len(msgs), folder))
	for i, m := range msgs {
		sb.WriteString(fmt.Sprintf("[%d] ID:%d\n  From: %s\n  Subject: %s\n  Date: %s\n  Read: %v\n\n",
			i+1, m.id, m.from, m.subject, m.date, m.seen))
	}
	return sb.String()
}

// ── send_email ────────────────────────────────────────────────────────────────

func toolSendEmail(workspace string, inp map[string]any) string {
	cfg, err := loadEmailConfig(workspace)
	if err != nil {
		return "Error: email not configured. Create .agent/email_config.json first. " + err.Error()
	}

	to, _ := inp["to"].(string)
	subject, _ := inp["subject"].(string)
	body, _ := inp["body"].(string)
	cc, _ := inp["cc"].(string)
	bcc, _ := inp["bcc"].(string)

	if to == "" {
		return "Error: 'to' is required."
	}
	if subject == "" {
		return "Error: 'subject' is required."
	}

	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)

	var recipients []string
	recipients = append(recipients, splitAddresses(to)...)
	if cc != "" {
		recipients = append(recipients, splitAddresses(cc)...)
	}
	if bcc != "" {
		recipients = append(recipients, splitAddresses(bcc)...)
	}

	var headers strings.Builder
	headers.WriteString("From: " + cfg.Username + "\r\n")
	headers.WriteString("To: " + to + "\r\n")
	if cc != "" {
		headers.WriteString("Cc: " + cc + "\r\n")
	}
	headers.WriteString("Subject: " + subject + "\r\n")
	headers.WriteString("Date: " + time.Now().Format(time.RFC1123Z) + "\r\n")
	headers.WriteString("MIME-Version: 1.0\r\n")
	headers.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	headers.WriteString("\r\n")
	headers.WriteString(body)

	if err := smtp.SendMail(addr, auth, cfg.Username, recipients, []byte(headers.String())); err != nil {
		return "Error sending email: " + err.Error()
	}
	return fmt.Sprintf("Email sent to %s.", to)
}

func splitAddresses(s string) []string {
	var out []string
	for _, a := range strings.Split(s, ",") {
		if t := strings.TrimSpace(a); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// ── minimal IMAP4 client ──────────────────────────────────────────────────────

type imapClient struct {
	conn   net.Conn
	reader *bufio.Reader
	seq    int
}

type imapMessage struct {
	id      int
	from    string
	subject string
	date    string
	seen    bool
}

func imapDial(cfg *emailConfig) (*imapClient, error) {
	addr := fmt.Sprintf("%s:%d", cfg.IMAPHost, cfg.IMAPPort)
	tlsCfg := &tls.Config{ServerName: cfg.IMAPHost}
	conn, err := tls.Dial("tcp", addr, tlsCfg)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
	c := &imapClient{conn: conn, reader: bufio.NewReader(conn)}
	// Read the server greeting.
	if _, err := c.reader.ReadString('\n'); err != nil {
		conn.Close()
		return nil, fmt.Errorf("greeting: %w", err)
	}
	return c, nil
}

func (c *imapClient) cmd(format string, args ...any) (string, error) {
	c.seq++
	tag := fmt.Sprintf("A%04d", c.seq)
	line := tag + " " + fmt.Sprintf(format, args...) + "\r\n"
	c.conn.SetDeadline(time.Now().Add(30 * time.Second)) //nolint:errcheck
	if _, err := c.conn.Write([]byte(line)); err != nil {
		return "", err
	}
	var resp strings.Builder
	for {
		l, err := c.reader.ReadString('\n')
		if err != nil {
			return resp.String(), err
		}
		resp.WriteString(l)
		if strings.HasPrefix(l, tag+" ") {
			if strings.Contains(l, " OK") {
				return resp.String(), nil
			}
			return resp.String(), fmt.Errorf("IMAP error: %s", strings.TrimSpace(l))
		}
	}
}

func (c *imapClient) login(user, pass string) error {
	_, err := c.cmd(`LOGIN "%s" "%s"`,
		strings.ReplaceAll(user, `"`, `\"`),
		strings.ReplaceAll(pass, `"`, `\"`))
	return err
}

func (c *imapClient) selectFolder(folder string) (int, error) {
	resp, err := c.cmd(`SELECT "%s"`, folder)
	if err != nil {
		return 0, err
	}
	// Parse "* N EXISTS"
	for _, line := range strings.Split(resp, "\n") {
		if strings.Contains(line, "EXISTS") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				n, _ := strconv.Atoi(parts[1])
				return n, nil
			}
		}
	}
	return 0, nil
}

func (c *imapClient) search(criteria string) ([]int, error) {
	resp, err := c.cmd("SEARCH %s", criteria)
	if err != nil {
		return nil, err
	}
	var ids []int
	for _, line := range strings.Split(resp, "\n") {
		if !strings.HasPrefix(line, "* SEARCH") {
			continue
		}
		parts := strings.Fields(strings.TrimPrefix(line, "* SEARCH"))
		for _, p := range parts {
			if id, e := strconv.Atoi(strings.TrimSpace(p)); e == nil {
				ids = append(ids, id)
			}
		}
	}
	return ids, nil
}

func (c *imapClient) fetchEnvelopes(ids []int) ([]imapMessage, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	set := intSliceToSet(ids)
	resp, err := c.cmd("FETCH %s (FLAGS ENVELOPE)", set)
	if err != nil {
		return nil, err
	}
	return parseEnvelopes(resp, ids), nil
}

func (c *imapClient) logout() {
	c.cmd("LOGOUT") //nolint:errcheck
	c.conn.Close()
}

func intSliceToSet(ids []int) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.Itoa(id)
	}
	return strings.Join(parts, ",")
}

// parseEnvelopes does a best-effort parse of FETCH ENVELOPE responses.
// IMAP ENVELOPE format: (date subject from sender reply-to to cc bcc in-reply-to message-id)
func parseEnvelopes(resp string, ids []int) []imapMessage {
	var msgs []imapMessage
	lines := strings.Split(resp, "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "* ") || !strings.Contains(line, "FETCH") {
			continue
		}
		// Extract message sequence number
		parts := strings.Fields(line)
		seqNum := 0
		if len(parts) >= 2 {
			seqNum, _ = strconv.Atoi(parts[1])
		}

		// Gather multi-line fetch response
		var fetch strings.Builder
		fetch.WriteString(line)
		for j := i + 1; j < len(lines); j++ {
			fetch.WriteString("\n")
			fetch.WriteString(lines[j])
			if strings.Contains(lines[j], ")") {
				break
			}
		}
		full := fetch.String()

		msg := imapMessage{id: seqNum}
		msg.seen = strings.Contains(full, `\Seen`)

		// Extract ENVELOPE tuple — look for the inner parenthesised list
		envStart := strings.Index(full, "ENVELOPE (")
		if envStart == -1 {
			msgs = append(msgs, msg)
			continue
		}
		inner := full[envStart+len("ENVELOPE ("):]
		fields := splitIMAPList(inner)
		if len(fields) >= 1 {
			msg.date = unquote(fields[0])
		}
		if len(fields) >= 2 {
			msg.subject = unquote(fields[1])
		}
		if len(fields) >= 3 {
			// FROM is a list of address structures: ((name NIL mailbox host))
			msg.from = extractAddress(fields[2])
		}
		msgs = append(msgs, msg)
	}
	// If we couldn't parse, return stubs.
	if len(msgs) == 0 {
		for _, id := range ids {
			msgs = append(msgs, imapMessage{id: id, subject: "(parse error)", from: "?"})
		}
	}
	return msgs
}

// splitIMAPList splits an IMAP parenthesised list into top-level tokens, respecting nesting.
func splitIMAPList(s string) []string {
	var fields []string
	depth := 0
	start := 0
	inQ := false
	for i, c := range s {
		switch {
		case c == '"' && (i == 0 || s[i-1] != '\\'):
			inQ = !inQ
		case !inQ && c == '(':
			depth++
		case !inQ && c == ')':
			if depth == 0 {
				fields = append(fields, strings.TrimSpace(s[start:i]))
				return fields
			}
			depth--
		case !inQ && c == ' ' && depth == 0:
			fields = append(fields, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	return fields
}

func unquote(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	if s == "NIL" {
		return ""
	}
	return s
}

func extractAddress(s string) string {
	// Address list looks like: ((name NIL mailbox host))
	s = strings.TrimSpace(s)
	if s == "NIL" || s == "" {
		return ""
	}
	// Remove outer parens
	s = strings.TrimPrefix(strings.TrimSuffix(s, ")"), "(")
	s = strings.TrimPrefix(strings.TrimSuffix(strings.TrimSpace(s), ")"), "(")
	fields := splitIMAPList(s + ")")
	if len(fields) >= 4 {
		name := unquote(fields[0])
		mailbox := unquote(fields[2])
		host := unquote(fields[3])
		if name != "" {
			return fmt.Sprintf("%s <%s@%s>", name, mailbox, host)
		}
		if mailbox != "" && host != "" {
			return fmt.Sprintf("%s@%s", mailbox, host)
		}
	}
	return s
}
