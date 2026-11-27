package aimode

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
)

type RedisStore struct {
	client *redis.Client
}

func NewRedisStore(client *redis.Client) *RedisStore {
	return &RedisStore{client: client}
}

const (
	redisModeKey    = "sonicos:ai:mode"
	redisOwnerKey   = "sonicos:ai:owner"
	redisUpdatedKey = "sonicos:ai:updated"
	redisGroupKey   = "sonicos:ai:group:"
)

func (s *RedisStore) GetState(ctx context.Context) (State, error) {
	enabled, err := s.client.Get(ctx, redisModeKey).Result()
	if errors.Is(err, redis.Nil) {
		return State{Enabled: false}, nil
	}
	if err != nil {
		return State{}, fmt.Errorf("get ai mode: %w", err)
	}
	owner, err := s.client.Get(ctx, redisOwnerKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return State{}, fmt.Errorf("get ai mode owner: %w", err)
	}
	updatedRaw, err := s.client.Get(ctx, redisUpdatedKey).Result()
	if err != nil && !errors.Is(err, redis.Nil) {
		return State{}, fmt.Errorf("get ai mode timestamp: %w", err)
	}
	updatedAt, _ := strconv.ParseInt(updatedRaw, 10, 64)
	return State{Enabled: enabled == "1", Owner: owner, UpdatedAt: updatedAt}, nil
}

func (s *RedisStore) SetState(ctx context.Context, enabled bool, owner string) error {
	val := "0"
	if enabled {
		val = "1"
	}
	now := time.Now().Unix()
	if err := s.client.Set(ctx, redisModeKey, val, 0).Err(); err != nil {
		return fmt.Errorf("set ai mode: %w", err)
	}
	if err := s.client.Set(ctx, redisOwnerKey, owner, 0).Err(); err != nil {
		return fmt.Errorf("set ai mode owner: %w", err)
	}
	if err := s.client.Set(ctx, redisUpdatedKey, strconv.FormatInt(now, 10), 0).Err(); err != nil {
		return fmt.Errorf("set ai mode timestamp: %w", err)
	}
	return nil
}

func (s *RedisStore) GetGroupEnabled(ctx context.Context, groupJID string) (bool, error) {
	val, err := s.client.Get(ctx, redisGroupKey+groupJID).Result()
	if errors.Is(err, redis.Nil) {

		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get ai group mode: %w", err)
	}
	return val == "1", nil
}

func (s *RedisStore) SetGroupEnabled(ctx context.Context, groupJID string, enabled bool) error {
	val := "0"
	if enabled {
		val = "1"
	}
	if err := s.client.Set(ctx, redisGroupKey+groupJID, val, 0).Err(); err != nil {
		return fmt.Errorf("set ai group mode: %w", err)
	}
	return nil
}
