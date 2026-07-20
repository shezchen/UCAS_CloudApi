package mail

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"mime"
	"mime/quotedprintable"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func validConfig() Config {
	return Config{
		Host:       "smtp.example.com",
		Port:       587,
		Username:   "campus@example.com",
		Password:   "smtp-secret",
		From:       "campus@example.com",
		SenderName: "AxonHub 校内共享",
		TLSMode:    TLSModeSTARTTLS,
		Timeout:    10 * time.Second,
	}
}

func TestConfigValidate(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		mutate    func(*Config)
		wantError string
	}{
		{name: "valid starttls"},
		{name: "valid implicit tls", mutate: func(c *Config) { c.TLSMode = TLSModeTLS }},
		{name: "valid unauthenticated relay", mutate: func(c *Config) { c.Username, c.Password = "", "" }},
		{name: "missing host", mutate: func(c *Config) { c.Host = "" }, wantError: "smtp host"},
		{name: "host contains port", mutate: func(c *Config) { c.Host = "smtp.example.com:587" }, wantError: "must not include a port"},
		{name: "invalid port", mutate: func(c *Config) { c.Port = 0 }, wantError: "smtp port"},
		{name: "missing from", mutate: func(c *Config) { c.From = "" }, wantError: "smtp from address"},
		{name: "from display name", mutate: func(c *Config) { c.From = "Campus <campus@example.com>" }, wantError: "without a display name"},
		{name: "sender header injection", mutate: func(c *Config) { c.SenderName = "Campus\r\nBcc: attacker@example.com" }, wantError: "control characters"},
		{name: "username without password", mutate: func(c *Config) { c.Password = "" }, wantError: "both be set"},
		{name: "password without username", mutate: func(c *Config) { c.Username = "" }, wantError: "both be set"},
		{name: "plaintext forbidden", mutate: func(c *Config) { c.TLSMode = "none" }, wantError: "tls mode"},
		{name: "missing timeout", mutate: func(c *Config) { c.Timeout = 0 }, wantError: "smtp timeout"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			config := validConfig()
			if tt.mutate != nil {
				tt.mutate(&config)
			}

			err := config.Validate()
			if tt.wantError == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.wantError)
			if config.Password != "" {
				require.NotContains(t, err.Error(), config.Password)
			}
		})
	}
}

func TestNewSenderFailsClosed(t *testing.T) {
	t.Parallel()

	config := validConfig()
	config.Host = ""

	sender, err := NewVerificationSender(config)
	require.Error(t, err)
	require.Nil(t, sender)
}

func TestConfigSerializationRedactsPassword(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(validConfig())
	require.NoError(t, err)
	require.NotContains(t, string(raw), "smtp-secret")
	require.NotContains(t, string(raw), "password")
}

func TestBuildVerificationMessageUTF8(t *testing.T) {
	t.Parallel()

	sentAt := time.Date(2026, time.July, 20, 12, 0, 0, 0, time.FixedZone("CST", 8*60*60))
	raw, err := buildVerificationMessage(validConfig(), "student@mails.ucas.ac.cn", "425170", 10*time.Minute, sentAt)
	require.NoError(t, err)

	headers, body := splitMessage(t, raw)
	require.Equal(t, "student@mails.ucas.ac.cn", headers["to"])
	require.Equal(t, "Mon, 20 Jul 2026 12:00:00 +0800", headers["date"])
	require.Equal(t, `text/plain; charset="UTF-8"`, headers["content-type"])
	require.Equal(t, "quoted-printable", headers["content-transfer-encoding"])

	subject, err := (&mime.WordDecoder{}).DecodeHeader(headers["subject"])
	require.NoError(t, err)
	require.Equal(t, verificationSubject, subject)

	decodedBody, err := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(body)))
	require.NoError(t, err)
	require.Contains(t, string(decodedBody), "425170")
	require.Contains(t, string(decodedBody), "10 分钟")
	require.Contains(t, string(decodedBody), "仅可使用一次")
	require.NotContains(t, string(raw), "\nBcc:")
}

func TestVerificationInputRejectsHeaderAndBodyInjection(t *testing.T) {
	t.Parallel()

	_, err := validateMailbox("recipient address", "student@mails.ucas.ac.cn\r\nBcc: attacker@example.com")
	require.ErrorContains(t, err, "control characters")

	for _, code := range []string{
		"425170\r\nBcc: attacker@example.com",
		"425170\nmalicious body",
		"425 170",
	} {
		require.Error(t, validateVerificationCode(code))
	}
}

func TestSMTPRequiresTLS12OrNewer(t *testing.T) {
	t.Parallel()

	sender := &smtpSender{config: validConfig()}
	require.Equal(t, uint16(tls.VersionTLS12), sender.tlsConfig().MinVersion)
	require.Equal(t, "smtp.example.com", sender.tlsConfig().ServerName)
}

func TestSendVerificationCodeHonorsCancelledContextBeforeDial(t *testing.T) {
	t.Parallel()

	sender, err := NewVerificationSender(validConfig())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err = sender.SendVerificationCode(ctx, "student@mails.ucas.ac.cn", "425170", 10*time.Minute)
	require.ErrorIs(t, err, context.Canceled)
}

func splitMessage(t *testing.T, raw []byte) (map[string]string, []byte) {
	t.Helper()

	parts := bytes.SplitN(raw, []byte("\r\n\r\n"), 2)
	require.Len(t, parts, 2)

	headers := make(map[string]string)
	scanner := bufio.NewScanner(bytes.NewReader(parts[0]))
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		require.True(t, ok)
		headers[strings.ToLower(name)] = strings.TrimSpace(value)
	}
	require.NoError(t, scanner.Err())

	return headers, parts[1]
}
