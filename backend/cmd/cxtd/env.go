package main

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/wnsdy95/cxthub/backend/internal/adapters/resend"
	"github.com/wnsdy95/cxthub/backend/internal/app"
)

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// loadServerEnv reads only the requested file (no parent-directory discovery).
// Values are literal: never execute shell syntax or expand other environment
// variables. Process environment wins, including explicitly empty values.
func loadServerEnv() error {
	path := os.Getenv("CXT_ENV_FILE")
	explicit := path != ""
	if !explicit {
		path = ".env"
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) && !explicit {
		return nil
	}
	if err != nil {
		return fmt.Errorf("cannot read server environment file")
	}
	defer f.Close()
	values := map[string]string{}
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 4096), 65536)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		text = strings.TrimPrefix(text, "export ")
		key, value, ok := strings.Cut(text, "=")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		invalid := func() error { return fmt.Errorf("invalid server environment entry at line %d", line) }
		if !ok || !envName.MatchString(key) {
			return invalid()
		}
		if strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			quote := value[0]
			end := -1
			for n := 1; n < len(value); n++ {
				if quote == '"' && value[n] == '\\' {
					n++
					continue
				}
				if value[n] == quote {
					end = n
					break
				}
			}
			if end < 0 {
				return invalid()
			}
			tail := strings.TrimSpace(value[end+1:])
			if tail != "" && !strings.HasPrefix(tail, "#") {
				return invalid()
			}
			if quote == '"' {
				decoded, e := strconv.Unquote(value[:end+1])
				if e != nil {
					return invalid()
				}
				value = decoded
			} else {
				value = value[1:end]
			}
		} else {
			if n := strings.Index(value, " #"); n >= 0 {
				value = strings.TrimSpace(value[:n])
			}
		}
		if strings.ContainsRune(value, 0) {
			return invalid()
		}
		values[key] = value
	}
	if scanner.Err() != nil {
		return fmt.Errorf("cannot parse server environment file")
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); !exists {
			if os.Setenv(key, value) != nil {
				return fmt.Errorf("cannot apply server environment")
			}
		}
	}
	return nil
}
func configureInvitationEmail(s *app.IdentityService, addr, publicURL string) error {
	key := strings.TrimSpace(os.Getenv("RESEND_API_KEY"))
	if key == "" {
		return nil
	}
	from := strings.TrimSpace(os.Getenv("RESEND_FROM"))
	if from == "" {
		from = "CXTHub <noreply@cxthub.com>"
	}
	origin := strings.TrimSpace(os.Getenv("CXT_WEB_URL"))
	if origin == "" {
		origin = publicURL
		if isLoopback(addr) {
			origin = "http://localhost:5173"
		}
	}
	return s.ConfigureInvitationEmail(resend.New(key), from, origin)
}
