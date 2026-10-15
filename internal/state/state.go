// Package state 集中管理进程级可变状态：唯一的 Redis 客户端。
// 与旧实现 src/state.ts 对 Redis 的用法一致（会话 / 终端票据 / 限流 / 采样锁）。
package state

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

// Client 包装 go-redis 客户端，暴露本项目用到的命令子集。
type Client struct {
	rdb *redis.Client
}

// New 建连并 Ping。连接失败返回错误（对齐旧实现启动即报错退出）。
func New(ctx context.Context, url string) (*Client, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("Redis 地址解析失败: %w", err)
	}
	rdb := redis.NewClient(opt)
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("Redis 连接失败: %w", err)
	}
	return &Client{rdb: rdb}, nil
}

// Close 关闭连接。
func (c *Client) Close() error { return c.rdb.Close() }

// Get 读取键。
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	v, err := c.rdb.Get(ctx, key).Result()
	if err == redis.Nil {
		return "", nil
	}
	return v, err
}

// Set 设置键；ttlSeconds > 0 时带过期。
func (c *Client) Set(ctx context.Context, key, value string, ttlSeconds int) error {
	if ttlSeconds > 0 {
		return c.rdb.Set(ctx, key, value, time.Duration(ttlSeconds)*time.Second).Err()
	}
	return c.rdb.Set(ctx, key, value, 0).Err()
}

// Expire 刷新键过期时间（会话滑动续期）。
func (c *Client) Expire(ctx context.Context, key string, ttlSeconds int) error {
	return c.rdb.Expire(ctx, key, time.Duration(ttlSeconds)*time.Second).Err()
}

// Del 删除一个或多个键。
func (c *Client) Del(ctx context.Context, keys ...string) error {
	if len(keys) == 0 {
		return nil
	}
	return c.rdb.Del(ctx, keys...).Err()
}

// Keys 按模式列举键（仅用于管理面小集合：会话、接口令牌）。
func (c *Client) Keys(ctx context.Context, pattern string) ([]string, error) {
	return c.rdb.Keys(ctx, pattern).Result()
}

// SetNX 仅在键不存在时设置，返回是否成功设置。
func (c *Client) SetNX(ctx context.Context, key string, ttlSeconds int) (bool, error) {
	return c.rdb.SetNX(ctx, key, "1", time.Duration(ttlSeconds)*time.Second).Result()
}
