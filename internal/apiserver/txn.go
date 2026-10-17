package apiserver

import (
	"context"
	"errors"
	"slices"
	"sync"

	"github.com/YukinaMochizuki/hangar/internal/auth"
	"github.com/YukinaMochizuki/hangar/internal/store"
)

// txKey carries, in a context, the store transaction a call runs in.
type txKey struct{}

// txStore is the apiserver's store: a call whose context carries a
// transaction (Server.atomically) reads and writes through it, so the generic
// machinery works inside one unchanged; any other call goes to the store.
type txStore struct{ store.Store }

func txOf(ctx context.Context) store.Tx {
	tx, _ := ctx.Value(txKey{}).(store.Tx)
	return tx
}

func (s txStore) Get(ctx context.Context, key store.Key) (store.Object, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Get(ctx, key)
	}
	return s.Store.Get(ctx, key)
}

func (s txStore) List(ctx context.Context, opts store.ListOptions) ([]store.Object, int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.List(ctx, opts)
	}
	return s.Store.List(ctx, opts)
}

func (s txStore) Create(ctx context.Context, key store.Key, data []byte) (int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Create(ctx, key, data)
	}
	return s.Store.Create(ctx, key, data)
}

func (s txStore) Update(ctx context.Context, key store.Key, data []byte, expected int64) (int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Update(ctx, key, data, expected)
	}
	return s.Store.Update(ctx, key, data, expected)
}

func (s txStore) Delete(ctx context.Context, key store.Key, expected int64) (store.Object, int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Delete(ctx, key, expected)
	}
	return s.Store.Delete(ctx, key, expected)
}

func (s txStore) Revision(ctx context.Context) (int64, error) {
	if tx := txOf(ctx); tx != nil {
		return tx.Revision(ctx)
	}
	return s.Store.Revision(ctx)
}

// Txn inside a transaction is part of it.
func (s txStore) Txn(ctx context.Context, fn func(tx store.Tx) error) error {
	if tx := txOf(ctx); tx != nil {
		return fn(tx)
	}
	return s.Store.Txn(ctx, fn)
}

// atomically runs fn in one store transaction, which every other write waits
// for: fn writes only as the system, which waits for neither the authorization
// view nor the quota lock, and the caller holds its writes' locks (holding).
// A context acting as anyone else is refused before the transaction starts.
func (s *Server) atomically(ctx context.Context, fn func(ctx context.Context) error) error {
	if p, ok := auth.FromContext(ctx); !ok || p.Type != auth.System {
		return errors.New("a store transaction runs only as the system, whose writes wait for nothing the transaction holds")
	}
	return s.store.Txn(ctx, func(tx store.Tx) error {
		return fn(context.WithValue(ctx, txKey{}, tx))
	})
}

// heldKey carries, in a context, the write locks its caller already holds.
type heldKey struct{}

// holding returns ctx recording that its caller holds mus, which writes made
// with it then do not take again.
func holding(ctx context.Context, mus ...*sync.Mutex) context.Context {
	held, _ := ctx.Value(heldKey{}).([]*sync.Mutex)
	return context.WithValue(ctx, heldKey{}, append(slices.Clone(held), mus...))
}

// lockUnlessHeld takes mu, unless ctx says its caller holds it already, and
// returns what releases what it took.
func lockUnlessHeld(ctx context.Context, mu *sync.Mutex) (unlock func()) {
	if held, _ := ctx.Value(heldKey{}).([]*sync.Mutex); slices.Contains(held, mu) {
		return func() {}
	}
	mu.Lock()
	return mu.Unlock
}
