package mail

import (
	"errors"
	"fmt"
	"net"
	stdmail "net/mail"
	"strings"
	"time"
	"unicode"
)

// TLSMode controls how the SMTP connection is encrypted.
type TLSMode string

const (
	// TLSModeSTARTTLS connects in plaintext only long enough to negotiate the
	// mandatory STARTTLS upgrade before authentication or message delivery.
	TLSModeSTARTTLS TLSMode = "starttls"
	// TLSModeTLS establishes TLS immediately, as commonly used on port 465.
	TLSModeTLS TLSMode = "tls"
)

// Config contains the SMTP transport settings used by VerificationSender.
//
// Password is deliberately excluded from JSON and YAML serialization so that
// config previews and structured logging cannot expose it. Configuration
// loaders may still populate it through the conf tag (for example from an
// environment variable).
type Config struct {
	Host       string        `conf:"host"        yaml:"host"        json:"host"`
	Port       int           `conf:"port"        yaml:"port"        json:"port"`
	Username   string        `conf:"username"    yaml:"username"    json:"username"`
	Password   string        `conf:"password"    yaml:"-"           json:"-"`
	From       string        `conf:"from"        yaml:"from"        json:"from"`
	SenderName string        `conf:"sender_name" yaml:"sender_name" json:"sender_name"`
	TLSMode    TLSMode       `conf:"tls_mode"    yaml:"tls_mode"    json:"tls_mode"`
	Timeout    time.Duration `conf:"timeout"     yaml:"timeout"     json:"timeout"`
}

// Validate rejects incomplete or unsafe SMTP settings. It never includes the
// password in returned errors.
func (c Config) Validate() error {
	if c.Host == "" || strings.TrimSpace(c.Host) != c.Host {
		return errors.New("smtp host is required and must not contain surrounding whitespace")
	}
	if err := rejectControlCharacters("smtp host", c.Host); err != nil {
		return err
	}
	if strings.Contains(c.Host, ":") && net.ParseIP(c.Host) == nil {
		return errors.New("smtp host must not include a port")
	}
	if c.Port < 1 || c.Port > 65535 {
		return errors.New("smtp port must be between 1 and 65535")
	}
	if _, err := validateMailbox("smtp from address", c.From); err != nil {
		return err
	}
	if err := rejectControlCharacters("smtp sender name", c.SenderName); err != nil {
		return err
	}
	if err := rejectControlCharacters("smtp username", c.Username); err != nil {
		return err
	}
	if (c.Username == "") != (c.Password == "") {
		return errors.New("smtp username and password must either both be set or both be empty")
	}
	if c.TLSMode != TLSModeSTARTTLS && c.TLSMode != TLSModeTLS {
		return fmt.Errorf("smtp tls mode must be %q or %q", TLSModeSTARTTLS, TLSModeTLS)
	}
	if c.Timeout <= 0 {
		return errors.New("smtp timeout must be greater than zero")
	}

	return nil
}

func validateMailbox(field, value string) (string, error) {
	if value == "" || strings.TrimSpace(value) != value {
		return "", fmt.Errorf("%s is required and must not contain surrounding whitespace", field)
	}
	if err := rejectControlCharacters(field, value); err != nil {
		return "", err
	}
	for _, r := range value {
		if r > unicode.MaxASCII {
			return "", fmt.Errorf("%s must be an ASCII email address", field)
		}
	}

	address, err := stdmail.ParseAddress(value)
	if err != nil || address.Name != "" || address.Address != value {
		return "", fmt.Errorf("%s must be a single email address without a display name", field)
	}
	local, domain, ok := strings.Cut(address.Address, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return "", fmt.Errorf("%s must be a complete email address", field)
	}

	return address.Address, nil
}

func rejectControlCharacters(field, value string) error {
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s must not contain control characters", field)
		}
	}

	return nil
}
