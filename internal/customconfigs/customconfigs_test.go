package customconfigs

import (
	"bytes"
	"testing"

	"github.com/ColinMario/Protondrive-for-Linux/internal/syncconfig"
)

func TestTemplatesAreStrictValidAndSorted(t *testing.T) {
	templates, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(templates) != 3 {
		t.Fatalf("template count = %d, want 3", len(templates))
	}
	for i, template := range templates {
		if _, err := syncconfig.Parse(template.Raw); err != nil {
			t.Errorf("template %s: %v", template.File, err)
		}
		if i > 0 && templates[i-1].Name > template.Name {
			t.Fatalf("templates are not sorted: %q before %q", templates[i-1].Name, template.Name)
		}
	}
	copyOfList := templates
	copyOfList[0].Name = "mutated"
	copyOfList[0].Raw[0] = 'x'
	again, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Name == "mutated" {
		t.Fatal("List exposed internal slice storage")
	}
	if again[0].Raw[0] == 'x' {
		t.Fatal("List exposed internal template bytes")
	}
}

func TestLookupAliasesAndRawTemplateIsolation(t *testing.T) {
	bySlug, found, err := Lookup("photo-drop-upload")
	if err != nil || !found {
		t.Fatalf("slug lookup: found=%v err=%v", found, err)
	}
	byName, found, err := Lookup("Photo Drop Upload")
	if err != nil || !found {
		t.Fatalf("name lookup: found=%v err=%v", found, err)
	}
	if bySlug.ID != byName.ID || !bytes.Equal(bySlug.Raw, byName.Raw) {
		t.Fatal("aliases resolved to different templates")
	}
	if _, found, err := Lookup("does-not-exist"); err != nil || found {
		t.Fatalf("missing lookup: found=%v err=%v", found, err)
	}
}

func TestSlugify(t *testing.T) {
	for input, want := range map[string]string{
		" Paperless_Export ": "paperless-export",
		"///":                "template",
		"A...B":              "a-b",
	} {
		if got := slugify(input); got != want {
			t.Errorf("slugify(%q) = %q, want %q", input, got, want)
		}
	}
}
