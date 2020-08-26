// Copyright 2020 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync/atomic"
	"time"

	redigo "github.com/opencensus-integrations/redigo/redis"
	"go.opencensus.io/stats/view"
)

var _ Cacher = (*redis)(nil)

// redis is a shared cache implementation backed by Redis. It's ideal for
// production installations since the cache is shared among all services.
type redis struct {
	pool    *redigo.Pool
	keyFunc KeyFunc

	stopped uint32
	stopCh  chan struct{}
}

type RedisConfig struct {
	// Address is the redis address and port. The default value is 127.0.0.1:6379.
	Address string

	// Username and Password are used for authentication.
	Username, Password string

	// KeyFunc is the key function.
	KeyFunc KeyFunc
}

// NewRedis creates a new in-memory cache.
func NewRedis(i *RedisConfig) (Cacher, error) {
	if i == nil {
		i = new(RedisConfig)
	}

	addr := "127.0.0.1:6379"
	if i.Address != "" {
		addr = i.Address
	}

	c := &redis{
		pool: &redigo.Pool{
			Dial: func() (redigo.Conn, error) {
				return redigo.Dial("tcp", addr,
					redigo.DialPassword(i.Password))
			},
			TestOnBorrow: func(conn redigo.Conn, _ time.Time) error {
				_, err := conn.Do("PING")
				return err
			},

			// TODO: make configurable
			MaxIdle:   0,
			MaxActive: 0,
		},
		keyFunc: i.KeyFunc,
		stopCh:  make(chan struct{}),
	}

	if err := view.Register(redigo.ObservabilityMetricViews...); err != nil {
		return nil, fmt.Errorf("redis view registration failure: %w", err)
	}

	return c, nil
}

// Fetch attempts to retrieve the given key from the cache. If successful, it
// returns the value. If the value does not exist, it calls f and caches the
// result of f in the cache for ttl. The ttl is calculated from the time the
// value is inserted, not the time the function is called.
func (c *redis) Fetch(ctx context.Context, key string, out interface{}, ttl time.Duration, f FetchFunc) error {
	if c.isStopped() {
		return ErrStopped
	}

	if c.keyFunc != nil {
		var err error
		key, err = c.keyFunc(key)
		if err != nil {
			return fmt.Errorf("failed to execute keyFunc: %w", err)
		}
	}

	fn := func(conn redigo.ConnWithContext) (io.Reader, error) {
		cached, err := redigo.String(conn.DoContext(ctx, "GET", key))
		if err != nil && !errors.Is(err, redigo.ErrNil) {
			return nil, fmt.Errorf("failed to GET key: %w", err)
		}

		// Found a value
		if cached != "" {
			return strings.NewReader(cached), nil
		}

		// No value found
		if f == nil {
			return nil, ErrMissingFetchFunc
		}
		val, err := f()
		if err != nil {
			return nil, err
		}

		// Encode the value
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(val); err != nil {
			return nil, fmt.Errorf("failed to encode value: %w", err)
		}

		if _, err := conn.DoContext(ctx, "WATCH", key); err != nil {
			return nil, fmt.Errorf("failed to WATCH key: %w", err)
		}

		if _, err := conn.DoContext(ctx, "MULTI"); err != nil {
			return nil, fmt.Errorf("failed to MULTI: %w", err)
		}

		if _, err := conn.DoContext(ctx, "PSETEX", key, int64(ttl.Milliseconds()), encoded.String()); err != nil {
			err = fmt.Errorf("failed to PSETEX: %w", err)

			if _, derr := conn.DoContext(ctx, "DISCARD"); derr != nil {
				err = fmt.Errorf("failed to DISCARD: %v, original error: %w", derr, err)
			}

			return nil, err
		}

		if _, err := conn.DoContext(ctx, "EXEC"); err != nil {
			return nil, fmt.Errorf("failed to EXEC: %w", err)
		}

		return &encoded, nil
	}

	// This is a CAS operation, so retry
	var err error
	for i := 0; i < 5; i++ {
		err = c.withConn(func(c redigo.ConnWithContext) error {
			r, err := fn(c)
			if err != nil {
				return err
			}

			// Decode the value into out.
			if err := json.NewDecoder(r).Decode(out); err != nil {
				return fmt.Errorf("failed to decode value")
			}

			return nil
		})
		if err == nil {
			break
		}
	}

	return err
}

// Write adds a new item to the cache with the given TTL.
func (c *redis) Write(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	if c.isStopped() {
		return ErrStopped
	}

	if c.keyFunc != nil {
		var err error
		key, err = c.keyFunc(key)
		if err != nil {
			return fmt.Errorf("failed to execute keyFunc: %w", err)
		}
	}

	return c.withConn(func(conn redigo.ConnWithContext) error {
		var encoded bytes.Buffer
		if err := json.NewEncoder(&encoded).Encode(value); err != nil {
			return fmt.Errorf("failed to encode value: %w", err)
		}

		if _, err := redigo.String(conn.DoContext(ctx, "PSETEX", key, int64(ttl.Milliseconds()), encoded.String())); err != nil {
			return fmt.Errorf("failed to PSETEX value: %w", err)
		}
		return nil
	})
}

// Read fetches the value at the key. If the value does not exist, it returns
// ErrNotFound.
func (c *redis) Read(ctx context.Context, key string, out interface{}) error {
	if c.isStopped() {
		return ErrStopped
	}

	if c.keyFunc != nil {
		var err error
		key, err = c.keyFunc(key)
		if err != nil {
			return fmt.Errorf("failed to execute keyFunc: %w", err)
		}
	}

	return c.withConn(func(conn redigo.ConnWithContext) error {
		val, err := redigo.String(conn.DoContext(ctx, "GET", key))
		if err != nil && !errors.Is(err, redigo.ErrNil) {
			return fmt.Errorf("failed to GET value: %w", err)
		}
		if val == "" {
			return ErrNotFound
		}

		r := strings.NewReader(val)
		if err := json.NewDecoder(r).Decode(out); err != nil {
			return fmt.Errorf("failed to decode cached value: %w", err)
		}
		return nil
	})

}

// Delete removes an item from the cache, if it exists, regardless of TTL.
func (c *redis) Delete(ctx context.Context, key string) error {
	if c.isStopped() {
		return ErrStopped
	}

	if c.keyFunc != nil {
		var err error
		key, err = c.keyFunc(key)
		if err != nil {
			return fmt.Errorf("failed to execute keyFunc: %w", err)
		}
	}

	return c.withConn(func(conn redigo.ConnWithContext) error {
		if _, err := conn.DoContext(ctx, "DEL", key); err != nil && !errors.Is(err, redigo.ErrNil) {
			return fmt.Errorf("failed to DEL: %w", err)
		}
		return nil
	})
}

// Close completely stops the cacher. It is not safe to use after closing.
func (c *redis) Close() error {
	if !atomic.CompareAndSwapUint32(&c.stopped, 0, 1) {
		return nil
	}

	close(c.stopCh)
	if err := c.pool.Close(); err != nil {
		return fmt.Errorf("failed to close pool: %w", err)
	}
	return nil
}

// withConn runs the function with a conn, ensuring cleanup of the connection.
func (c *redis) withConn(f func(conn redigo.ConnWithContext) error) error {
	if f == nil {
		return fmt.Errorf("missing function")
	}

	ctx := context.Background()

	conn, ok := c.pool.GetWithContext(ctx).(redigo.ConnWithContext)
	if !ok {
		return fmt.Errorf("redis conn is not ConnWithContext")
	}

	if err := conn.Err(); err != nil {
		return fmt.Errorf("connection is not usable: %w", err)
	}

	if err := f(conn); err != nil {
		if cerr := conn.CloseContext(ctx); cerr != nil {
			return fmt.Errorf("failed to close connection: %v, original error: %w", cerr, err)
		}
		return err
	}

	if err := conn.CloseContext(ctx); err != nil {
		return fmt.Errorf("failed to close connection: %w", err)
	}
	return nil
}

// isStopped returns true if the cacher is stopped.
func (c *redis) isStopped() bool {
	return atomic.LoadUint32(&c.stopped) == 1
}
