package registry

import (
	"testing"

	"github.com/XuLaiQ/all2api/internal/adapters/fake"
	"github.com/XuLaiQ/all2api/internal/ports"
)

func TestNewRegistersGrokManifest(t *testing.T) {
	item, ok := New(map[string]ports.Adapter{"grok": &fake.Adapter{Slug: "grok"}}).Get("grok")
	if !ok {
		t.Fatal("Grok manifest was not registered")
	}
	if item.Manifest.Name != "Grok" || item.Manifest.Slug != "grok" || item.Adapter == nil {
		t.Fatalf("Grok registration = %#v", item)
	}
}
