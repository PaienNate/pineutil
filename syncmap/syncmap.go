package syncmap

import (
	"sync"
	"sync/atomic"

	"github.com/bytedance/sonic"
)

type SyncMap[K comparable, V any] struct {
	m     sync.Map
	count int64 // 原子计数器，用于快速获取长度
}

// New 创建一个新的SyncMap实例
func New[K comparable, V any]() *SyncMap[K, V] {
	return &SyncMap[K, V]{}
}

func (m *SyncMap[K, V]) Delete(key K) {
	_, loaded := m.m.LoadAndDelete(key)
	if loaded {
		atomic.AddInt64(&m.count, -1)
	}
}

func (m *SyncMap[K, V]) Load(key K) (value V, ok bool) {
	v, ok := m.m.Load(key)
	if !ok {
		return value, ok
	}
	return v.(V), ok
}

func (m *SyncMap[K, V]) Exists(key K) bool {
	_, ok := m.m.Load(key)
	return ok
}

func (m *SyncMap[K, V]) LoadAndDelete(key K) (value V, loaded bool) {
	v, loaded := m.m.LoadAndDelete(key)
	if loaded {
		atomic.AddInt64(&m.count, -1)
		return v.(V), loaded
	}
	return value, loaded
}

func (m *SyncMap[K, V]) LoadOrStore(key K, value V) (actual V, loaded bool) {
	a, loaded := m.m.LoadOrStore(key, value)
	if !loaded {
		atomic.AddInt64(&m.count, 1)
	}
	return a.(V), loaded
}

func (m *SyncMap[K, V]) Range(f func(key K, value V) bool) {
	m.m.Range(func(key, value any) bool { return f(key.(K), value.(V)) })
}

func (m *SyncMap[K, V]) Store(key K, value V) {
	_, loaded := m.m.LoadOrStore(key, value)
	if !loaded {
		atomic.AddInt64(&m.count, 1)
	} else {
		// 如果key已存在，需要更新值
		m.m.Store(key, value)
	}
}

// Len 返回map中元素的数量，O(1)时间复杂度
func (m *SyncMap[K, V]) Len() int {
	return int(atomic.LoadInt64(&m.count))
}

// Clear 清空map中的所有元素
func (m *SyncMap[K, V]) Clear() {
	m.m.Range(func(key, value any) bool {
		m.m.Delete(key)
		return true
	})
	atomic.StoreInt64(&m.count, 0)
}

// Keys 返回所有键的切片
func (m *SyncMap[K, V]) Keys() []K {
	keys := make([]K, 0, m.Len())
	m.m.Range(func(key, value any) bool {
		keys = append(keys, key.(K))
		return true
	})
	return keys
}

// Values 返回所有值的切片
func (m *SyncMap[K, V]) Values() []V {
	values := make([]V, 0, m.Len())
	m.m.Range(func(key, value any) bool {
		values = append(values, value.(V))
		return true
	})
	return values
}

func (m *SyncMap[K, V]) MarshalJSON() ([]byte, error) {
	length := m.Len()
	m2 := make(map[K]V, length) // 预分配容量
	m.Range(func(key K, value V) bool {
		m2[key] = value
		return true
	})
	return sonic.Marshal(m2)
}

func (m *SyncMap[K, V]) UnmarshalJSON(b []byte) error {
	m2 := make(map[K]V)
	err := sonic.Unmarshal(b, &m2)
	if err != nil {
		return err
	}

	// 清空现有数据
	m.Clear()

	// 批量插入新数据
	for k, v := range m2 {
		m.m.Store(k, v)
		atomic.AddInt64(&m.count, 1)
	}
	return nil
}
