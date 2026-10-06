package process

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  Set
		web   bool // WebDeclared
		notes int
	}{
		{name: "nothing", files: map[string]string{}, want: Set{}},
		{
			name: "full yaml",
			files: map[string]string{"paas.yaml": `
processes:
  web: { command: "node server.js" }
  worker: { command: "node worker.js", replicas: 2, previews: true }
  bot: { command: "python bot.py", previews: false }
crons:
  - name: cleanup
    schedule: "*/15 * * * *"
    command: "node scripts/cleanup.js"
  - name: backup
    schedule: "@daily"
    command: ["/usr/bin/backup", "--all"]
`},
			web: true,
			want: Set{
				Source: "paas.yaml", WebCommand: "node server.js",
				Workers: []Worker{
					{Name: "bot", Command: "python bot.py", Replicas: 1},
					{Name: "worker", Command: "node worker.js", Replicas: 2, Previews: true},
				},
				Crons: []Cron{
					{Name: "backup", Schedule: "@daily", Command: "/usr/bin/backup --all", Exec: []string{"/usr/bin/backup", "--all"}},
					{Name: "cleanup", Schedule: "*/15 * * * *", Command: "node scripts/cleanup.js"},
				},
			},
		},
		{
			name:  "string shorthand and web none",
			files: map[string]string{"paas.yml": "processes:\n  web: none\n  bot: python bot.py\n"},
			web:   true,
			want:  Set{Source: "paas.yml", NoWeb: true, Workers: []Worker{{Name: "bot", Command: "python bot.py", Replicas: 1}}},
		},
		{
			name:  "web command none in an object",
			files: map[string]string{"paas.yaml": "processes:\n  web: {command: none}\n  bot: python bot.py\n"},
			web:   true,
			want:  Set{Source: "paas.yaml", NoWeb: true, Workers: []Worker{{Name: "bot", Command: "python bot.py", Replicas: 1}}},
		},
		{
			name:  "web list command",
			files: map[string]string{"paas.json": `{"processes": {"web": {"command": ["/app", "serve"]}}}`},
			web:   true,
			want:  Set{Source: "paas.json", WebCommand: "/app serve", WebExec: []string{"/app", "serve"}},
		},
		{
			name:  "web empty object is the detected command",
			files: map[string]string{"paas.yaml": "processes:\n  web: {}\n  q: {command: node q.js, replicas: 0}\n"},
			web:   true,
			want:  Set{Source: "paas.yaml", Workers: []Worker{{Name: "q", Command: "node q.js", Replicas: 0}}},
		},
		{
			name:  "crons only",
			files: map[string]string{"paas.yaml": "crons:\n  - {name: tick, schedule: '0 * * * *', command: date}\n"},
			want:  Set{Source: "paas.yaml", Crons: []Cron{{Name: "tick", Schedule: "0 * * * *", Command: "date"}}},
		},
		{
			name: "procfile",
			files: map[string]string{"Procfile": "# processes\nweb: gunicorn app:app\nworker: celery -A app worker\n" +
				"release: python manage.py migrate\n\nclock: python clock.py\n"},
			web:   true,
			notes: 1,
			want: Set{Source: "Procfile", WebCommand: "gunicorn app:app", Workers: []Worker{
				{Name: "clock", Command: "python clock.py", Replicas: 1},
				{Name: "worker", Command: "celery -A app worker", Replicas: 1},
			}},
		},
		{
			name:  "procfile web only",
			files: map[string]string{"Procfile": "web: npm start\n"},
			web:   true,
			want:  Set{Source: "Procfile", WebCommand: "npm start"},
		},
		{
			name: "yaml wins over procfile",
			files: map[string]string{
				"paas.yaml": "processes:\n  worker: node w.js\n",
				"Procfile":  "web: npm start\nother: node o.js\n",
			},
			notes: 1,
			want:  Set{Source: "paas.yaml", Workers: []Worker{{Name: "worker", Command: "node w.js", Replicas: 1}}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := Load(write(t, tt.files))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(c.Set, tt.want) {
				t.Errorf("set:\n got %+v\nwant %+v", c.Set, tt.want)
			}
			if c.WebDeclared != tt.web {
				t.Errorf("WebDeclared = %v, want %v", c.WebDeclared, tt.web)
			}
			if len(c.Notes) != tt.notes {
				t.Errorf("notes = %q, want %d", c.Notes, tt.notes)
			}
		})
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"two files", map[string]string{"paas.yaml": "crons: []", "paas.json": "{}"}, "keep only one"},
		{"empty", map[string]string{"paas.yaml": "\n"}, "empty"},
		{"bad yaml", map[string]string{"paas.yaml": "processes: [\n"}, "invalid YAML"},
		{"duplicate key", map[string]string{"paas.yaml": "processes:\n  a: x\n  a: y\n"}, "invalid YAML"},
		{"unknown top-level key", map[string]string{"paas.yaml": "process:\n  a: x\n"}, "unknown field"},
		{"unknown process key", map[string]string{"paas.yaml": "processes:\n  a: {command: x, replica: 2}\n"}, "allowed keys"},
		{"bad name", map[string]string{"paas.yaml": "processes:\n  Worker_1: node w.js\n"}, "process name"},
		{"long name", map[string]string{"paas.yaml": "processes:\n  " + strings.Repeat("a", 21) + ": x\n"}, "1-20"},
		{"missing command", map[string]string{"paas.yaml": "processes:\n  w: {replicas: 1}\n"}, "command is empty"},
		{"too many replicas", map[string]string{"paas.yaml": "processes:\n  w: {command: x, replicas: 11}\n"}, "between 0 and 10"},
		{"negative replicas", map[string]string{"paas.yaml": "processes:\n  w: {command: x, replicas: -1}\n"}, "between 0 and 10"},
		{"web replicas", map[string]string{"paas.yaml": "processes:\n  web: {command: x, replicas: 2}\n"}, "cannot be set"},
		{"web null", map[string]string{"paas.yaml": "processes:\n  web:\n"}, "web: none"},
		{"nothing to run", map[string]string{"paas.yaml": "processes:\n  web: none\n"}, "run nothing"},
		{"bad schedule", map[string]string{"paas.yaml": "crons:\n  - {name: c, schedule: '* * *', command: x}\n"}, "5 fields"},
		{"cron without name", map[string]string{"paas.yaml": "crons:\n  - {schedule: '@daily', command: x}\n"}, "name is required"},
		{"cron named web", map[string]string{"paas.yaml": "crons:\n  - {name: web, schedule: '@daily', command: x}\n"}, "reserved"},
		{"shared name", map[string]string{"paas.yaml": "processes:\n  job: x\ncrons:\n  - {name: job, schedule: '@daily', command: x}\n"}, "already used"},
		{"multi-line command", map[string]string{"paas.json": `{"processes": {"w": {"command": "a\nb"}}}`}, "single line"},
		{"command type", map[string]string{"paas.yaml": "processes:\n  w: {command: 5}\n"}, "string or a list"},
		{"empty list", map[string]string{"paas.yaml": "processes:\n  w: {command: []}\n"}, "empty"},
		{"too many workers", map[string]string{"paas.yaml": "processes:\n" + manyWorkers(11)}, "at most 10"},
		{"procfile syntax", map[string]string{"Procfile": "web npm start\n"}, "line 1"},
		{"procfile duplicate", map[string]string{"Procfile": "w: a\nw: b\n"}, "twice"},
		{"procfile bad name", map[string]string{"Procfile": "clock_process: python clock.py\n"}, "process name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Load(write(t, tt.files))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func manyWorkers(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString("  w" + string(rune('a'+i)) + ": x\n")
	}
	return b.String()
}

func TestResolve(t *testing.T) {
	s := Set{
		Workers: []Worker{{Name: "a", Command: "node a.js"}, {Name: "b", Command: "x", Exec: []string{"/bin/x", "-v"}}},
		Crons:   []Cron{{Name: "c", Command: "/app  cleanup --now"}},
	}
	shell := s
	shell.Workers = append([]Worker(nil), s.Workers...)
	shell.Crons = append([]Cron(nil), s.Crons...)
	shell.Resolve(true)
	if got := shell.Workers[0].Exec; !reflect.DeepEqual(got, []string{"sh", "-c", "node a.js"}) {
		t.Errorf("shell exec = %q", got)
	}
	if got := shell.Workers[1].Exec; !reflect.DeepEqual(got, []string{"/bin/x", "-v"}) {
		t.Errorf("list exec changed: %q", got)
	}
	s.Resolve(false)
	if got := s.Crons[0].Exec; !reflect.DeepEqual(got, []string{"/app", "cleanup", "--now"}) {
		t.Errorf("no-shell exec = %q", got)
	}
}
