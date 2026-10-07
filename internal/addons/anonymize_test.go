package addons

import (
	"strings"
	"testing"
)

func TestParseRule(t *testing.T) {
	for _, c := range []struct {
		in   string
		want Rule
		str  string
	}{
		{"users.email: email", Rule{"public", "users", "email", "email"}, "users.email: email"},
		{"  users.name :NAME ", Rule{"public", "users", "name", "name"}, "users.name: name"},
		{"billing.cards.number: hash", Rule{"billing", "cards", "number", "hash"}, "billing.cards.number: hash"},
		{"public.users.phone: null", Rule{"public", "users", "phone", "null"}, "users.phone: null"},
		{"*.phone: null", Rule{"", "*", "phone", "null"}, "*.phone: null"},
		{"orders.note: redact", Rule{"public", "orders", "note", "redact"}, "orders.note: redact"},
		{"t_1.c$2: empty", Rule{"public", "t_1", "c$2", "empty"}, "t_1.c$2: empty"},
	} {
		got, err := ParseRule(c.in)
		if err != nil || got != c.want || got.String() != c.str {
			t.Errorf("ParseRule(%q) = %+v, %v; want %+v (%q)", c.in, got, err, c.want, c.str)
		}
	}
	for _, in := range []string{
		"users.email",                          // no strategy
		"users.email: shuffle",                 // unknown strategy
		"email: email",                         // no table
		"a.b.c.d: null",                        // too many parts
		`users.email"; DROP TABLE x; --: null`, // SQL in an identifier
		"Users.Email: null",                    // mixed case
		"users.e mail: null",
		"*.*: null",
		"x.*.c: null",
		"pg_catalog.pg_authid.rolpassword: null",
		"information_schema.tables.x: null",
		"users.: null",
		": null",
		"users.email: email; DROP TABLE users",
		"1users.email: null",
	} {
		if r, err := ParseRule(in); err == nil {
			t.Errorf("ParseRule(%q) = %+v, want an error", in, r)
		}
	}
}

func TestParseRules(t *testing.T) {
	rules, err := NormalizeRules([]string{"", "# comment", "users.email: email", "public.users.name: name", " *.phone: null "})
	if err != nil || strings.Join(rules, "|") != "users.email: email|users.name: name|*.phone: null" {
		t.Fatalf("NormalizeRules = %q, %v", rules, err)
	}
	if _, err := ParseRules([]string{"users.email: email", "public.users.email: hash"}); err == nil {
		t.Error("a column with two rules must be refused")
	}
	many := make([]string, MaxRules+1)
	for i := range many {
		many[i] = "t.c" + strings.Repeat("x", i) + ": null"
	}
	if _, err := ParseRules(many); err == nil {
		t.Error("too many rules must be refused")
	}
}

func TestCompileRules(t *testing.T) {
	if CompileRules(nil) != "" {
		t.Fatal("no rules compile to nothing")
	}
	rules, err := ParseRules([]string{"users.email: email", "orders.note: redact", "users.name: name", "*.phone: phone"})
	if err != nil {
		t.Fatal(err)
	}
	sql := CompileRules(rules)
	for _, want := range []string{
		"SET session_replication_role = replica;\n",
		// One UPDATE per table, in order of first appearance; NULL stays NULL.
		`UPDATE "public"."users" SET "email" = CASE WHEN "email" IS NULL THEN NULL ELSE 'user_' || left(md5("email"::text), 12) || '@example.invalid' END, "name" = CASE WHEN "name" IS NULL THEN NULL ELSE 'User ' || upper(left(md5("name"::text), 6)) END;`,
		`UPDATE "public"."orders" SET "note" = CASE WHEN "note" IS NULL THEN NULL ELSE '[redacted]' END;`,
		// The wildcard loops over base tables; quotes doubled and % escaped
		// inside format().
		"WHERE c.column_name = 'phone' AND t.table_type = 'BASE TABLE'",
		`EXECUTE format('UPDATE %I.%I SET "phone" = CASE WHEN "phone" IS NULL THEN NULL ELSE ''+1555'' || lpad((abs((''x'' || left(md5("phone"::text), 8))::bit(32)::int) %% 10000000)::text, 7, ''0'') END', r.table_schema, r.table_name);`,
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("compiled SQL lacks\n%s\n---\n%s", want, sql)
		}
	}
	if strings.Index(sql, `"users"`) > strings.Index(sql, `"orders"`) {
		t.Error("tables must keep the order of their first rule")
	}
	null, _ := ParseRules([]string{"users.phone: null"})
	if got := CompileRules(null); !strings.Contains(got, `UPDATE "public"."users" SET "phone" = NULL;`) {
		t.Errorf("null rule: %s", got)
	}
}

func TestCheckSQL(t *testing.T) {
	for _, ok := range []string{"", "UPDATE orders SET note = NULL;", "UPDATE t SET s = E'a\\nb';"} {
		if err := CheckSQL(ok, 1000); err != nil {
			t.Errorf("CheckSQL(%q) = %v", ok, err)
		}
	}
	for _, bad := range []string{`\! sh`, "SELECT 1;\n  \\copy t to 'x'", strings.Repeat("x", 1001)} {
		if err := CheckSQL(bad, 1000); err == nil {
			t.Errorf("CheckSQL(%q) accepted", bad)
		}
	}
}

func TestWarningsAndSizes(t *testing.T) {
	w := FallbackWarning("size", 6<<30, 5<<30)
	if w != "production is 6.0 GiB, above the copy limit of 5.0 GiB: an empty database was created instead" {
		t.Errorf("size warning = %q", w)
	}
	if got := WarningTR(w); got != "production 6.0 GiB, kopya sınırı 5.0 GiB: kopya yerine boş bir veritabanı oluşturuldu" {
		t.Errorf("WarningTR = %q", got)
	}
	if got := WarningTR(FallbackWarning("disk", 300<<20, 0)); !strings.Contains(got, "300.0 MiB") || !strings.Contains(got, "%80") {
		t.Errorf("disk warning = %q", got)
	}
	if FallbackWarning("", 1, 1) != "" || WarningTR("other") != "other" {
		t.Error("no warning")
	}
	for n, want := range map[int64]string{0: "0 B", 1023: "1023 B", 1536: "1.5 KiB", 12 << 20: "12.0 MiB"} {
		if got := HumanBytes(n); got != want {
			t.Errorf("HumanBytes(%d) = %q, want %q", n, got, want)
		}
	}
}

func TestLastLines(t *testing.T) {
	msg := "==> pg_dump app | pg_restore x\npg_restore: error: could not execute query\nERROR:  relation \"users\" does not exist\n\n"
	if got := lastLines(msg); got != `pg_restore: error: could not execute query ⏎ ERROR:  relation "users" does not exist` {
		t.Errorf("lastLines = %q", got)
	}
}
