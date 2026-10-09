package registry

import (
	"sort"

	"github.com/XuLaiQ/all2api/internal/ports"
)

type Registry struct {
	items map[string]ports.AdapterRegistration
}

func New(adapters map[string]ports.Adapter) *Registry {
	items := map[string]ports.AdapterRegistration{
		"wb": {
			Manifest: ports.AdapterManifest{Slug: "wb", Name: "WorkBuddy", Version: "0.1.0", Protocols: []string{"openai", "anthropic", "responses"}, Capabilities: []string{"chat"}},
		},
		"doubao": {
			Manifest: ports.AdapterManifest{Slug: "doubao", Name: "Doubao", Version: "0.1.0", Protocols: []string{"openai", "anthropic", "responses"}, Capabilities: []string{"chat"}},
		},
		"chatgpt": {
			Manifest: ports.AdapterManifest{Slug: "chatgpt", Name: "ChatGPT", Version: "0.1.0", Protocols: []string{"openai", "anthropic", "responses"}, Capabilities: []string{"chat"}},
		},
	}
	for slug, adapter := range adapters {
		item, ok := items[slug]
		if !ok {
			item = ports.AdapterRegistration{Manifest: ports.AdapterManifest{Slug: slug, Name: slug, Version: "0.1.0"}}
		}
		item.Adapter = adapter
		items[slug] = item
	}
	return &Registry{items: items}
}

func (r *Registry) List() []ports.AdapterRegistration {
	result := make([]ports.AdapterRegistration, 0, len(r.items))
	for _, item := range r.items {
		result = append(result, item)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Manifest.Slug < result[j].Manifest.Slug })
	return result
}

func (r *Registry) Get(slug string) (ports.AdapterRegistration, bool) {
	item, ok := r.items[slug]
	return item, ok
}
