package build

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func project(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDetectWithProcesses(t *testing.T) {
	const pkg = `{"name":"x","scripts":{"start":"node server.js"}}`
	tests := []struct {
		name    string
		files   map[string]string
		opts    Options
		noWeb   bool
		cmd     string   // expected in the generated Dockerfile
		webExec []string // expected Set.WebExec
		exec    []string // expected Exec of the first worker or cron
		log     string   // expected in the build log
	}{
		{
			name: "node web from paas.yaml, workers through sh",
			files: map[string]string{"package.json": pkg, "paas.yaml": `
processes:
  web: {command: "node web.js"}
  worker: {command: "node worker.js", replicas: 2}
crons:
  - {name: cleanup, schedule: "*/15 * * * *", command: "node cleanup.js"}
`},
			cmd:  `CMD ["sh", "-c", "node web.js"]`,
			exec: []string{"sh", "-c", "node worker.js"},
			log:  "==> processes (paas.yaml): web, worker ×2; cron cleanup (*/15 * * * *)",
		},
		{
			name: "paas.yaml replaces the Procfile web line",
			files: map[string]string{"package.json": pkg, "Procfile": "web: node old.js\n",
				"paas.yaml": "processes:\n  worker: node worker.js\n"},
			cmd: `CMD ["npm", "start"]`,
			log: "Procfile ignored",
		},
		{
			name:  "procfile workers",
			files: map[string]string{"package.json": pkg, "Procfile": "web: node web.js\nworker: node worker.js\n"},
			cmd:   `CMD ["sh", "-c", "node web.js"]`,
			exec:  []string{"sh", "-c", "node worker.js"},
			log:   "==> processes (Procfile): web, worker",
		},
		{
			name:  "start command setting wins",
			files: map[string]string{"package.json": pkg, "paas.yaml": "processes:\n  web: node web.js\n"},
			opts:  Options{StartCommand: "node setting.js"},
			cmd:   `CMD ["sh", "-c", "node setting.js"]`,
			log:   "replaces processes.web",
		},
		{
			name:  "explicit web none",
			files: map[string]string{"requirements.txt": "discord.py\n", "bot.py": "", "paas.yaml": "processes:\n  web: none\n  bot: python bot.py\n"},
			noWeb: true,
			cmd:   `CMD ["sh", "-c", "python bot.py"]`,
			exec:  []string{"sh", "-c", "python bot.py"},
			log:   "no web, bot",
		},
		{
			name:  "no detectable web start command",
			files: map[string]string{"requirements.txt": "discord.py\n", "bot.py": "", "Procfile": "bot: python bot.py\n"},
			noWeb: true,
			exec:  []string{"sh", "-c", "python bot.py"},
			log:   "no web start command detected",
		},
		{
			name:    "dockerfile keeps CMD, web command overrides it",
			files:   map[string]string{"Dockerfile": "FROM alpine\n", "paas.yaml": "processes:\n  web: ./serve --port $PORT\n  q: ./consume\n"},
			webExec: []string{"sh", "-c", "./serve --port $PORT"},
			exec:    []string{"sh", "-c", "./consume"},
		},
		{
			name:    "list command runs without a shell",
			files:   map[string]string{"Dockerfile": "FROM scratch\n", "paas.yaml": "processes:\n  web: {command: [/srv, web]}\n  q: {command: [/srv, queue]}\n"},
			webExec: []string{"/srv", "web"},
			exec:    []string{"/srv", "queue"},
		},
		{
			name:  "go images have no shell",
			files: map[string]string{"go.mod": "module x\n\ngo 1.24\n", "main.go": "package main\nfunc main() {}\n", "paas.yaml": "crons:\n  - {name: tick, schedule: '@hourly', command: /app tick}\n"},
			cmd:   `ENTRYPOINT ["/app"]`,
			exec:  []string{"/app", "tick"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var log strings.Builder
			plan, set, err := detectWithProcesses(project(t, tt.files), tt.opts, func(f string, a ...any) {
				log.WriteString(strings.TrimSpace(fmt.Sprintf(f, a...)) + "\n")
			})
			if err != nil {
				t.Fatal(err)
			}
			if set.NoWeb != tt.noWeb {
				t.Errorf("NoWeb = %v, want %v", set.NoWeb, tt.noWeb)
			}
			if tt.cmd != "" && !strings.Contains(plan.Dockerfile, tt.cmd) {
				t.Errorf("Dockerfile lacks %q:\n%s", tt.cmd, plan.Dockerfile)
			}
			if !reflect.DeepEqual(set.WebExec, tt.webExec) {
				t.Errorf("WebExec = %q, want %q", set.WebExec, tt.webExec)
			}
			if tt.exec != nil {
				var got []string
				if len(set.Workers) > 0 {
					got = set.Workers[0].Exec
				} else if len(set.Crons) > 0 {
					got = set.Crons[0].Exec
				}
				if !reflect.DeepEqual(got, tt.exec) {
					t.Errorf("exec = %q, want %q", got, tt.exec)
				}
			}
			if !strings.Contains(log.String(), tt.log) {
				t.Errorf("log lacks %q:\n%s", tt.log, log.String())
			}
		})
	}
}

func TestDetectWithProcessesErrors(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"invalid paas.yaml": {"package.json": `{"scripts":{"start":"node s.js"}}`, "paas.yaml": "processes:\n  Bad_Name: x\n"},
		// A web process was declared, so a missing start command stays an error.
		"declared web without start": {"requirements.txt": "x\n", "Procfile": "web: \nbot: python bot.py\n"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := detectWithProcesses(project(t, files), Options{}, func(string, ...any) {})
			if err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}
