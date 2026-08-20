package object

import (
	"strings"
	"testing"
)

// --- Environment ---

const envDoc = `apiVersion: drover/v1
kind: Environment
metadata:
  name: prod
spec:
  description: production
  variables:
    baseUrl: https://api.acme.com
    tenant: acme
  secrets:
    token: ${ACME_PROD_TOKEN}
`

func TestEnvironmentParses(t *testing.T) {
	o := mustParse(t, "env.yaml", envDoc)[0]
	spec, err := o.Environment()
	if err != nil {
		t.Fatal(err)
	}
	if spec.Variables["baseUrl"] != "https://api.acme.com" {
		t.Errorf("variables = %v", spec.Variables)
	}
	if !spec.IsSecret("token") || spec.IsSecret("baseUrl") {
		t.Error("secret classification is wrong")
	}
}

// The whole point of the secrets block is that it holds references. A literal
// there is a credential in a file people commit.
func TestEnvironmentRejectsLiteralSecret(t *testing.T) {
	cases := map[string]string{
		"bare literal":      "token: hunter2",
		"partial":           `token: "Bearer ${TOKEN}"`,
		"two references":    `token: "${A}${B}"`,
		"environment-style": `token: "{{other}}"`,
		"empty reference":   "token: ${}",
	}
	for name, line := range cases {
		t.Run(name, func(t *testing.T) {
			doc := strings.Replace(envDoc, "token: ${ACME_PROD_TOKEN}", line, 1)
			_, err := Parse("env.yaml", []byte(doc))
			if err == nil {
				t.Fatalf("%s was accepted as a secret", line)
			}
			if !strings.Contains(err.Error(), "${ENV_VAR}") {
				t.Errorf("error = %q, want it to explain the rule", err)
			}
		})
	}
}

func TestEnvironmentLookup(t *testing.T) {
	spec, err := mustParse(t, "env.yaml", envDoc)[0].Environment()
	if err != nil {
		t.Fatal(err)
	}
	getenv := func(n string) (string, bool) {
		if n == "ACME_PROD_TOKEN" {
			return "s3cret", true
		}
		return "", false
	}

	if v, ok := spec.Lookup("baseUrl", getenv); !ok || v != "https://api.acme.com" {
		t.Errorf("variable lookup = %q, %v", v, ok)
	}
	if v, ok := spec.Lookup("token", getenv); !ok || v != "s3cret" {
		t.Errorf("secret lookup = %q, %v", v, ok)
	}
	if _, ok := spec.Lookup("nope", getenv); ok {
		t.Error("an unknown name resolved")
	}

	// A secret whose backing variable is unset fails at request time, not at
	// apply time -- the engine may be restarted with more env set.
	if _, ok := spec.Lookup("token", func(string) (string, bool) { return "", false }); ok {
		t.Error("an unset secret resolved")
	}
}

// get must never print a secret's value, only where it comes from and whether
// it is there.
func TestEnvironmentSecretStatusHidesValues(t *testing.T) {
	spec, err := mustParse(t, "env.yaml", envDoc)[0].Environment()
	if err != nil {
		t.Fatal(err)
	}
	statuses := spec.SecretStatuses(func(n string) (string, bool) { return "s3cret", n == "ACME_PROD_TOKEN" })
	if len(statuses) != 1 {
		t.Fatalf("got %d statuses", len(statuses))
	}
	st := statuses[0]
	if st.Name != "token" || st.FromEnv != "ACME_PROD_TOKEN" || !st.Set {
		t.Errorf("status = %+v", st)
	}
	// The value itself must appear nowhere in the struct.
	if strings.Contains(st.Name+st.FromEnv, "s3cret") {
		t.Error("the secret value leaked into its status")
	}
}

func TestEnvironmentRejectsNameInBothBlocks(t *testing.T) {
	doc := strings.Replace(envDoc, "    tenant: acme", "    token: oops", 1)
	if _, err := Parse("env.yaml", []byte(doc)); err == nil {
		t.Fatal("a name in both variables and secrets was accepted")
	}
}

func TestEmptyEnvironmentRejected(t *testing.T) {
	doc := `apiVersion: drover/v1
kind: Environment
metadata:
  name: empty
spec:
  description: nothing here
`
	if _, err := Parse("env.yaml", []byte(doc)); err == nil {
		t.Fatal("an environment with no variables or secrets was accepted")
	}
}

// --- HTTPRequest ---

const reqDoc = `apiVersion: drover/v1
kind: HTTPRequest
metadata:
  name: get-user
spec:
  description: Fetch one user by id.
  method: GET
  url: "{{baseUrl}}/v1/users/{userId}"
  environments: [local, stage, prod]
  defaultEnvironment: stage
  pathParams:
    - name: userId
      description: The user's id.
      required: true
      example: usr_1a2b
  query:
    - name: include
      description: Relations to expand.
  headers:
    - name: Authorization
      value: "Bearer {{token}}"
`

func TestHTTPRequestParses(t *testing.T) {
	spec, err := mustParse(t, "req.yaml", reqDoc)[0].HTTPRequest()
	if err != nil {
		t.Fatal(err)
	}
	if spec.NormalizedMethod() != "GET" || !spec.IsSafe() {
		t.Errorf("method = %q safe = %v", spec.NormalizedMethod(), spec.IsSafe())
	}
	if got := spec.RequiredParams(); len(got) != 1 || got[0] != "userId" {
		t.Errorf("required = %v, want [userId]", got)
	}
}

// The check that keeps an advertised tool honest: {name} in the url and the
// declared pathParams must agree, both ways.
func TestHTTPRequestParamURLAgreement(t *testing.T) {
	t.Run("url uses an undeclared param", func(t *testing.T) {
		doc := strings.Replace(reqDoc, "/{userId}", "/{userId}/{orgId}", 1)
		_, err := Parse("req.yaml", []byte(doc))
		if err == nil {
			t.Fatal("an undeclared {orgId} was accepted")
		}
		if !strings.Contains(err.Error(), "orgId") {
			t.Errorf("error = %q", err)
		}
	})

	t.Run("param declared but never used", func(t *testing.T) {
		doc := strings.Replace(reqDoc, `  url: "{{baseUrl}}/v1/users/{userId}"`, `  url: "{{baseUrl}}/v1/users"`, 1)
		_, err := Parse("req.yaml", []byte(doc))
		if err == nil {
			t.Fatal("a pathParam that the url never uses was accepted")
		}
		if !strings.Contains(err.Error(), "userId") {
			t.Errorf("error = %q", err)
		}
	})
}

// A description is what an agent reads to fill a value in. Without one the
// tool is unusable, so it is required rather than optional.
func TestHTTPRequestRequiresParamDescriptions(t *testing.T) {
	doc := strings.Replace(reqDoc, "      description: The user's id.\n", "", 1)
	_, err := Parse("req.yaml", []byte(doc))
	if err == nil {
		t.Fatal("a parameter with no description was accepted")
	}
	if !strings.Contains(err.Error(), "description") {
		t.Errorf("error = %q", err)
	}
}

// A caller-settable header is a credential-theft and request-smuggling
// primitive, so headers may only interpolate server-side values.
func TestHTTPRequestRejectsCallerControlledHeader(t *testing.T) {
	doc := strings.Replace(reqDoc, `      value: "Bearer {{token}}"`, `      value: "Bearer {token}"`, 1)
	_, err := Parse("req.yaml", []byte(doc))
	if err == nil {
		t.Fatal("a header interpolating a caller parameter was accepted")
	}
	if !strings.Contains(err.Error(), "never caller-supplied") {
		t.Errorf("error = %q", err)
	}
}

func TestHTTPRequestRejectsBadDefaults(t *testing.T) {
	doc := strings.Replace(reqDoc, "  defaultEnvironment: stage", "  defaultEnvironment: nowhere", 1)
	if _, err := Parse("req.yaml", []byte(doc)); err == nil {
		t.Fatal("a defaultEnvironment outside environments was accepted")
	}
}

func TestHTTPRequestSelectEnvironment(t *testing.T) {
	spec, err := mustParse(t, "req.yaml", reqDoc)[0].HTTPRequest()
	if err != nil {
		t.Fatal(err)
	}
	if got, err := spec.SelectEnvironment(""); err != nil || got != "stage" {
		t.Errorf("default = %q, %v; want stage", got, err)
	}
	if got, err := spec.SelectEnvironment("prod"); err != nil || got != "prod" {
		t.Errorf("explicit = %q, %v", got, err)
	}
	if _, err := spec.SelectEnvironment("nowhere"); err == nil {
		t.Error("an environment outside the list was accepted")
	}

	// With several environments and no default, the caller must choose.
	spec.DefaultEnvironment = ""
	if _, err := spec.SelectEnvironment(""); err == nil {
		t.Error("no default with several environments should force a choice")
	}
}

func TestHTTPRequestNonGetIsStoredButNotSafe(t *testing.T) {
	doc := strings.Replace(reqDoc, "  method: GET", "  method: POST", 1)
	spec, err := mustParse(t, "req.yaml", doc)[0].HTTPRequest()
	if err != nil {
		t.Fatalf("a POST must still be storable: %v", err)
	}
	if spec.IsSafe() {
		t.Error("POST reported as safe; it must never be offered to an agent")
	}
}

func TestHTTPRequestRejectsRelativeURL(t *testing.T) {
	doc := strings.Replace(reqDoc, `  url: "{{baseUrl}}/v1/users/{userId}"`, `  url: "/v1/users/{userId}"`, 1)
	if _, err := Parse("req.yaml", []byte(doc)); err == nil {
		t.Fatal("a relative url with no {{baseUrl}} was accepted")
	}
}

// --- SQLConnection ---

func sqlDoc(body string) string {
	return "apiVersion: drover/v1\nkind: SQLConnection\nmetadata:\n  name: prod\nspec:\n" + body
}

func TestSQLProviders(t *testing.T) {
	cases := []struct {
		body     string
		provider Provider
		driver   string
	}{
		{"  url: postgres://host/db\n", ProviderPostgres, "pgx"},
		{"  url: postgresql://host/db\n", ProviderPostgres, "pgx"},
		{"  url: mysql://host/db\n", ProviderMySQL, "mysql"},
		{"  url: redshift://host:5439/db\n", ProviderRedshift, "pgx"},
		{"  provider: redshift\n  url: ${DATABASE_URL}\n", ProviderRedshift, "pgx"},
		{"  provider: mariadb\n  url: ${DATABASE_URL}\n", ProviderMySQL, "mysql"},
		{"  provider: pg\n  url: ${DATABASE_URL}\n", ProviderPostgres, "pgx"},
	}
	for _, tc := range cases {
		t.Run(tc.body, func(t *testing.T) {
			spec, err := mustParse(t, "sql.yaml", sqlDoc(tc.body))[0].SQLConnection()
			if err != nil {
				t.Fatal(err)
			}
			got, err := spec.ResolveProvider()
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.provider {
				t.Errorf("provider = %q, want %q", got, tc.provider)
			}
			if got.Driver() != tc.driver {
				t.Errorf("driver = %q, want %q", got.Driver(), tc.driver)
			}
		})
	}
}

// Redshift rides the Postgres driver but stays its own provider, because the
// dialect an agent must write against is not the same.
func TestRedshiftIsItsOwnProviderOnThePostgresDriver(t *testing.T) {
	if ProviderRedshift.Driver() != ProviderPostgres.Driver() {
		t.Error("redshift should share the postgres driver")
	}
	if ProviderRedshift.Dialect() == ProviderPostgres.Dialect() {
		t.Error("redshift should not claim the postgres dialect")
	}
	if !strings.Contains(ProviderRedshift.Dialect(), "Redshift") {
		t.Errorf("dialect = %q", ProviderRedshift.Dialect())
	}
}

// A ${ENV} url hides its scheme, so the provider has to be written down.
func TestSQLProviderRequiredWhenURLIsAReference(t *testing.T) {
	_, err := Parse("sql.yaml", []byte(sqlDoc("  url: ${DATABASE_URL}\n")))
	if err == nil {
		t.Fatal("a ${ENV} url with no provider was accepted")
	}
	if !strings.Contains(err.Error(), "provider") {
		t.Errorf("error = %q", err)
	}
}

func TestSQLRejectsUnknownProvider(t *testing.T) {
	_, err := Parse("sql.yaml", []byte(sqlDoc("  provider: oracle\n  url: ${DATABASE_URL}\n")))
	if err == nil {
		t.Fatal("an unsupported provider was accepted")
	}
	if !strings.Contains(err.Error(), "postgres") {
		t.Errorf("error = %q, want it to list what is supported", err)
	}
}

// A DSN with a password in a file people commit is a leaked credential.
func TestSQLRejectsInlinePassword(t *testing.T) {
	_, err := Parse("sql.yaml", []byte(sqlDoc("  url: postgres://user:hunter2@host/db\n")))
	if err == nil {
		t.Fatal("a url with an inline password was accepted")
	}
	if !strings.Contains(err.Error(), "${ENV_VAR}") {
		t.Errorf("error = %q, want it to point at the fix", err)
	}

	// Without a password it is fine.
	if _, err := Parse("sql.yaml", []byte(sqlDoc("  url: postgres://user@host/db\n"))); err != nil {
		t.Errorf("a passwordless url was rejected: %v", err)
	}
}

func TestSQLReadOnlyDefaultsToTrue(t *testing.T) {
	spec, err := mustParse(t, "sql.yaml", sqlDoc("  url: postgres://host/db\n"))[0].SQLConnection()
	if err != nil {
		t.Fatal(err)
	}
	if !spec.IsReadOnly() {
		t.Error("readOnly defaulted to false; a database tool handed to an agent must not write by default")
	}
	if spec.RowLimit() != DefaultMaxRows {
		t.Errorf("row limit = %d, want %d", spec.RowLimit(), DefaultMaxRows)
	}

	spec, err = mustParse(t, "sql.yaml", sqlDoc("  url: postgres://host/db\n  readOnly: false\n  maxRows: 10\n"))[0].SQLConnection()
	if err != nil {
		t.Fatal(err)
	}
	if spec.IsReadOnly() {
		t.Error("an explicit readOnly: false was ignored")
	}
	if spec.RowLimit() != 10 {
		t.Errorf("row limit = %d, want 10", spec.RowLimit())
	}
}

func TestCheckReadOnly(t *testing.T) {
	allowed := []string{
		"SELECT 1",
		"select * from users",
		"  SELECT 1  ",
		"SELECT 1;",
		"WITH x AS (SELECT 1) SELECT * FROM x",
		"EXPLAIN SELECT 1",
		"SHOW TABLES",
		"DESCRIBE users",
		"VALUES (1)",
		"(SELECT 1)",
		"-- a comment\nSELECT 1",
		"/* comment */ SELECT 1",
		"SELECT 'a; b' FROM t",
	}
	for _, q := range allowed {
		if err := CheckReadOnly(q); err != nil {
			t.Errorf("CheckReadOnly(%q) = %v, want allowed", q, err)
		}
	}

	refused := []string{
		"",
		"INSERT INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"DROP TABLE t",
		"TRUNCATE t",
		"CREATE TABLE t (a int)",
		"ALTER TABLE t ADD b int",
		"GRANT ALL ON t TO x",
		"COPY t FROM '/tmp/x'",
		"UNLOAD ('SELECT 1') TO 's3://b/'", // redshift
		"CALL do_thing()",
		"SET search_path = x",
		"BEGIN",
		"SELECT 1; DROP TABLE users", // stacked
		"WITH x AS (INSERT INTO t VALUES (1) RETURNING *) SELECT * FROM x", // writing CTE
		"-- SELECT 1\nDROP TABLE t",                                        // comment cannot disguise it
		"MERGE INTO t USING s ON x",
		"REFRESH MATERIALIZED VIEW v",
	}
	for _, q := range refused {
		if err := CheckReadOnly(q); err == nil {
			t.Errorf("CheckReadOnly(%q) = nil, want refused", q)
		}
	}
}

// An unrecognised statement must fail closed rather than be assumed harmless.
func TestCheckReadOnlyFailsClosed(t *testing.T) {
	err := CheckReadOnly("FROBNICATE the_database")
	if err == nil {
		t.Fatal("an unrecognised statement was allowed")
	}
	if !strings.Contains(err.Error(), "not a recognised read") {
		t.Errorf("error = %q", err)
	}
}

// A health query runs under the same rules, so one that would be refused at
// run time must not pass apply.
func TestSQLHealthQueryMustBeReadOnly(t *testing.T) {
	_, err := Parse("sql.yaml", []byte(sqlDoc("  url: postgres://host/db\n  health: DELETE FROM t\n")))
	if err == nil {
		t.Fatal("a writing health query was accepted on a read-only connection")
	}

	// With writes allowed it is the user's call.
	if _, err := Parse("sql.yaml", []byte(sqlDoc("  url: postgres://host/db\n  readOnly: false\n  health: DELETE FROM t\n"))); err != nil {
		t.Errorf("readOnly: false should permit it: %v", err)
	}
}
