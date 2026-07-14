package customconfigs

import (
	"bytes"
	"embed"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"github.com/ColinMario/Protondrive-for-Linux/internal/syncconfig"
)

// Template holds metadata and the raw JSON for a built-in sync configuration template.
type Template struct {
	ID          string
	File        string
	Name        string
	Description string
	Raw         []byte
}

//go:embed templates/*.json
var templatesFS embed.FS

var (
	templatesOnce sync.Once
	templatesErr  error
	templateList  []Template
	templateMap   map[string]Template
	templateAlias map[string]string
)

// List returns all built-in templates sorted by display name.
func List() ([]Template, error) {
	if err := loadTemplates(); err != nil {
		return nil, err
	}
	result := make([]Template, len(templateList))
	for i := range templateList {
		result[i] = cloneTemplate(templateList[i])
	}
	return result, nil
}

// Lookup retrieves a template by name, slug, or filename (without extension).
func Lookup(name string) (Template, bool, error) {
	if err := loadTemplates(); err != nil {
		return Template{}, false, err
	}
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return Template{}, false, nil
	}
	if slug, ok := templateAlias[key]; ok {
		tpl, ok := templateMap[slug]
		return cloneTemplate(tpl), ok, nil
	}
	return Template{}, false, nil
}

func cloneTemplate(template Template) Template {
	template.Raw = bytes.Clone(template.Raw)
	return template
}

func loadTemplates() error {
	templatesOnce.Do(func() {
		entries, err := templatesFS.ReadDir("templates")
		if err != nil {
			templatesErr = err
			return
		}
		templateList = make([]Template, 0, len(entries))
		templateMap = make(map[string]Template, len(entries))
		templateAlias = make(map[string]string, len(entries)*2)
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			data, err := templatesFS.ReadFile(filepath.ToSlash(filepath.Join("templates", entry.Name())))
			if err != nil {
				templatesErr = fmt.Errorf("read template %s: %w", entry.Name(), err)
				return
			}
			header, err := syncconfig.Parse(data)
			if err != nil {
				templatesErr = fmt.Errorf("template %s: %w", entry.Name(), err)
				return
			}
			name := strings.TrimSpace(header.Name)
			if name == "" {
				name = strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name()))
			}
			desc := strings.TrimSpace(header.Description)
			if desc == "" {
				desc = "(no description)"
			}
			slug := slugify(name)
			tpl := Template{
				ID:          slug,
				File:        entry.Name(),
				Name:        name,
				Description: desc,
				Raw:         data,
			}
			templateList = append(templateList, tpl)
			templateMap[slug] = tpl
			templateAlias[strings.ToLower(name)] = slug
			base := strings.ToLower(strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())))
			templateAlias[base] = slug
			templateAlias[slug] = slug
		}
		sort.Slice(templateList, func(i, j int) bool {
			return templateList[i].Name < templateList[j].Name
		})
	})
	return templatesErr
}

func slugify(input string) string {
	input = strings.ToLower(strings.TrimSpace(input))
	if input == "" {
		return "template"
	}
	var builder strings.Builder
	prevHyphen := false
	for _, r := range input {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			builder.WriteRune(r)
			prevHyphen = false
			continue
		}
		switch r {
		case ' ', '-', '_', '.', '/', '\\':
			if !prevHyphen {
				builder.WriteByte('-')
				prevHyphen = true
			}
		default:
			// skip other characters
		}
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "template"
	}
	return result
}
