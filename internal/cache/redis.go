package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

const popularKey = "popular"

func linkKey(code string) string { return "link:" + code }
func hitsKey(code string) string { return "hits:" + code }

// RedisCache — реализация LinkCache поверх Redis.
type RedisCache struct {
	c *redis.Client
}

var _ LinkCache = (*RedisCache)(nil)

// NewRedis подключается по URL (redis://...) и проверяет соединение через Ping.
func NewRedis(ctx context.Context, url string) (*RedisCache, error) {
	opt, err := redis.ParseURL(url)
	if err != nil {
		return nil, fmt.Errorf("cache: разбор redis url: %w", err)
	}
	c := redis.NewClient(opt)
	if err := c.Ping(ctx).Err(); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("cache: ping redis: %w", err)
	}
	return &RedisCache{c: c}, nil
}

// NewRedisFromClient оборачивает готовый клиент (для тестов).
func NewRedisFromClient(c *redis.Client) *RedisCache { return &RedisCache{c: c} }

// Get читает JSON-запись по ключу link:{code}.
func (r *RedisCache) Get(ctx context.Context, code string) (*Entry, error) {
	b, err := r.c.Get(ctx, linkKey(code)).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, ErrMiss
	}
	if err != nil {
		return nil, err
	}
	var e Entry
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("cache: разбор записи %q: %w", code, err)
	}
	return &e, nil
}

// Set записывает запись с TTL.
func (r *RedisCache) Set(ctx context.Context, code string, e Entry, ttl time.Duration) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return r.c.Set(ctx, linkKey(code), b, ttl).Err()
}

// Delete удаляет запись.
func (r *RedisCache) Delete(ctx context.Context, code string) error {
	return r.c.Del(ctx, linkKey(code)).Err()
}

// Hit: INCR hits:{code} и ZINCRBY popular одной транзакцией; при первом
// обращении (результат INCR == 1) выставляется EXPIRE окна.
func (r *RedisCache) Hit(ctx context.Context, code string, window time.Duration) (int64, error) {
	var incr *redis.IntCmd
	_, err := r.c.TxPipelined(ctx, func(p redis.Pipeliner) error {
		incr = p.Incr(ctx, hitsKey(code))
		p.ZIncrBy(ctx, popularKey, 1, code)
		return nil
	})
	if err != nil {
		return 0, err
	}
	n := incr.Val()
	if n == 1 && window > 0 {
		if err := r.c.Expire(ctx, hitsKey(code), window).Err(); err != nil {
			return n, err
		}
	}
	return n, nil
}

// TopCodes возвращает до n самых популярных кодов.
func (r *RedisCache) TopCodes(ctx context.Context, n int) ([]string, error) {
	if n <= 0 {
		return nil, nil
	}
	return r.c.ZRangeArgs(ctx, redis.ZRangeArgs{
		Key: popularKey, Start: 0, Stop: int64(n - 1), Rev: true,
	}).Result()
}

// Ping проверяет соединение.
func (r *RedisCache) Ping(ctx context.Context) error { return r.c.Ping(ctx).Err() }

// Close закрывает клиент.
func (r *RedisCache) Close() error { return r.c.Close() }
