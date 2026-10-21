package pool

import (
	"context"
	"errors"
	"testing"
)

// The public pool is a hard boundary: a gongyi key only sees gongyi accounts,
// a personal key never sees them, and an empty public pool returns capacity
// error instead of falling back to personal accounts.
func TestGongyiPoolIsolation(t *testing.T) {
	f := newFixture(t)
	f.addAccountWith(t, "personal", 0, "oauth_token=p", 5)
	f.addAccountWith(t, "public", 0, "oauth_token=q", 5)
	if n := f.store.SetGongyiBatch(map[string]bool{"public": true}, true); n != 1 {
		t.Fatalf("SetGongyiBatch=%d want 1", n)
	}

	for i := 0; i < 10; i++ {
		lease, err := f.pool.Acquire(context.Background(), "", "")
		if err != nil {
			t.Fatalf("personal acquire: %v", err)
		}
		if lease.Account.ID != "personal" {
			t.Fatalf("personal key picked %q", lease.Account.ID)
		}
		lease.Release(nil)
	}
	for i := 0; i < 10; i++ {
		lease, err := f.pool.Acquire(context.Background(), "", "gongyi")
		if err != nil {
			t.Fatalf("gongyi acquire: %v", err)
		}
		if lease.Account.ID != "public" {
			t.Fatalf("gongyi key picked %q", lease.Account.ID)
		}
		lease.Release(nil)
	}

	// Empty the public pool: the public key must fail, never fall back.
	f.store.SetGongyiBatch(map[string]bool{}, true)
	if _, err := f.pool.Acquire(context.Background(), "", "gongyi"); !errors.Is(err, ErrNoCapacity) {
		t.Fatalf("empty gongyi pool err=%v want ErrNoCapacity", err)
	}
	lease, err := f.pool.Acquire(context.Background(), "", "")
	if err != nil || lease.Account.ID != "personal" {
		t.Fatalf("personal pool affected by gongyi clear: %v %+v", err, lease)
	}
	lease.Release(nil)
}
