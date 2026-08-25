package object

import (
	"strings"
	"testing"
	"time"
)

const repoDoc = `apiVersion: drover/v1
kind: Repository
metadata:
  name: api
spec:
  url: https://github.com/acme/api
  branch: main
`

func mustParse(t *testing.T, src, doc string) []*Object {
	t.Helper()
	objs, err := Parse(src, []byte(doc))
	if err != nil {
		t.Fatalf("Parse(%s): %v", src, err)
	}
	return objs
}

func TestParseRepository(t *testing.T) {
	objs := mustParse(t, "repo.yaml", repoDoc)
	if len(objs) != 1 {
		t.Fatalf("got %d objects, want 1", len(objs))
	}
	o := objs[0]
	if o.Kind != KindRepository {
		t.Errorf("kind = %q, want %q", o.Kind, KindRepository)
	}
	if o.Metadata.Name != "api" {
		t.Errorf("name = %q, want api", o.Metadata.Name)
	}
	if o.Ref().String() != "Repository/api" {
		t.Errorf("ref = %q", o.Ref())
	}

	spec, err := o.Repository()
	if err != nil {
		t.Fatalf("Repository(): %v", err)
	}
	if spec.URL != "https://github.com/acme/api" || spec.Branch != "main" {
		t.Errorf("spec = %+v", spec)
	}
	if spec.RefreshInterval.Set {
		t.Error("refreshInterval should be unset when the document omits it")
	}
}

func TestParseMultiDoc(t *testing.T) {
	doc := repoDoc + "---\n" + strings.Replace(repoDoc, "name: api", "name: web", 1) + "---\n"
	objs := mustParse(t, "many.yaml", doc)
	if len(objs) != 2 {
		t.Fatalf("got %d objects, want 2 (a trailing --- is not a document)", len(objs))
	}
	if objs[1].Index != 1 {
		t.Errorf("second document Index = %d, want 1", objs[1].Index)
	}
	if !strings.Contains(objs[1].Where(), "document 2") {
		t.Errorf("Where() = %q, want it to name the document", objs[1].Where())
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		name, doc, want string
	}{
		{"no apiVersion", "kind: Repository\nmetadata:\n  name: api\nspec:\n  url: https://x/y\n  branch: main\n", "apiVersion is required"},
		{"wrong apiVersion", "apiVersion: v1\nkind: Repository\nmetadata:\n  name: api\nspec: {}\n", "unsupported apiVersion"},
		{"unknown kind", "apiVersion: drover/v1\nkind: Widget\nmetadata:\n  name: api\nspec: {}\n", "unknown kind"},
		{"short form kind", "apiVersion: drover/v1\nkind: Repo\nmetadata:\n  name: api\nspec: {}\n", "unknown kind"},
		{"no name", "apiVersion: drover/v1\nkind: Repository\nmetadata: {}\nspec:\n  url: https://x/y\n  branch: main\n", "name: is required"},
		{"no url", "apiVersion: drover/v1\nkind: Repository\nmetadata:\n  name: api\nspec:\n  branch: main\n", "spec.url is required"},
		{"no branch", "apiVersion: drover/v1\nkind: Repository\nmetadata:\n  name: api\nspec:\n  url: https://x/y\n", "spec.branch is required"},
		{"typo in field", "apiVersion: drover/v1\nkind: Repository\nmetadata:\n  name: api\nspec:\n  url: https://x/y\n  branch: main\n  refreshIntervl: 5m\n", "refreshIntervl"},
		{"empty file", "", "no documents"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse("bad.yaml", []byte(tc.doc))
			if err == nil {
				t.Fatal("want an error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A "Repo" kind must not quietly work. Spelling kinds out is the rule, and a
// helpful alias here would undermine it everywhere else.
func TestKindHasNoShortForm(t *testing.T) {
	for _, short := range []string{"repo", "repos", "env", "http", "sql"} {
		if _, err := ParseKind(short); err == nil {
			t.Errorf("ParseKind(%q) succeeded; short forms must be rejected", short)
		}
	}
	for _, full := range []string{"repository", "Repository", "REPOSITORY", "repositories", "environment", "httprequest", "sqlconnection"} {
		if _, err := ParseKind(full); err != nil {
			t.Errorf("ParseKind(%q): %v", full, err)
		}
	}
}

func TestValidateName(t *testing.T) {
	// One dot is legal: it is the namespace separator for an object a
	// repository declared about itself.
	ok := []string{"api", "a", "api-server", "x1", "1x", "api.get-user", strings.Repeat("a", MaxNameLen)}
	for _, n := range ok {
		if err := ValidateName(n); err != nil {
			t.Errorf("ValidateName(%q) = %v, want ok", n, err)
		}
	}
	bad := []string{"", "API", "Api", "-api", "api-", "api_server", "api/server", "..", ".", ".api", "api.", "a.b.c", "a b", strings.Repeat("a", MaxNameLen+1)}
	for _, n := range bad {
		if err := ValidateName(n); err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", n)
		}
	}
}

// Uppercase is refused because the name becomes a directory, and on a
// case-insensitive filesystem two spellings would collide over one checkout.
func TestNameRejectsUppercaseWithReason(t *testing.T) {
	err := ValidateName("API")
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "lowercase") {
		t.Errorf("error = %q, want it to explain the lowercase rule", err)
	}
}

func TestRefreshInterval(t *testing.T) {
	cases := []struct {
		in       string
		wantErr  bool
		never    bool
		duration time.Duration
	}{
		{in: "15m", duration: 15 * time.Minute},
		{in: "30s", duration: 30 * time.Second},
		{in: "24h", duration: 24 * time.Hour},
		{in: "never", never: true},
		{in: "off", never: true},
		{in: "0", never: true},
		{in: "10s", wantErr: true},  // below the minimum
		{in: "30", wantErr: true},   // no unit
		{in: "-5m", wantErr: true},  // negative
		{in: "soon", wantErr: true}, // not a duration
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			doc := repoDoc + "  refreshInterval: " + tc.in + "\n"
			objs, err := Parse("repo.yaml", []byte(doc))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("refreshInterval %q parsed, want an error", tc.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			spec, err := objs[0].Repository()
			if err != nil {
				t.Fatal(err)
			}
			got := spec.RefreshInterval
			if !got.Set {
				t.Fatal("Set = false, want true")
			}
			if got.Never != tc.never {
				t.Errorf("Never = %v, want %v", got.Never, tc.never)
			}
			if got.Duration != tc.duration {
				t.Errorf("Duration = %v, want %v", got.Duration, tc.duration)
			}
		})
	}
}

func TestIntervalResolve(t *testing.T) {
	def := time.Hour

	if d, tick := (Interval{}).Resolve(def); d != def || !tick {
		t.Errorf("unset resolved to (%v, %v), want (%v, true)", d, tick, def)
	}
	if _, tick := (Interval{Set: true, Never: true}).Resolve(def); tick {
		t.Error("never resolved to ticking, want no ticking")
	}
	if d, tick := (Interval{Set: true, Duration: 5 * time.Minute}).Resolve(def); d != 5*time.Minute || !tick {
		t.Errorf("explicit resolved to (%v, %v), want (5m, true)", d, tick)
	}
	// A server started with --sync 0 turns the ticker off for objects that
	// did not ask for their own cadence.
	if _, tick := (Interval{}).Resolve(0); tick {
		t.Error("unset with a zero default resolved to ticking, want no ticking")
	}
}

func TestRepositoryURLValidation(t *testing.T) {
	ok := []string{
		"https://github.com/acme/api",
		"http://git.internal/acme/api.git",
		"ssh://git@github.com/acme/api.git",
		"git@github.com:acme/api.git",
		"git://host/repo",
		"file:///srv/git/api",
		"/srv/git/api", // a local mirror is a real remote
	}
	for _, u := range ok {
		s := &RepositorySpec{URL: u, Branch: "main"}
		if err := s.Validate(); err != nil {
			t.Errorf("url %q: %v", u, err)
		}
	}
	bad := []string{"", "  ", "not a url", "--upload-pack=evil", "ftp://host/repo", "https://", "relative/path"}
	for _, u := range bad {
		s := &RepositorySpec{URL: u, Branch: "main"}
		if err := s.Validate(); err == nil {
			t.Errorf("url %q was accepted, want an error", u)
		}
	}
}

func TestRepositoryBranchValidation(t *testing.T) {
	ok := []string{"main", "release/2.0", "feature/api-v2", "v1.2.3"}
	for _, b := range ok {
		s := &RepositorySpec{URL: "https://x/y", Branch: b}
		if err := s.Validate(); err != nil {
			t.Errorf("branch %q: %v", b, err)
		}
	}
	bad := []string{"", "-main", "/main", "main/", "main.lock", "a..b", "a//b", "a b", "a^b", "a:b", "a*b"}
	for _, b := range bad {
		s := &RepositorySpec{URL: "https://x/y", Branch: b}
		if err := s.Validate(); err == nil {
			t.Errorf("branch %q was accepted, want an error", b)
		}
	}
}
