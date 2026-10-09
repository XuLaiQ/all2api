package httptransport

import "testing"

func TestManifestCapabilityGateDoesNotExposeUnverifiedMedia(t *testing.T) {
	if manifestHasCapability([]string{"chat"}, "image") {
		t.Fatal("image capability was exposed by a chat-only manifest")
	}
	if !manifestHasCapability([]string{"chat", "image"}, "image") {
		t.Fatal("declared image capability was rejected by the manifest gate")
	}
}
