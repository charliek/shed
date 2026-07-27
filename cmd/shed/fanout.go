package main

import (
	"sort"
	"sync"

	"github.com/charliek/shed/internal/config"
)

// ServerResult is one entry returned by forEachServer: either Value is set
// (success) or Err is set (failure). ServerName is always populated so the
// caller can render per-server output even when the call failed.
type ServerResult[T any] struct {
	ServerName string
	Entry      config.ServerEntry
	Value      T
	Err        error
}

// forEachServer invokes fn concurrently for each named server and returns
// results in deterministic (alphabetical) order. It never aborts early:
// per-server errors are captured in the corresponding ServerResult.Err so
// the caller can partially succeed and report what failed.
//
// This is the reusable primitive behind `shed system df --all` and
// `shed system prune --all`.
func forEachServer[T any](servers map[string]config.ServerEntry, fn func(name string, entry config.ServerEntry) (T, error)) []ServerResult[T] {
	names := make([]string, 0, len(servers))
	for name := range servers {
		names = append(names, name)
	}
	sort.Strings(names)

	// Snapshot every entry from the shared map BEFORE spawning any goroutine.
	// fn may mutate the source map — NewAPIClientFromNamedEntry persists a refreshed
	// control token back into clientConfig.Servers — so reading servers[name] in
	// the spawn loop would race those writes (a fatal concurrent map read+write).
	results := make([]ServerResult[T], len(names))
	entries := make([]config.ServerEntry, len(names))
	for i, name := range names {
		results[i].ServerName = name
		entries[i] = servers[name]
		results[i].Entry = entries[i]
	}
	var wg sync.WaitGroup
	for i, name := range names {
		wg.Add(1)
		go func(i int, name string, entry config.ServerEntry) {
			defer wg.Done()
			value, err := fn(name, entry)
			results[i].Value = value
			results[i].Err = err
		}(i, name, entries[i])
	}
	wg.Wait()
	return results
}
