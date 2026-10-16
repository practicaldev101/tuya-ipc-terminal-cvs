package session

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Credentials used for non-interactive re-login. They are only ever read from
// the environment or from an operator-provided file outside the repository;
// they are never written to disk or logged by this package.
type Credentials struct {
	Region   string // optional; when set only sessions of this region match
	Email    string
	Password string
}

// Credential sources (environment variable names; the credentials file uses
// the same KEY=VALUE names).
const (
	EnvEmail           = "TUYA_EMAIL"
	EnvPassword        = "TUYA_PASSWORD"
	EnvRegion          = "TUYA_REGION"
	EnvCredentialsFile = "TUYA_CREDENTIALS_FILE"
)

// ErrIncompleteCredentials is returned (wrapped) by LoadCredentials when only
// one of TUYA_EMAIL / TUYA_PASSWORD is set. Callers should treat it as "no
// automatic re-login", not as a fatal configuration error.
var ErrIncompleteCredentials = errors.New("incomplete credentials")

// LoadCredentials returns the configured re-login credentials, or nil when
// none are configured. Values come from the environment (TUYA_EMAIL,
// TUYA_PASSWORD, TUYA_REGION); when path is non-empty the file is read as
// KEY=VALUE lines (systemd EnvironmentFile / docker env-file style, '#'
// comments, optional surrounding quotes) and its values override the
// environment.
func LoadCredentials(path string, getenv func(string) string) (*Credentials, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	values := map[string]string{
		EnvEmail:    getenv(EnvEmail),
		EnvPassword: getenv(EnvPassword),
		EnvRegion:   getenv(EnvRegion),
	}

	if path != "" {
		fileValues, err := readKeyValueFile(path)
		if err != nil {
			return nil, err
		}
		for key, value := range fileValues {
			if _, known := values[key]; known {
				values[key] = value
			}
		}
	}

	creds := &Credentials{
		Region:   strings.TrimSpace(values[EnvRegion]),
		Email:    strings.TrimSpace(values[EnvEmail]),
		Password: values[EnvPassword],
	}

	if creds.Email == "" && creds.Password == "" {
		return nil, nil
	}
	if creds.Email == "" || creds.Password == "" {
		return nil, fmt.Errorf("%w: both %s and %s are required", ErrIncompleteCredentials, EnvEmail, EnvPassword)
	}

	return creds, nil
}

// Matches reports whether these credentials belong to the stored session.
func (c *Credentials) Matches(region, email string) bool {
	if c == nil {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(c.Email), strings.TrimSpace(email)) {
		return false
	}
	return c.Region == "" || strings.EqualFold(c.Region, region)
}

func readKeyValueFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("cannot read credentials file: %w", err)
	}
	defer file.Close()

	values := make(map[string]string)
	scanner := bufio.NewScanner(file)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("credentials file line %d: expected KEY=VALUE", lineNo)
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("cannot read credentials file: %w", err)
	}
	return values, nil
}
