package main

import "testing"

func TestMessageNodeStoreEvictExcessKeepsNewestMessageIDs(t *testing.T) {
	t.Parallel()

	store := newMessageNodeStore(2)
	store.getOrCreate("2")
	store.getOrCreate("1")
	store.getOrCreate("10")

	store.evictExcess()

	if len(store.nodes) != 2 {
		t.Fatalf("unexpected node count after eviction: %d", len(store.nodes))
	}

	if _, ok := store.nodes["1"]; ok {
		t.Fatal("expected oldest message id to be evicted")
	}

	if _, ok := store.nodes["2"]; !ok {
		t.Fatal("expected newer message id 2 to remain")
	}

	if _, ok := store.nodes["10"]; !ok {
		t.Fatal("expected newer message id 10 to remain")
	}
}
