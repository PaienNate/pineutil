package socketio

import "github.com/PaienNate/pineutil/syncmap"

// safeListeners 是一个线程安全的事件监听器集合.
type safeListeners struct {
	listeners syncmap.SyncMap[string, []eventCallback]
}

// set 方法用于向指定事件添加一个回调函数.
// 如果该事件尚不存在，则初始化一个回调列表
func (l *safeListeners) set(event string, callback eventCallback) {
	callbacks, loaded := l.listeners.LoadOrStore(event, []eventCallback{})
	if !loaded {
		// 如果事件不存在，初始化一个空的回调列表
		callbacks = []eventCallback{}
	}
	// 添加回调到该事件的回调列表
	callbacks = append(callbacks, callback)
	// 将更新后的回调列表存回 SyncMap
	l.listeners.Store(event, callbacks)
}

// get 方法用于获取指定事件的所有回调函数.
// 如果该事件没有回调，返回一个空的切片
func (l *safeListeners) get(event string) []eventCallback {
	// 加载事件的回调列表
	callbacks, ok := l.listeners.Load(event)
	if !ok {
		// 如果事件没有回调，返回一个空的切片
		return []eventCallback{}
	}
	// 返回回调列表的副本，避免外部修改
	return append([]eventCallback{}, callbacks...)
}
