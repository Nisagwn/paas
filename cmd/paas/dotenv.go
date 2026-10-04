package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
)

// loadDotEnv reads KEY=VALUE lines from path into the environment, so a plain
// `go run ./cmd/paas` picks up .env like `make run` does. Variables already set
// in the environment win; a missing file is not an error (in the cluster the
// settings come from the Deployment).
func loadDotEnv(path string) (int, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer f.Close()

	n, line := 0, 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line++
		s := strings.TrimSpace(strings.TrimPrefix(sc.Text(), string(rune(0xFEFF))))
		if s == "" || strings.HasPrefix(s, "#") {
			continue
		}
		s = strings.TrimPrefix(s, "export ")
		k, v, ok := strings.Cut(s, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			return n, fmt.Errorf("%s:%d: expected KEY=VALUE", path, line)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); set {
			continue
		}
		if err := os.Setenv(k, v); err != nil {
			return n, err
		}
		n++
	}
	return n, sc.Err()
}
