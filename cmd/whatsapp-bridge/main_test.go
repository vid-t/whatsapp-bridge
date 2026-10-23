package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnvFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "acme.env")
	content := "# settings\nBRIDGE_TEST_PLAIN=Acme Team\n; comment\n\nexport BRIDGE_TEST_EXPORTED=1\n" +
		`BRIDGE_TEST_DQ="pa\"ss\\word \$HOME \` + "`" + `x\n"` + "\nBRIDGE_TEST_SQ='a \"b\" \\'\nBRIDGE_TEST_FROM=whatsapp-bridge <bridge@example.com>\r\nBRIDGE_TEST_EMPTY=\n" +
		`BRIDGE_TEST_BARE=a\b\\c` + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BRIDGE_TEST_PLAIN", "from the environment")
	if err := loadEnvFile(path); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]string{
		"BRIDGE_TEST_PLAIN": "Acme Team", "BRIDGE_TEST_EXPORTED": "1", "BRIDGE_TEST_DQ": `pa"ss\word $HOME ` + "`" + `x\n`,
		"BRIDGE_TEST_SQ": "a \"b\" \\", "BRIDGE_TEST_FROM": "whatsapp-bridge <bridge@example.com>", "BRIDGE_TEST_EMPTY": "",
		"BRIDGE_TEST_BARE": `ab\c`,
	} {
		if got := os.Getenv(k); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
		_ = os.Unsetenv(k)
	}
	if err := os.WriteFile(path, []byte("not an assignment\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := loadEnvFile(path); err == nil {
		t.Fatal("malformed line accepted")
	}
	for args, want := range map[string][]string{
		"/a.env": {"-env-file", "/a.env", "list"}, "/b.env": {"--env-file=/b.env", "invite", "x@y.ch"}, "": {"list", "--", "-env-file", "/c.env"},
	} {
		if got := envFileArg(want); got != args {
			t.Errorf("envFileArg(%v) = %q, want %q", want, got, args)
		}
	}
}
