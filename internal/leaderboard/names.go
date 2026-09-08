package leaderboard

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/saim61/podium/internal/platform/redis"
)

// ErrUnranked means a user has no score on the requested board.
var ErrUnranked = errors.New("user is not ranked on this board")

// Directory looks up usernames by id. Implemented against Postgres; an interface here so the
// leaderboard does not depend on the database layer.
type Directory interface {
	Usernames(ctx context.Context, ids []int64) (map[int64]string, error)
}

const nameCacheTTL = time.Hour

// nameResolver turns the user ids stored in sorted sets into usernames.
//
// Sorted sets hold ids because ids are small and stable; a page therefore needs names attached
// before it can be shown. One batched lookup per page, cached in Redis - never a query per row.
type nameResolver struct {
	client    *redis.Client
	directory Directory
}

func newNameResolver(client *redis.Client, directory Directory) *nameResolver {
	return &nameResolver{client: client, directory: directory}
}

func nameKey(userID int64) string {
	return "user:name:" + strconv.FormatInt(userID, 10)
}

func (n *nameResolver) resolve(ctx context.Context, ids []int64) (map[int64]string, error) {
	names := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return names, nil
	}

	unique := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}

	keys := make([]string, 0, len(unique))
	for _, id := range unique {
		keys = append(keys, nameKey(id))
	}

	cached, err := n.client.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, fmt.Errorf("read cached usernames: %w", err)
	}

	var missing []int64
	for i, value := range cached {
		if name, ok := value.(string); ok && name != "" {
			names[unique[i]] = name
			continue
		}
		missing = append(missing, unique[i])
	}

	if len(missing) == 0 {
		return names, nil
	}

	fetched, err := n.directory.Usernames(ctx, missing)
	if err != nil {
		return nil, fmt.Errorf("look up usernames: %w", err)
	}

	pipe := n.client.Pipeline()
	for id, name := range fetched {
		names[id] = name
		pipe.Set(ctx, nameKey(id), name, nameCacheTTL)
	}
	if _, err := pipe.Exec(ctx); err != nil && !errors.Is(err, goredis.Nil) {
		// A failed cache write is not a failed read. The names are already in hand.
		return names, nil
	}
	return names, nil
}

// Forget drops a cached username, for when one changes.
func (n *nameResolver) Forget(ctx context.Context, userID int64) error {
	return n.client.Del(ctx, nameKey(userID)).Err()
}
