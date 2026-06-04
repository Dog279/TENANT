package model_test

import (
	"testing"

	"tenant/internal/model"
)

func TestEmbedCache_HitMiss(t *testing.T) {
	c := model.NewEmbedCache(8)
	if _, ok := c.Get("e1", "hello"); ok {
		t.Fatal("Get on empty cache returned ok")
	}
	c.Put("e1", "hello", []float32{1, 2, 3})
	v, ok := c.Get("e1", "hello")
	if !ok {
		t.Fatal("Get after Put missed")
	}
	if len(v) != 3 || v[0] != 1 {
		t.Fatalf("Get returned %v, want [1 2 3]", v)
	}
}

func TestEmbedCache_KeyedByEmbedder(t *testing.T) {
	c := model.NewEmbedCache(8)
	c.Put("e1", "hello", []float32{1})
	c.Put("e2", "hello", []float32{2})
	v1, _ := c.Get("e1", "hello")
	v2, _ := c.Get("e2", "hello")
	if v1[0] != 1 || v2[0] != 2 {
		t.Fatalf("cache keys collided across embedders: %v %v", v1, v2)
	}
}

func TestEmbedCache_EvictsAtCapacity(t *testing.T) {
	c := model.NewEmbedCache(2)
	c.Put("e1", "a", []float32{1})
	c.Put("e1", "b", []float32{2})
	c.Put("e1", "c", []float32{3}) // should evict "a" (LRU)
	if _, ok := c.Get("e1", "a"); ok {
		t.Fatal("evicted entry still present")
	}
	if _, ok := c.Get("e1", "b"); !ok {
		t.Fatal("b unexpectedly evicted")
	}
	if _, ok := c.Get("e1", "c"); !ok {
		t.Fatal("c not present")
	}
	if c.Len() != 2 {
		t.Fatalf("Len = %d, want 2", c.Len())
	}
}

func TestEmbedCache_GetRefreshesLRU(t *testing.T) {
	c := model.NewEmbedCache(2)
	c.Put("e1", "a", []float32{1})
	c.Put("e1", "b", []float32{2})
	_, _ = c.Get("e1", "a")        // a is now most recent
	c.Put("e1", "c", []float32{3}) // should evict "b" not "a"
	if _, ok := c.Get("e1", "a"); !ok {
		t.Fatal("a was evicted despite recent Get")
	}
	if _, ok := c.Get("e1", "b"); ok {
		t.Fatal("b should have been evicted")
	}
}

func TestEmbedCache_PutUpdatesExisting(t *testing.T) {
	c := model.NewEmbedCache(8)
	c.Put("e1", "hello", []float32{1})
	c.Put("e1", "hello", []float32{99})
	v, _ := c.Get("e1", "hello")
	if v[0] != 99 {
		t.Fatalf("Put didn't update; v = %v", v)
	}
	if c.Len() != 1 {
		t.Fatalf("Len = %d, want 1 (update should not grow)", c.Len())
	}
}
