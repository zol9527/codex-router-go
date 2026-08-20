package vision

// 会话级转写缓存：Codex 每轮重发完整会话历史（含历史图片），同一
// 会话同一张图只在第一次真正调读图引擎，之后按 (session, ImageKey)
// 复用 Evidence —— 省下重复的读图配额与每轮 5-30s 的读图延迟。
//
// 键不含 Question：CollectImages 给所有图片（含历史轮次的）标注的
// 都是最新提问，第二轮起必然变化，Question 进键则缓存永不命中；
// Evidence.Question 保留首次转写时的值即可。
//
// 只存进程内存、不落盘：转录是图片内容的衍生文本，落盘有隐私顾虑；
// 进程重启自然清空，等价于"读图能力随服务重启归零"。失败不缓存 ——
// 下轮可重试（fail-closed 语义不变）。
//
// session 为空的客户端（没有 X-Codex-Turn-Metadata 头）不参与缓存：
// 没有会话维度就无法防止跨会话串图，宁可不省这次调用。

import (
	"container/list"
	"sync"
)

// SessionCacheCapacity 是缓存条数上限。单条转录受 EvidenceMaxChars
// （24KB）约束，峰值 ~3MB，按条数计量足够，无需字节水位。
const SessionCacheCapacity = 128

// sessionKey 唯一定位一份转写：会话名 + 图片字节摘要。
type sessionKey struct {
	session string
	image   string
}

// sessionEntry 是 LRU 链表节点持有的键值对。
type sessionEntry struct {
	key      sessionKey
	evidence Evidence
}

// SessionCache 是线程安全的 LRU 转写缓存（多请求并发读写，命中即
// 挪到队首）。经 NewSessionCache 构造；容量 ≤0 视为禁用。
type SessionCache struct {
	mu    sync.Mutex
	cap   int
	ll    *list.List // front = 最近使用；element.Value 为 sessionEntry
	items map[sessionKey]*list.Element
}

// NewSessionCache 构造指定容量的缓存；capacity ≤ 0 得到一个永久
// 空缓存（Get 永不命中、Put 永不存储）。
func NewSessionCache(capacity int) *SessionCache {
	if capacity < 0 {
		capacity = 0
	}
	return &SessionCache{
		cap:   capacity,
		ll:    list.New(),
		items: make(map[sessionKey]*list.Element, capacity),
	}
}

// Get 返回缓存的转写并标记最近使用。nil 接收者与空 session 一律
// miss（防御式：调用方无需再判空）。
func (c *SessionCache) Get(session, imageKey string) (Evidence, bool) {
	if c == nil || session == "" {
		return Evidence{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.items[sessionKey{session, imageKey}]
	if !ok {
		return Evidence{}, false
	}
	c.ll.MoveToFront(element)
	return element.Value.(sessionEntry).evidence, true
}

// Put 写入转写；容量已满时驱逐最久未使用的条目。nil 接收者与空
// session 一律丢弃（与 Get 对称）。
func (c *SessionCache) Put(session, imageKey string, evidence Evidence) {
	if c == nil || session == "" || c.cap == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	key := sessionKey{session, imageKey}
	if element, ok := c.items[key]; ok {
		element.Value = sessionEntry{key, evidence}
		c.ll.MoveToFront(element)
		return
	}
	if c.ll.Len() >= c.cap {
		if oldest := c.ll.Back(); oldest != nil {
			c.ll.Remove(oldest)
			delete(c.items, oldest.Value.(sessionEntry).key)
		}
	}
	c.items[key] = c.ll.PushFront(sessionEntry{key, evidence})
}
