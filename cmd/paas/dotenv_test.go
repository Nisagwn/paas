package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDotEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := string(rune(0xFEFF)) + "# comment\n\nPAAS_T_A=one\nexport PAAS_T_B = \"two words\"\nPAAS_T_C='x=y'\nPAAS_T_SET=from-file\nPAAS_T_EMPTY=\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PAAS_T_SET", "from-env")
	for _, k := range []string{"PAAS_T_A", "PAAS_T_B", "PAAS_T_C", "PAAS_T_EMPTY"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}

	n, err := loadDotEnv(path)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"PAAS_T_A": "one", "PAAS_T_B": "two words", "PAAS_T_C": "x=y", "PAAS_T_SET": "from-env", "PAAS_T_EMPTY": ""}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if n != 4 {
		t.Errorf("loaded %d, want 4", n)
	}

	if n, err := loadDotEnv(filepath.Join(t.TempDir(), "missing")); n != 0 || err != nil {
		t.Errorf("missing file: %d, %v", n, err)
	}
	bad := filepath.Join(t.TempDir(), ".env")
	os.WriteFile(bad, []byte("not a pair\n"), 0o600)
	if _, err := loadDotEnv(bad); err == nil {
		t.Error("want an error for a line without =")
	}
}
