package vision

import (
	"fmt"
	"sync"
	"testing"
)

// 基本命中/未命中：Put 后同键 Get 命中且值完整；不同会话或不同图
// 各自独立（会话维度是键的一部分，不能串）。
func TestSessionCacheHitAndMiss(t *testing.T) {
	cache := NewSessionCache(SessionCacheCapacity)
	cache.Put("s1", "img-a", Evidence{Engine: "e1", Transcript: "t1"})

	if got, ok := cache.Get("s1", "img-a"); !ok || got.Transcript != "t1" || got.Engine != "e1" {
		t.Errorf("same session+image must hit with full evidence, got (%v, %v)", got, ok)
	}
	if _, ok := cache.Get("s1", "img-b"); ok {
		t.Error("different image must miss")
	}
	if _, ok := cache.Get("s2", "img-a"); ok {
		t.Error("different session must miss (session is part of the key)")
	}
}

// 空 session 永不参与缓存：没有会话维度就无法防止跨会话串图。
func TestSessionCacheEmptySessionNeverCaches(t *testing.T) {
	cache := NewSessionCache(SessionCacheCapacity)
	cache.Put("", "img-a", Evidence{Transcript: "t"})
	if _, ok := cache.Get("", "img-a"); ok {
		t.Error("empty session must never hit")
	}
	if _, ok := cache.Get("s1", "img-a"); ok {
		t.Error("empty-session put must not leak into any session")
	}
}

// LRU 驱逐：容量满时逐出最久未使用；命中会刷新新鲜度，被逐出的
// 应是更旧的那条。
func TestSessionCacheLRUEviction(t *testing.T) {
	cache := NewSessionCache(2)
	cache.Put("s", "img-a", Evidence{Transcript: "a"})
	cache.Put("s", "img-b", Evidence{Transcript: "b"})
	// 刷新 img-a：img-b 变成最久未使用。
	if _, ok := cache.Get("s", "img-a"); !ok {
		t.Fatal("refresh get must hit")
	}
	cache.Put("s", "img-c", Evidence{Transcript: "c"}) // 驱逐 img-b

	if _, ok := cache.Get("s", "img-b"); ok {
		t.Error("img-b was least recently used, must be evicted")
	}
	if _, ok := cache.Get("s", "img-a"); !ok {
		t.Error("img-a was refreshed, must survive")
	}
	if _, ok := cache.Get("s", "img-c"); !ok {
		t.Error("img-c was just inserted, must survive")
	}
}

// 同键覆盖写：Put 已存在的键更新值而非重复插入（链表长度不变）。
func TestSessionCacheOverwrite(t *testing.T) {
	cache := NewSessionCache(2)
	cache.Put("s", "img-a", Evidence{Transcript: "first"})
	cache.Put("s", "img-a", Evidence{Transcript: "second"})
	if got, ok := cache.Get("s", "img-a"); !ok || got.Transcript != "second" {
		t.Errorf("overwrite must replace value, got (%v, %v)", got, ok)
	}
	if cache.ll.Len() != 1 {
		t.Errorf("overwrite must not grow the list, len=%d", cache.ll.Len())
	}
}

// 容量 ≤0 = 禁用：Put 静默丢弃，Get 永不命中。
func TestSessionCacheDisabled(t *testing.T) {
	cache := NewSessionCache(0)
	cache.Put("s", "img-a", Evidence{Transcript: "t"})
	if _, ok := cache.Get("s", "img-a"); ok {
		t.Error("zero-capacity cache must never hit")
	}
}

// 并发安全：多 goroutine 混合 Get/Put 不崩、不脏（-race 下验证）。
func TestSessionCacheConcurrent(t *testing.T) {
	cache := NewSessionCache(16)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				key := fmt.Sprintf("img-%d", (w+i)%10)
				cache.Put("s", key, Evidence{Transcript: key})
				if got, ok := cache.Get("s", key); ok && got.Transcript != key {
					t.Errorf("dirty read: key=%s got=%s", key, got.Transcript)
				}
			}
		}(w)
	}
	wg.Wait()
}
