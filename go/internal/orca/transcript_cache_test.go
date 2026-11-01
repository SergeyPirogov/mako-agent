package orca

import (
	"strconv"
	"testing"
)

func TestConvCacheBounded(t *testing.T) {
	convMu.Lock()
	defer convMu.Unlock()
	convCache = map[string]conversationCache{}
	storeConv("keep", conversationCache{})
	for i := 0; i < convCacheMax*2; i++ {
		storeConv(strconv.Itoa(i), conversationCache{})
		storeConv("keep", conversationCache{}) // stays in use
	}
	if len(convCache) != convCacheMax {
		t.Fatalf("cache has %d entries, want %d", len(convCache), convCacheMax)
	}
	if _, ok := convCache["keep"]; !ok {
		t.Fatal("recently used entry was evicted")
	}
	convCache = map[string]conversationCache{}
}
