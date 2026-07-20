package mail

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"mime"
	"mime/quotedprintable"
	"net"
	stdmail "net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const verificationSubject = "【AxonHub】邮箱验证码"

// VerificationSender sends one-time verification codes. Implementations must not retain or
// log the plaintext code after SendVerificationCode returns.
type VerificationSender interface {
	SendVerificationCode(ctx context.Context, to, code string, ttl time.Duration) error
}

type smtpSender struct {
	config Config
}

// NewVerificationSender validates the SMTP configuration and returns a
// fail-closed sender.
// There is intentionally no disabled or no-op implementation: incomplete
// configuration is an error and cannot silently bypass email verification.
func NewVerificationSender(config Config) (VerificationSender, error) {
	if err := config.Validate(); err != nil {
		return nil, fmt.Errorf("invalid smtp configuration: %w", err)
	}

	return &smtpSender{config: config}, nil
}

func (s *smtpSender) SendVerificationCode(ctx context.Context, to, code string, ttl time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	recipient, err := validateMailbox("recipient address", to)
	if err != nil {
		return err
	}
	if err := validateVerificationCode(code); err != nil {
		return err
	}
	if ttl <= 0 {
		return fmt.Errorf("verification code ttl must be greater than zero")
	}

	message, err := buildVerificationMessage(s.config, recipient, code, ttl, time.Now())
	if err != nil {
		return fmt.Errorf("build verification email: %w", err)
	}

	sendCtx, cancel := context.WithTimeout(ctx, s.config.Timeout)
	defer cancel()

	client, conn, stopCloseOnCancel, err := s.connect(sendCtx)
	if err != nil {
		if ctxErr := sendCtx.Err(); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	defer stopCloseOnCancel()
	defer conn.Close()
	defer client.Close()

	if s.config.Username != "" {
		auth := smtp.PlainAuth("", s.config.Username, s.config.Password, s.config.Host)
		if err := client.Auth(auth); err != nil {
			return smtpOperationError(sendCtx, "smtp authentication failed", err)
		}
	}
	if err := client.Mail(s.config.From); err != nil {
		return smtpOperationError(sendCtx, "smtp rejected sender", err)
	}
	if err := client.Rcpt(recipient); err != nil {
		return smtpOperationError(sendCtx, "smtp rejected recipient", err)
	}

	w, err := client.Data()
	if err != nil {
		return smtpOperationError(sendCtx, "smtp rejected message data", err)
	}
	if _, err := w.Write(message); err != nil {
		_ = w.Close()
		return smtpOperationError(sendCtx, "write smtp message", err)
	}
	if err := w.Close(); err != nil {
		return smtpOperationError(sendCtx, "finalize smtp message", err)
	}

	// Once DATA has been accepted, a later QUIT failure must not make callers
	// retry and send a duplicate message.
	_ = client.Quit()

	return nil
}

func (s *smtpSender) connect(ctx context.Context) (*smtp.Client, net.Conn, func() bool, error) {
	address := net.JoinHostPort(s.config.Host, strconv.Itoa(s.config.Port))
	tlsConfig := s.tlsConfig()

	var (
		conn net.Conn
		err  error
	)
	switch s.config.TLSMode {
	case TLSModeTLS:
		dialer := tls.Dialer{
			NetDialer: &net.Dialer{},
			Config:    tlsConfig,
		}
		conn, err = dialer.DialContext(ctx, "tcp", address)
	case TLSModeSTARTTLS:
		conn, err = (&net.Dialer{}).DialContext(ctx, "tcp", address)
	default:
		return nil, nil, nil, fmt.Errorf("unsupported smtp tls mode")
	}
	if err != nil {
		return nil, nil, nil, fmt.Errorf("connect to smtp server: %w", err)
	}

	deadline, ok := ctx.Deadline()
	if !ok {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("smtp context has no deadline")
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("set smtp connection deadline: %w", err)
	}

	stopCloseOnCancel := context.AfterFunc(ctx, func() {
		_ = conn.Close()
	})

	client, err := smtp.NewClient(conn, s.config.Host)
	if err != nil {
		stopCloseOnCancel()
		_ = conn.Close()
		return nil, nil, nil, fmt.Errorf("initialize smtp client: %w", err)
	}

	if s.config.TLSMode == TLSModeSTARTTLS {
		if ok, _ := client.Extension("STARTTLS"); !ok {
			stopCloseOnCancel()
			_ = client.Close()
			_ = conn.Close()
			return nil, nil, nil, fmt.Errorf("smtp server does not support required STARTTLS")
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			stopCloseOnCancel()
			_ = client.Close()
			_ = conn.Close()
			return nil, nil, nil, fmt.Errorf("upgrade smtp connection to TLS: %w", err)
		}
	}

	return client, conn, stopCloseOnCancel, nil
}

func smtpOperationError(ctx context.Context, operation string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}

	return fmt.Errorf("%s: %w", operation, err)
}

func (s *smtpSender) tlsConfig() *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		ServerName: s.config.Host,
	}
}

func validateVerificationCode(code string) error {
	if code == "" {
		return fmt.Errorf("verification code is required")
	}
	if len(code) > 64 {
		return fmt.Errorf("verification code is too long")
	}
	for _, r := range code {
		if r > unicode.MaxASCII || unicode.IsControl(r) || unicode.IsSpace(r) {
			return fmt.Errorf("verification code contains invalid characters")
		}
	}

	return nil
}

func buildVerificationMessage(config Config, to, code string, ttl time.Duration, sentAt time.Time) ([]byte, error) {
	from := (&stdmail.Address{Name: config.SenderName, Address: config.From}).String()
	subject := mime.QEncoding.Encode("UTF-8", verificationSubject)
	body := fmt.Sprintf(
		"您好：\r\n\r\n您的 AxonHub 校内共享邮箱验证码是：\r\n\r\n    %s\r\n\r\n验证码将在 %s 后失效，且仅可使用一次。请勿向任何人透露此验证码。\r\n如非本人操作，请忽略此邮件。\r\n",
		code,
		formatDurationChinese(ttl),
	)

	var encodedBody bytes.Buffer
	quotedPrintable := quotedprintable.NewWriter(&encodedBody)
	if _, err := io.WriteString(quotedPrintable, body); err != nil {
		return nil, err
	}
	if err := quotedPrintable.Close(); err != nil {
		return nil, err
	}

	var message bytes.Buffer
	writeHeader := func(name, value string) {
		fmt.Fprintf(&message, "%s: %s\r\n", name, value)
	}
	writeHeader("From", from)
	writeHeader("To", to)
	writeHeader("Subject", subject)
	writeHeader("Date", sentAt.Format(time.RFC1123Z))
	writeHeader("MIME-Version", "1.0")
	writeHeader("Content-Type", `text/plain; charset="UTF-8"`)
	writeHeader("Content-Transfer-Encoding", "quoted-printable")
	message.WriteString("\r\n")
	message.Write(encodedBody.Bytes())

	return message.Bytes(), nil
}

func formatDurationChinese(duration time.Duration) string {
	duration = duration.Truncate(time.Second)
	if duration < time.Second {
		return "不足 1 秒"
	}

	hours := duration / time.Hour
	duration %= time.Hour
	minutes := duration / time.Minute
	seconds := (duration % time.Minute) / time.Second

	parts := make([]string, 0, 3)
	if hours > 0 {
		parts = append(parts, fmt.Sprintf("%d 小时", hours))
	}
	if minutes > 0 {
		parts = append(parts, fmt.Sprintf("%d 分钟", minutes))
	}
	if seconds > 0 {
		parts = append(parts, fmt.Sprintf("%d 秒", seconds))
	}

	return strings.Join(parts, " ")
}
