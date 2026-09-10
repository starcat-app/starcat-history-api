// Package cache 提供查询服务使用的有界内存缓存和请求合并器。
//
// History API 是公开图片入口，不能把每个 README 请求都放大成一次 GitHub
// 请求。因此缓存必须有容量上限，并且同一仓库的并发冷启动只能共享一次回源。
package cache

import (
	"container/list"
	"context"
	"sync"
	"time"
)

type entry struct {
	key       string
	value     any
	expiresAt time.Time
}

// LRU 是带 TTL 的有界 LRU。过期项只在访问时淘汰，避免为一个简单的内存缓存
// 再维护后台清理 goroutine 和额外生命周期。
type LRU struct {
	mu       sync.Mutex
	capacity int
	items    map[string]*list.Element
	order    *list.List
}

// NewLRU 创建有界缓存。无效容量会收敛为 1，防止错误配置退化成无限缓存。
func NewLRU(capacity int) *LRU {
	if capacity <= 0 {
		capacity = 1
	}
	return &LRU{capacity: capacity, items: make(map[string]*list.Element), order: list.New()}
}

// Get 读取未过期值，并把命中项移动到 LRU 队首。
func (c *LRU) Get(key string, now time.Time) (any, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[key]
	if !ok {
		return nil, false
	}
	item := element.Value.(entry)
	if !item.expiresAt.IsZero() && !now.Before(item.expiresAt) {
		delete(c.items, key)
		c.order.Remove(element)
		return nil, false
	}
	c.order.MoveToFront(element)
	return item.value, true
}

// Set 写入值并按容量淘汰最久未使用项。
func (c *LRU) Set(key string, value any, now time.Time, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	expiresAt := time.Time{}
	if ttl > 0 {
		expiresAt = now.Add(ttl)
	}
	if element, ok := c.items[key]; ok {
		element.Value = entry{key: key, value: value, expiresAt: expiresAt}
		c.order.MoveToFront(element)
		return
	}
	element := c.order.PushFront(entry{key: key, value: value, expiresAt: expiresAt})
	c.items[key] = element
	for len(c.items) > c.capacity {
		oldest := c.order.Back()
		if oldest == nil {
			break
		}
		delete(c.items, oldest.Value.(entry).key)
		c.order.Remove(oldest)
	}
}

type call struct {
	done  chan struct{}
	value any
	err   error
}

// Group 合并同一 key 的并发回源请求，避免公开 README 被同时刷新时打穿 GitHub
// rate limit。等待者仍可通过自己的 context 取消等待，不会取消正在执行的主请求。
type Group struct {
	mu    sync.Mutex
	calls map[string]*call
}

// Do 执行或等待一个 key 对应的回源操作。
func (g *Group) Do(ctx context.Context, key string, fn func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.calls == nil {
		g.calls = make(map[string]*call)
	}
	if current, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-current.done:
			return current.value, current.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	current := &call{done: make(chan struct{})}
	g.calls[key] = current
	g.mu.Unlock()

	current.value, current.err = fn()
	close(current.done)
	g.mu.Lock()
	delete(g.calls, key)
	g.mu.Unlock()
	return current.value, current.err
}
