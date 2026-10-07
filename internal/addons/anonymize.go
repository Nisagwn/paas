package addons

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Faz 22: anonymization of preview copies.
//
// A rule names a column and what to put in it:
//
//	users.email: email          one column ([schema.]table.column, schema "public" by default)
//	billing.cards.number: hash  a table in another schema
//	*.phone: null               the column in every table that has it
//
// Rules are identifiers only: they are validated here and compiled into
// SQL with quoted identifiers and fixed expressions, so no rule can carry
// SQL of its own. Members who need more write explicit statements
// (anonymize_sql); those run after the rules, as the branch role, inside
// the copy only.

// Strategies and the value they produce. Every strategy keeps NULL as NULL
// and derives the new value from md5 of the old one, so equal inputs stay
// equal (joins on an anonymized column still match) and unique columns
// stay unique in practice.
// {c} stands for the quoted column.
var strategies = map[string]string{
	"null":   "NULL",
	"email":  "'user_' || left(md5({c}::text), 12) || '@example.invalid'",
	"name":   "'User ' || upper(left(md5({c}::text), 6))",
	"phone":  "'+1555' || lpad((abs(('x' || left(md5({c}::text), 8))::bit(32)::int) % 10000000)::text, 7, '0')",
	"hash":   "md5({c}::text)",
	"redact": "'[redacted]'",
	"empty":  "''",
}

// Strategies lists the strategy names, for help texts.
var Strategies = []string{"email", "name", "phone", "hash", "redact", "empty", "null"}

// MaxRules bounds the rules of one add-on.
const MaxRules = 50

// identRe is an unquoted lower-case PostgreSQL identifier. Mixed-case
// (quoted) names are not supported by rules; use a statement for them.
var identRe = regexp.MustCompile(`^[a-z_][a-z0-9_$]{0,62}$`)

// Rule is one parsed column rule.
type Rule struct {
	Schema   string // "" with Table "*"
	Table    string // "*": every table with the column
	Column   string
	Strategy string
}

// Wildcard reports whether the rule applies to every table.
func (r Rule) Wildcard() bool { return r.Table == "*" }

// String is the canonical form ("public.users.email: email" is shown as
// "users.email: email").
func (r Rule) String() string {
	switch {
	case r.Wildcard():
		return "*." + r.Column + ": " + r.Strategy
	case r.Schema == "public":
		return r.Table + "." + r.Column + ": " + r.Strategy
	}
	return r.Schema + "." + r.Table + "." + r.Column + ": " + r.Strategy
}

// ParseRule reads "[schema.]table.column: strategy" or "*.column: strategy".
// Error messages are meant for users.
func ParseRule(s string) (Rule, error) {
	target, strategy, ok := strings.Cut(s, ":")
	target, strategy = strings.TrimSpace(target), strings.ToLower(strings.TrimSpace(strategy))
	if !ok || target == "" || strategy == "" {
		return Rule{}, fmt.Errorf("rule %q: write it as table.column: strategy (e.g. users.email: email)", s)
	}
	if _, ok := strategies[strategy]; !ok {
		return Rule{}, fmt.Errorf("rule %q: unknown strategy %q (one of %s)", s, strategy, strings.Join(Strategies, ", "))
	}
	parts := strings.Split(target, ".")
	var r Rule
	switch len(parts) {
	case 2:
		r = Rule{Schema: "public", Table: parts[0], Column: parts[1]}
	case 3:
		r = Rule{Schema: parts[0], Table: parts[1], Column: parts[2]}
	default:
		return Rule{}, fmt.Errorf("rule %q: the column must be table.column or schema.table.column", s)
	}
	r.Strategy = strategy
	if r.Table == "*" {
		if len(parts) != 2 {
			return Rule{}, fmt.Errorf("rule %q: a wildcard rule is *.column", s)
		}
		r.Schema = ""
	} else if !identRe.MatchString(r.Schema) || !identRe.MatchString(r.Table) {
		return Rule{}, fmt.Errorf("rule %q: table names must be lower-case identifiers (a-z, 0-9, _)", s)
	}
	if !identRe.MatchString(r.Column) {
		return Rule{}, fmt.Errorf("rule %q: column names must be lower-case identifiers (a-z, 0-9, _)", s)
	}
	if r.Schema == "pg_catalog" || r.Schema == "information_schema" {
		return Rule{}, fmt.Errorf("rule %q: system schemas cannot be anonymized", s)
	}
	return r, nil
}

// ParseRules parses and checks a rule list: at most MaxRules, each column
// once. Blank lines and lines starting with # are skipped.
func ParseRules(lines []string) ([]Rule, error) {
	var out []Rule
	seen := map[string]bool{}
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		r, err := ParseRule(l)
		if err != nil {
			return nil, err
		}
		key := r.Schema + "." + r.Table + "." + r.Column
		if seen[key] {
			return nil, fmt.Errorf("rule %q: the column has another rule already", l)
		}
		seen[key] = true
		out = append(out, r)
	}
	if len(out) > MaxRules {
		return nil, fmt.Errorf("at most %d anonymization rules", MaxRules)
	}
	return out, nil
}

// NormalizeRules parses lines and returns their canonical form.
func NormalizeRules(lines []string) ([]string, error) {
	rules, err := ParseRules(lines)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		out = append(out, r.String())
	}
	return out, nil
}

func quoteIdent(s string) string { return `"` + s + `"` } // s matched identRe: no quotes inside

// expr is the new value of col (quoted) under strategy.
func expr(strategy, col string) string {
	tmpl := strategies[strategy]
	if strategy == "null" {
		return tmpl
	}
	return "CASE WHEN " + col + " IS NULL THEN NULL ELSE " + strings.ReplaceAll(tmpl, "{c}", col) + " END"
}

// CompileRules turns rules into the SQL the copy Job runs in the branch
// database as the superuser, in one transaction. Triggers and foreign key
// checks are off (session_replication_role = replica): anonymizing must
// not run the application's triggers. Rules of one table become one
// UPDATE; wildcard rules loop over information_schema. An empty rule list
// compiles to "".
func CompileRules(rules []Rule) string {
	if len(rules) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("SET session_replication_role = replica;\n")
	type table struct{ schema, name string }
	var order []table
	sets := map[table][]string{}
	for _, r := range rules {
		if r.Wildcard() {
			continue
		}
		t := table{r.Schema, r.Table}
		if _, ok := sets[t]; !ok {
			order = append(order, t)
		}
		col := quoteIdent(r.Column)
		sets[t] = append(sets[t], col+" = "+expr(r.Strategy, col))
	}
	for _, t := range order {
		fmt.Fprintf(&b, "UPDATE %s.%s SET %s;\n", quoteIdent(t.schema), quoteIdent(t.name), strings.Join(sets[t], ", "))
	}
	for _, r := range rules {
		if !r.Wildcard() {
			continue
		}
		col := quoteIdent(r.Column)
		// The statement goes through format(): %I quotes the table, and the
		// fixed expression has its quotes doubled and % escaped.
		stmt := "UPDATE %I.%I SET " + col + " = " + strings.ReplaceAll(expr(r.Strategy, col), "%", "%%")
		fmt.Fprintf(&b, `DO $paas$
DECLARE r record;
BEGIN
  FOR r IN SELECT c.table_schema, c.table_name FROM information_schema.columns c
    JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
    WHERE c.column_name = '%s' AND t.table_type = 'BASE TABLE'
      AND c.table_schema NOT IN ('pg_catalog', 'information_schema')
    ORDER BY 1, 2
  LOOP
    EXECUTE format('%s', r.table_schema, r.table_name);
  END LOOP;
END $paas$;
`, r.Column, strings.ReplaceAll(stmt, "'", "''"))
	}
	return b.String()
}

// CheckSQL does the checks of the member's own statements that can be done
// without a server: a size limit and no psql meta-commands. The Job passes
// the statements with psql -c, which sends them to the server as they are
// unless they start with a backslash (then psql would run the command
// itself, e.g. \! for a shell); lines starting with one are refused too.
func CheckSQL(sql string, max int) error {
	if len(sql) > max {
		return fmt.Errorf("anonymize_sql is longer than %d bytes", max)
	}
	for _, line := range strings.Split(sql, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), `\`) {
			return errors.New(`anonymize_sql must be plain SQL: psql meta-commands (lines starting with "\") are not allowed`)
		}
	}
	return nil
}
