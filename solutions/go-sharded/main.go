package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"go.dw1.io/rapidhash"
)

const SHARD_COUNT = 64

type Lock struct {
	Key    string    `json:"key"`
	Lockee string    `json:"lockee"`
	Since  time.Time `json:"since"`
}

type LockShard struct {
	shardKey uint64
	mu       sync.RWMutex
	locks    map[string]Lock
}

type store struct {
	shards []*LockShard
}

func newStore() *store {
	shards := make([]*LockShard, 64)
	for i := uint64(0); i < 64; i++ {
		shards[i] = &LockShard{
			shardKey: i,
			locks:    make(map[string]Lock),
		}
	}
	return &store{shards: shards}
}

func (s *store) acquire(key, lockee string, force bool) (Lock, bool) {
	data := []byte(key)
	hash := rapidhash.Hash(data)
	shard := hash & (SHARD_COUNT - 1)
	shard_info := s.shards[shard]
	shard_info.mu.Lock()
	defer shard_info.mu.Unlock()

	existing, ok := shard_info.locks[key]
	if ok {
		if existing.Lockee == lockee {
			// Idempotent re-acquire: preserve the original since timestamp.
			return existing, true
		}
		if !force {
			return existing, false
		}
		// Force: fall through to overwrite with new since.
	}

	l := Lock{Key: key, Lockee: lockee, Since: time.Now().UTC()}
	shard_info.locks[key] = l
	return l, true
}

func (s *store) release(key, lockee string) (int, string) {
	data := []byte(key)
	hash := rapidhash.Hash(data)
	shard := hash & (SHARD_COUNT - 1)
	shard_info := s.shards[shard]
	shard_info.mu.Lock()
	defer shard_info.mu.Unlock()

	existing, ok := shard_info.locks[key]
	if !ok {
		return http.StatusNotFound, "not found"
	}
	if existing.Lockee != lockee {
		return http.StatusForbidden, "lockee mismatch"
	}
	delete(shard_info.locks, key)
	return http.StatusOK, ""
}

func (s *store) list() []Lock {
	locks := make([]Lock, 0)
	for _, shard := range s.shards {
		shard.mu.RLock()
		for _, lock := range shard.locks {
			locks = append(locks, lock)
		}
		shard.mu.RUnlock()
	}
	return locks
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type lockOKResp struct {
	Locked bool   `json:"locked"`
	Key    string `json:"key"`
	Lockee string `json:"lockee"`
}

type lockConflictResp struct {
	Locked        bool   `json:"locked"`
	Key           string `json:"key"`
	CurrentLockee string `json:"currentLockee"`
}

type lockListEntry struct {
	Key    string `json:"key"`
	Lockee string `json:"lockee"`
	Since  string `json:"since"`
}

type lockListResp struct {
	Locks []lockListEntry `json:"locks"`
}

func main() {
	s := newStore()
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, struct {
			Status string `json:"status"`
		}{"ok"})
	})

	mux.HandleFunc("POST /lock", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key    string `json:"key"`
			Lockee string `json:"lockee"`
			Force  bool   `json:"force"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Key == "" || req.Lockee == "" {
			http.Error(w, "key and lockee are required", http.StatusBadRequest)
			return
		}

		l, acquired := s.acquire(req.Key, req.Lockee, req.Force)
		if acquired {
			writeJSON(w, http.StatusOK, lockOKResp{Locked: true, Key: l.Key, Lockee: l.Lockee})
		} else {
			writeJSON(w, http.StatusConflict, lockConflictResp{Locked: false, Key: req.Key, CurrentLockee: l.Lockee})
		}
	})

	mux.HandleFunc("POST /unlock", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Key    string `json:"key"`
			Lockee string `json:"lockee"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if req.Key == "" || req.Lockee == "" {
			http.Error(w, "key and lockee are required", http.StatusBadRequest)
			return
		}

		status, msg := s.release(req.Key, req.Lockee)
		if status == http.StatusOK {
			w.WriteHeader(http.StatusOK)
		} else {
			http.Error(w, msg, status)
		}
	})

	mux.HandleFunc("GET /locks", func(w http.ResponseWriter, r *http.Request) {
		locks := s.list()
		entries := make([]lockListEntry, len(locks))
		for i, l := range locks {
			entries[i] = lockListEntry{Key: l.Key, Lockee: l.Lockee, Since: l.Since.Format(time.RFC3339)}
		}
		writeJSON(w, http.StatusOK, lockListResp{Locks: entries})
	})

	http.ListenAndServe(":8080", mux)
}
