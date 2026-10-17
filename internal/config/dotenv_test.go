package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadEnvFileDoesNotOverrideExistingEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.env")
	content := "\ufeff# comment\n" +
		"A2A_TEST_PLAIN=plain value # trailing comment\n" +
		"export A2A_TEST_EXPORTED=exported\n" +
		"A2A_TEST_QUOTED=\"keep # this\"\n" +
		"A2A_TEST_SINGLE='single'\n" +
		"A2A_TEST_EMPTY=\n" +
		"A2A_TEST_PRESET=from-file\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A2A_ENV_FILE", path)
	t.Setenv("A2A_TEST_PRESET", "from-process")
	for _, key := range []string{"A2A_TEST_PLAIN", "A2A_TEST_EXPORTED", "A2A_TEST_QUOTED", "A2A_TEST_SINGLE", "A2A_TEST_EMPTY"} {
		key := key
		t.Cleanup(func() { _ = os.Unsetenv(key) })
	}

	loaded, err := LoadEnvFile()
	if err != nil {
		t.Fatalf("LoadEnvFile returned error: %v", err)
	}
	if loaded != path {
		t.Fatalf("loaded path = %q, want %q", loaded, path)
	}
	want := map[string]string{
		"A2A_TEST_PLAIN":    "plain value",
		"A2A_TEST_EXPORTED": "exported",
		"A2A_TEST_QUOTED":   "keep # this",
		"A2A_TEST_SINGLE":   "single",
		"A2A_TEST_EMPTY":    "",
		"A2A_TEST_PRESET":   "from-process",
	}
	for key, value := range want {
		got, ok := os.LookupEnv(key)
		if !ok || got != value {
			t.Errorf("%s = %q (set=%v), want %q", key, got, ok, value)
		}
	}
}

func TestLoadEnvFileMissingDefaultIsIgnoredButExplicitIsError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("A2A_ENV_FILE", "")
	if loaded, err := LoadEnvFile(); err != nil || loaded != "" {
		t.Fatalf("missing default .env: loaded=%q err=%v, want no-op", loaded, err)
	}
	t.Setenv("A2A_ENV_FILE", filepath.Join(t.TempDir(), "missing.env"))
	if _, err := LoadEnvFile(); err == nil {
		t.Fatal("missing explicit A2A_ENV_FILE should be an error")
	}
}

func TestLoadEnvFileRejectsMalformedLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.env")
	if err := os.WriteFile(path, []byte("NOT A PAIR\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("A2A_ENV_FILE", path)
	if _, err := LoadEnvFile(); err == nil {
		t.Fatal("malformed line should be rejected")
	}
}
