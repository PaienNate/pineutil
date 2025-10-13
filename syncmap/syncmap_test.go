package syncmap_test

import (
	"encoding/json"
	"testing"

	"github.com/PaienNate/pineutil/syncmap"
)

func TestSyncMapBasicOperations(t *testing.T) {
	m := syncmap.New[string, int]()

	// 测试Store和Load
	m.Store("key1", 100)
	m.Store("key2", 200)

	if val, ok := m.Load("key1"); !ok || val != 100 {
		t.Errorf("Expected key1=100, got %v, %v", val, ok)
	}

	// 测试Len
	if m.Len() != 2 {
		t.Errorf("Expected length 2, got %d", m.Len())
	}

	// 测试Exists
	if !m.Exists("key1") {
		t.Error("Expected key1 to exist")
	}
	if m.Exists("key3") {
		t.Error("Expected key3 to not exist")
	}

	// 测试Delete
	m.Delete("key1")
	if m.Len() != 1 {
		t.Errorf("Expected length 1 after delete, got %d", m.Len())
	}
	if m.Exists("key1") {
		t.Error("Expected key1 to not exist after delete")
	}

	// 测试LoadOrStore
	actual, loaded := m.LoadOrStore("key3", 300)
	if loaded || actual != 300 {
		t.Errorf("Expected new key3=300, got %v, loaded=%v", actual, loaded)
	}
	if m.Len() != 2 {
		t.Errorf("Expected length 2 after LoadOrStore, got %d", m.Len())
	}

	// 测试LoadAndDelete
	val, loaded := m.LoadAndDelete("key2")
	if !loaded || val != 200 {
		t.Errorf("Expected key2=200, got %v, loaded=%v", val, loaded)
	}
	if m.Len() != 1 {
		t.Errorf("Expected length 1 after LoadAndDelete, got %d", m.Len())
	}
}

func TestSyncMapJSON(t *testing.T) {
	m := syncmap.New[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)
	m.Store("c", 3)

	// 测试JSON序列化
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("JSON marshal failed: %v", err)
	}

	// 测试JSON反序列化
	m2 := syncmap.New[string, int]()
	err = json.Unmarshal(data, m2)
	if err != nil {
		t.Fatalf("JSON unmarshal failed: %v", err)
	}

	if m2.Len() != 3 {
		t.Errorf("Expected length 3 after unmarshal, got %d", m2.Len())
	}

	if val, ok := m2.Load("a"); !ok || val != 1 {
		t.Errorf("Expected a=1, got %v, %v", val, ok)
	}
}

func TestSyncMapClear(t *testing.T) {
	m := syncmap.New[string, int]()
	m.Store("key1", 1)
	m.Store("key2", 2)

	if m.Len() != 2 {
		t.Errorf("Expected length 2, got %d", m.Len())
	}

	m.Clear()
	if m.Len() != 0 {
		t.Errorf("Expected length 0 after clear, got %d", m.Len())
	}
	if m.Exists("key1") {
		t.Error("Expected key1 to not exist after clear")
	}
}

func TestSyncMapKeysValues(t *testing.T) {
	m := syncmap.New[string, int]()
	m.Store("a", 1)
	m.Store("b", 2)
	m.Store("c", 3)

	keys := m.Keys()
	if len(keys) != 3 {
		t.Errorf("Expected 3 keys, got %d", len(keys))
	}

	values := m.Values()
	if len(values) != 3 {
		t.Errorf("Expected 3 values, got %d", len(values))
	}
}

// 性能基准测试
func BenchmarkSyncMapLen(b *testing.B) {
	m := syncmap.New[int, int]()
	for i := 0; i < 1000; i++ {
		m.Store(i, i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Len()
	}
}

func BenchmarkSyncMapStore(b *testing.B) {
	m := syncmap.New[int, int]()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m.Store(i, i)
	}
}

func BenchmarkSyncMapLoad(b *testing.B) {
	m := syncmap.New[int, int]()
	for i := 0; i < 1000; i++ {
		m.Store(i, i)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = m.Load(i % 1000)
	}
}
