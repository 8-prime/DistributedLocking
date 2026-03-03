package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

type Lock struct {
	Key    string    `json:"key"`
	Lockee string    `json:"lockee"`
	Since  time.Time `json:"since"`
}

type store struct {
	mu    sync.RWMutex
	locks map[string]Lock
}

func newStore() *store {
	return &store{locks: make(map[string]Lock)}
}

func (s *store) acquire(key, lockee string, force bool) (Lock, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if existing, ok := s.locks[key]; ok && !force {
		return existing, false
	}

	l := Lock{Key: key, Lockee: lockee, Since: time.Now().UTC()}
	s.locks[key] = l
	return l, true
}

func (s *store) release(key, lockee string) (int, string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	existing, ok := s.locks[key]
	if !ok {
		return http.StatusNotFound, "not found"
	}
	if existing.Lockee != lockee {
		return http.StatusForbidden, "lockee mismatch"
	}
	delete(s.locks, key)
	return http.StatusOK, ""
}

func (s *store) list() []Lock {
	s.mu.RLock()
	defer s.mu.RUnlock()

	locks := make([]Lock, 0, len(s.locks))
	for _, l := range s.locks {
		locks = append(locks, l)
	}
	return locks
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func main() {
	s := newStore()
	mux := http.NewServeMux()

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})

	mux.HandleFunc("/lock", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
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
				writeJSON(w, http.StatusOK, map[string]any{
					"locked": true,
					"key":    l.Key,
					"lockee": l.Lockee,
				})
			} else {
				writeJSON(w, http.StatusConflict, map[string]any{
					"locked":        false,
					"key":           req.Key,
					"currentLockee": l.Lockee,
				})
			}

		case http.MethodDelete:
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

		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})

	mux.HandleFunc("/locks", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		locks := s.list()
		type lockResp struct {
			Key    string `json:"key"`
			Lockee string `json:"lockee"`
			Since  string `json:"since"`
		}
		resp := make([]lockResp, len(locks))
		for i, l := range locks {
			resp[i] = lockResp{Key: l.Key, Lockee: l.Lockee, Since: l.Since.Format(time.RFC3339)}
		}
		writeJSON(w, http.StatusOK, map[string]any{"locks": resp})
	})

	http.ListenAndServe(":8080", mux)
}
