package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var ctx = context.Background()

func TestRecordDeployKeepsFirstOwner(t *testing.T) {
	s := open(t)
	if err := s.RecordDeploy(ctx, "blog", "ana@example.com", 3, 100); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordDeploy(ctx, "blog", "bo@example.com", 5, 200); err != nil {
		t.Fatal(err)
	}
	site, err := s.GetSite(ctx, "blog")
	if err != nil {
		t.Fatal(err)
	}
	if site.Owner != "ana@example.com" || site.DeployedBy != "bo@example.com" || site.Files != 5 || site.Bytes != 200 {
		t.Errorf("got %+v", site)
	}
	if site.UpdatedAt.Before(site.CreatedAt) {
		t.Errorf("updated_at %v before created_at %v", site.UpdatedAt, site.CreatedAt)
	}
	var deploys int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM deploys WHERE site = 'blog'`).Scan(&deploys); err != nil || deploys != 2 {
		t.Errorf("deploy log has %d rows (err %v), want 2", deploys, err)
	}
}

func TestListAndDeleteSites(t *testing.T) {
	s := open(t)
	if list, err := s.ListSites(ctx); err != nil || len(list) != 0 {
		t.Fatalf("empty list = %v, %v", list, err)
	}
	for _, name := range []string{"a", "b"} {
		if err := s.RecordDeploy(ctx, name, "ana@example.com", 1, 1); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateDoc(ctx, "a", "posts", "ana@example.com", Doc{"x": 1.0}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateDoc(ctx, "b", "posts", "ana@example.com", Doc{"x": 2.0}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSite(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetSite(ctx, "a"); !errors.Is(err, ErrNotFound) {
		t.Errorf("deleted site lookup = %v", err)
	}
	if docs, _ := s.ListDocs(ctx, "a", "posts", DocQuery{Limit: 10}); len(docs) != 0 {
		t.Errorf("deleted site still has %d docs", len(docs))
	}
	if docs, _ := s.ListDocs(ctx, "b", "posts", DocQuery{Limit: 10}); len(docs) != 1 {
		t.Errorf("other site lost docs: %d", len(docs))
	}
	if list, _ := s.ListSites(ctx); len(list) != 1 || list[0].Name != "b" {
		t.Errorf("list = %+v", list)
	}
}

func TestTokens(t *testing.T) {
	s := open(t)
	if _, err := s.LookupToken(ctx, "nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing token = %v", err)
	}
	if err := s.CreateToken(ctx, "hash1", "ana@example.com", "Ana", "cli"); err != nil {
		t.Fatal(err)
	}
	owner, err := s.LookupToken(ctx, "hash1")
	if err != nil || owner.Email != "ana@example.com" || owner.Name != "Ana" {
		t.Errorf("lookup = %+v, %v", owner, err)
	}
	var used *int64
	s.db.QueryRow(`SELECT last_used_at FROM tokens WHERE hash = 'hash1'`).Scan(&used)
	if used == nil {
		t.Error("last_used_at not recorded")
	}
}

func TestDocCRUD(t *testing.T) {
	s := open(t)
	created, err := s.CreateDoc(ctx, "blog", "posts", "ana@example.com", Doc{
		"title": "Hello", "id": "spoofed", "_created_by": "evil@example.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	id, _ := created["id"].(string)
	if len(id) != 20 || id == "spoofed" {
		t.Errorf("id = %q", id)
	}
	if created["_created_by"] != "ana@example.com" || created["title"] != "Hello" {
		t.Errorf("created = %v", created)
	}
	if created["_created_at"] == nil || created["_updated_at"] == nil {
		t.Errorf("missing timestamps: %v", created)
	}

	updated, err := s.UpdateDoc(ctx, "blog", "posts", id, Doc{"status": "live", "title": nil})
	if err != nil {
		t.Fatal(err)
	}
	if updated["status"] != "live" {
		t.Errorf("status not merged: %v", updated)
	}
	if _, ok := updated["title"]; ok {
		t.Errorf("null did not remove title: %v", updated)
	}
	if updated["_created_by"] != "ana@example.com" {
		t.Errorf("creator changed on update: %v", updated)
	}

	if _, err := s.UpdateDoc(ctx, "blog", "posts", "missing", Doc{"a": 1.0}); !errors.Is(err, ErrNotFound) {
		t.Errorf("update missing = %v", err)
	}
	if err := s.DeleteDoc(ctx, "blog", "posts", id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetDoc(ctx, "blog", "posts", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("get deleted = %v", err)
	}
	if err := s.DeleteDoc(ctx, "blog", "posts", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice = %v", err)
	}
}

func TestSetDoc(t *testing.T) {
	s := open(t)
	doc, created, err := s.SetDoc(ctx, "blog", "config", "main", "ana@example.com", Doc{"theme": "dark", "size": 1.0})
	if err != nil || !created || doc["id"] != "main" {
		t.Fatalf("first set = %v, %v, %v", doc, created, err)
	}
	doc, created, err = s.SetDoc(ctx, "blog", "config", "main", "bo@example.com", Doc{"theme": "light"})
	if err != nil || created {
		t.Fatalf("second set = %v, %v, %v", doc, created, err)
	}
	if doc["theme"] != "light" {
		t.Errorf("not replaced: %v", doc)
	}
	if _, ok := doc["size"]; ok {
		t.Errorf("set should replace, not merge: %v", doc)
	}
	if doc["_created_by"] != "ana@example.com" {
		t.Errorf("creator should stay the original: %v", doc)
	}
}

func TestDocsIsolatedBySiteAndCollection(t *testing.T) {
	s := open(t)
	d, _ := s.CreateDoc(ctx, "a", "posts", "x@example.com", Doc{"n": 1.0})
	id := d["id"].(string)
	if _, err := s.GetDoc(ctx, "b", "posts", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("other site can read doc: %v", err)
	}
	if _, err := s.GetDoc(ctx, "a", "comments", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("other collection can read doc: %v", err)
	}
	if err := s.DeleteDoc(ctx, "b", "posts", id); !errors.Is(err, ErrNotFound) {
		t.Errorf("other site can delete doc: %v", err)
	}
}

func TestListDocsFiltersAndOrder(t *testing.T) {
	s := open(t)
	seed := []Doc{
		{"name": "a", "n": 1.0, "done": true, "tag": nil, "author": map[string]any{"email": "ana@example.com"}},
		{"name": "b", "n": 2.0, "done": false, "author": map[string]any{"email": "bo@example.com"}},
		{"name": "c", "n": 2.0, "done": true},
	}
	for _, d := range seed {
		if _, err := s.CreateDoc(ctx, "blog", "items", "x@example.com", d); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // created_at has millisecond precision; keep insertion order stable
	}
	names := func(q DocQuery) []string {
		t.Helper()
		if q.Limit == 0 {
			q.Limit = 100
		}
		docs, err := s.ListDocs(ctx, "blog", "items", q)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, d := range docs {
			out = append(out, d["name"].(string))
		}
		return out
	}
	eq := func(got []string, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	tests := []struct {
		name string
		q    DocQuery
		want []string
	}{
		{"all, oldest first", DocQuery{}, []string{"a", "b", "c"}},
		{"desc", DocQuery{Desc: true}, []string{"c", "b", "a"}},
		{"limit", DocQuery{Limit: 2}, []string{"a", "b"}},
		{"offset", DocQuery{Limit: 2, Offset: 2}, []string{"c"}},
		{"string", DocQuery{Where: map[string]any{"name": "b"}}, []string{"b"}},
		{"number", DocQuery{Where: map[string]any{"n": 2.0}}, []string{"b", "c"}},
		{"bool true", DocQuery{Where: map[string]any{"done": true}}, []string{"a", "c"}},
		{"bool false", DocQuery{Where: map[string]any{"done": false}}, []string{"b"}},
		{"null matches missing and null", DocQuery{Where: map[string]any{"tag": nil}}, []string{"a", "b", "c"}},
		{"nested path", DocQuery{Where: map[string]any{"author.email": "bo@example.com"}}, []string{"b"}},
		{"anded", DocQuery{Where: map[string]any{"n": 2.0, "done": true}}, []string{"c"}},
		{"no match", DocQuery{Where: map[string]any{"name": "zzz"}}, nil},
	}
	for _, tt := range tests {
		if got := names(tt.q); !eq(got, tt.want...) {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestListDocsRejectsBadFilters(t *testing.T) {
	s := open(t)
	for _, where := range []map[string]any{
		{"a') OR 1=1 --": "x"},
		{"$.x": "x"},
		{"a..b": "x"},
		{"obj": map[string]any{"x": 1.0}},
		{"arr": []any{1.0}},
	} {
		_, err := s.ListDocs(ctx, "blog", "items", DocQuery{Where: where, Limit: 10})
		var bad *BadQueryError
		if !errors.As(err, &bad) {
			t.Errorf("where %v: got %v, want BadQueryError", where, err)
		}
	}
}

func TestValidators(t *testing.T) {
	for name, want := range map[string]bool{"posts": true, "my_posts-2": true, "": false, "a/b": false, "a b": false} {
		if ValidCollection(name) != want {
			t.Errorf("ValidCollection(%q) != %v", name, want)
		}
	}
	for id, want := range map[string]bool{"abc": true, "A_b-1": true, "": false, "../x": false, "a.b": false} {
		if ValidDocID(id) != want {
			t.Errorf("ValidDocID(%q) != %v", id, want)
		}
	}
}

func TestNewIDUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 1000 {
		id := NewID()
		if seen[id] || !ValidDocID(id) {
			t.Fatalf("bad or duplicate id %q", id)
		}
		seen[id] = true
	}
}

func TestFiles(t *testing.T) {
	s := open(t)
	mk := func(site, id string, size int64, at time.Time) {
		t.Helper()
		f := &File{ID: id, Name: id + ".png", ContentType: "image/png", Size: size, CreatedBy: "ana@example.com", CreatedAt: at}
		if err := s.CreateFile(ctx, site, f); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().UTC().Truncate(time.Millisecond)
	mk("blog", "f1", 100, base)
	mk("blog", "f2", 250, base.Add(time.Second))
	mk("other", "f3", 999, base)

	f, err := s.GetFile(ctx, "blog", "f1")
	if err != nil || f.Name != "f1.png" || f.Size != 100 || !f.CreatedAt.Equal(base) {
		t.Errorf("get = %+v, %v", f, err)
	}
	if _, err := s.GetFile(ctx, "other", "f1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("cross-site get = %v", err)
	}
	if list, _ := s.ListFiles(ctx, "blog", 10, 0); len(list) != 2 || list[0].ID != "f2" {
		t.Errorf("list = %+v", list)
	}
	if list, _ := s.ListFiles(ctx, "blog", 1, 1); len(list) != 1 || list[0].ID != "f1" {
		t.Errorf("paged list = %+v", list)
	}
	if n, _ := s.UploadedBytes(ctx, "blog"); n != 350 {
		t.Errorf("uploaded bytes = %d", n)
	}
	if n, _ := s.UploadedBytes(ctx, "empty"); n != 0 {
		t.Errorf("empty site bytes = %d", n)
	}
	if err := s.DeleteFile(ctx, "blog", "f1"); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteFile(ctx, "blog", "f1"); !errors.Is(err, ErrNotFound) {
		t.Errorf("delete twice = %v", err)
	}
	if err := s.DeleteSite(ctx, "blog"); err != nil {
		t.Fatal(err)
	}
	if list, _ := s.ListFiles(ctx, "blog", 10, 0); len(list) != 0 {
		t.Errorf("files survived site delete: %+v", list)
	}
	if n, _ := s.UploadedBytes(ctx, "other"); n != 999 {
		t.Errorf("other site affected: %d", n)
	}
}
