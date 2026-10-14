// WorkBuddy2API —— 禁止商用倒卖（完整声明与法律边界见仓库根目录 NOTICE）
// 允许：个人学习 / 研究 / 自用部署 / 非营利分享与二次开发。
// 禁止：以本源码或其衍生版本为标的的商业转售、倒卖、付费分发。

package main

import (
	"database/sql"
	"sync"
	"time"

	"workbuddy2api/internal/redisstore"
)

// 编译期断言：本实现直接注入 session.Router 的 Store 位（类型即 redisstore.Store）。
var _ redisstore.Store = (*stickyStore)(nil)

// stickyStore 会话粘性绑定的本地持久化实现（v0.17.5）。
//
// **为什么需要它**：此前 NewProxy 注入的是 redisstore.Noop{} —— 粘性绑定纯内存，
// 进程一重启全部清零。重启后同一场对话的下一个请求会在「可用集」里重新哈希分配账号，
// 于是同一段连续对话的前后两半分别由**两个不同账号**服务（消耗的是两个账号的积分）。
// 这正是「调用过程中切换账号」的可复现来源之一，与选号策略无关，纯粹是持久化缺失。
//
// webapp 是单机部署（SQLite 就是它的持久层），没有 Redis 可依赖，所以直接把绑定落到
// sticky_binds 表。语义与既有 redisstore 完全一致：
//   - SetBind/DelBind 是 **fire-and-forget**：入小缓冲 channel，由后台 goroutine 落库，
//     主转发路径不被 SQLite 写放大（队列满即丢 —— 粘性是优化项，绝不能阻塞请求）；
//   - LoadBinds 只在启动时同步调用一次（Router.LoadFromStore），用于冷启动恢复；
//   - TTL 由 expires_at 承载，LoadBinds 侧过滤过期行（Router 内存侧另有 TTL 兜底）。
//
// SaveState/LoadState 不承担：池状态快照已有本地 state.json 通道，重复一套只会产生
// 两个真相源。
type stickyStore struct {
	db   *sql.DB
	ch   chan stickyOp
	once sync.Once
	done chan struct{}
	wg   sync.WaitGroup
}

type stickyOp struct {
	key     string
	uid     string
	expires int64
	del     bool
}

// stickyBindQueue 异步队列长度。256 足以吸收突发（每请求至多 1 次绑定变更），
// 溢出即丢弃，不影响正确性（丢了只是下次未命中重新分配，行为退回旧语义）。
const stickyBindQueue = 256

func newStickyStore(db *sql.DB) *stickyStore {
	s := &stickyStore{
		db:   db,
		ch:   make(chan stickyOp, stickyBindQueue),
		done: make(chan struct{}),
	}
	s.wg.Add(1)
	go s.loop()
	return s
}

func (s *stickyStore) loop() {
	defer s.wg.Done()
	for {
		select {
		case op := <-s.ch:
			s.apply(op)
		case <-s.done:
			// 停机语义：把已入队的最后几笔变更落完再退出，避免刚建的绑定丢失。
			for {
				select {
				case op := <-s.ch:
					s.apply(op)
				default:
					return
				}
			}
		}
	}
}

func (s *stickyStore) apply(op stickyOp) {
	if s.db == nil {
		return
	}
	if op.del {
		_, _ = s.db.Exec(`DELETE FROM sticky_binds WHERE bind_key = ?`, op.key)
		return
	}
	_, _ = s.db.Exec(
		`INSERT INTO sticky_binds (bind_key, uid, expires_at) VALUES (?,?,?)
		 ON CONFLICT(bind_key) DO UPDATE SET uid = excluded.uid, expires_at = excluded.expires_at`,
		op.key, op.uid, op.expires)
}

// SetBind 异步镜像绑定（key→uid，带 TTL）。ttl<=0 记为不过期（0）。
func (s *stickyStore) SetBind(key, uid string, ttl time.Duration) {
	if key == "" || uid == "" {
		return
	}
	exp := int64(0)
	if ttl > 0 {
		exp = time.Now().Add(ttl).Unix()
	}
	select {
	case s.ch <- stickyOp{key: key, uid: uid, expires: exp}:
	default:
	}
}

// DelBind 异步删除绑定。
func (s *stickyStore) DelBind(key string) {
	if key == "" {
		return
	}
	select {
	case s.ch <- stickyOp{key: key, del: true}:
	default:
	}
}

// LoadBinds 全量读取未过期绑定（启动时调用一次）。出错返回 nil（Router 视为无备份）。
//
// 顺带清理过期行：内存侧 GC 只删「已加载进内存」的过期项，而**进程在过期前就退出**的
// 那些行没人管（LoadBinds 过滤掉它们但不删）—— 长期运行会缓慢堆积。启动时删一次即可
// （它本来就是低频路径，不值得为它加定时任务）。
func (s *stickyStore) LoadBinds() map[string]string {
	if s.db == nil {
		return nil
	}
	now := time.Now().Unix()
	_, _ = s.db.Exec(`DELETE FROM sticky_binds WHERE expires_at <> 0 AND expires_at <= ?`, now)
	rows, err := s.db.Query(
		`SELECT bind_key, uid FROM sticky_binds WHERE expires_at = 0 OR expires_at > ?`, now)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, u string
		if rows.Scan(&k, &u) == nil && k != "" && u != "" {
			out[k] = u
		}
	}
	return out
}

// SaveState/LoadState：池状态快照走本地 state.json，本 store 不重复承担。
func (s *stickyStore) SaveState([]byte)          {}
func (s *stickyStore) LoadState() ([]byte, bool) { return nil, false }

// Close 幂等关停：排空队列后返回。
func (s *stickyStore) Close() error {
	s.once.Do(func() { close(s.done) })
	s.wg.Wait()
	return nil
}
