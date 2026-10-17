package config

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// DefaultEnvFile is read by LoadEnvFile when A2A_ENV_FILE is not set.
const DefaultEnvFile = ".env"

// LoadEnvFile reads KEY=VALUE pairs from the file named by A2A_ENV_FILE (or
// ./.env) into the process environment. Variables that are already set always
// win, so container/Secret Manager injection overrides the file. A missing file
// is not an error; a malformed one is.
func LoadEnvFile() (string, error) {
	path := strings.TrimSpace(os.Getenv("A2A_ENV_FILE"))
	explicit := path != ""
	if !explicit {
		path = DefaultEnvFile
	}
	values, err := parseEnvFile(path)
	if err != nil {
		if !explicit && errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", err
	}
	for key, value := range values {
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return "", fmt.Errorf("set %s from %s: %w", key, path, err)
		}
	}
	return path, nil
}

func parseEnvFile(path string) (map[string]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	values := map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNumber := 0
	for scanner.Scan() {
		lineNumber++
		line := strings.TrimSpace(scanner.Text())
		if lineNumber == 1 {
			line = strings.TrimPrefix(line, "\ufeff")
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !found || !validEnvKey(key) {
			return nil, fmt.Errorf("%s:%d: expected KEY=VALUE", path, lineNumber)
		}
		values[key] = unquoteEnvValue(strings.TrimSpace(value))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return values, nil
}

func validEnvKey(key string) bool {
	if key == "" {
		return false
	}
	for index, char := range key {
		switch {
		case char == '_', char >= 'A' && char <= 'Z', char >= 'a' && char <= 'z':
		case char >= '0' && char <= '9' && index > 0:
		default:
			return false
		}
	}
	return true
}

// unquoteEnvValue strips matching surrounding quotes. Unquoted values drop a
// trailing " # comment"; quoted values are kept verbatim.
func unquoteEnvValue(value string) string {
	if len(value) >= 2 {
		first, last := value[0], value[len(value)-1]
		if (first == '"' || first == '\'') && first == last {
			return value[1 : len(value)-1]
		}
	}
	if index := strings.Index(value, " #"); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	return value
}
