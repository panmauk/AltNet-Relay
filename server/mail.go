package server

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// SMTPMailer sends plain-text email through the same SMTP relay the accounts
// backend uses (configured with the same ALTNET_SMTP_* settings). Port 465 is
// implicit TLS; anything else is STARTTLS, which net/smtp does by itself.
type SMTPMailer struct {
	Host string
	Port int
	User string
	Pass string
	From string // header form, e.g. `AltNet <code.verification@panmox.org>`
}

func (m *SMTPMailer) Send(to, subject, body string) error {
	if m.Host == "" || m.Port == 0 || m.From == "" {
		return fmt.Errorf("smtp isn't configured")
	}
	if strings.ContainsAny(to, "\r\n") || strings.ContainsAny(subject, "\r\n") {
		return fmt.Errorf("refusing a header with a line break")
	}
	from := envelopeAddress(m.From)
	msg := mailMessage(m.From, to, subject, body)
	addr := net.JoinHostPort(m.Host, fmt.Sprint(m.Port))
	auth := smtp.PlainAuth("", m.User, m.Pass, m.Host)
	if m.Port != 465 {
		return smtp.SendMail(addr, auth, from, []string{to}, msg)
	}
	conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 15 * time.Second}, "tcp", addr, &tls.Config{ServerName: m.Host})
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, m.Host)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if err := c.Auth(auth); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	if err := c.Rcpt(to); err != nil {
		return err
	}
	wc, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := wc.Write(msg); err != nil {
		return err
	}
	if err := wc.Close(); err != nil {
		return err
	}
	return c.Quit()
}

func mailMessage(from, to, subject, body string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	fmt.Fprintf(&b, "Date: %s\r\n", time.Now().UTC().Format(time.RFC1123Z))
	b.WriteString("MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\n\r\n")
	body = strings.ReplaceAll(body, "\r\n", "\n")
	b.WriteString(strings.ReplaceAll(body, "\n", "\r\n"))
	return []byte(b.String())
}

func envelopeAddress(header string) string {
	if i := strings.LastIndex(header, "<"); i >= 0 {
		if j := strings.Index(header[i:], ">"); j > 0 {
			return strings.TrimSpace(header[i+1 : i+j])
		}
	}
	return strings.TrimSpace(header)
}
