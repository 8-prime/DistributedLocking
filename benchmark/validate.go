package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const validateKey = "__validate__"
const validateLockeeA = "lockee-a"
const validateLockeeB = "lockee-b"

type lockResponse struct {
	Locked        bool   `json:"locked"`
	Key           string `json:"key"`
	Lockee        string `json:"lockee"`
	CurrentLockee string `json:"currentLockee"`
}

type lockEntry struct {
	Key    string `json:"key"`
	Lockee string `json:"lockee"`
	Since  string `json:"since"`
}

type locksResponse struct {
	Locks []lockEntry `json:"locks"`
}

// validateSpec runs a sequence of deterministic requests against baseURL and
// returns an error if the service deviates from the spec.
func validateSpec(baseURL string) error {
	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Acquire lock as lockee-a.
	status, resp, err := postLock(client, baseURL, validateKey, validateLockeeA, false)
	if err != nil {
		return fmt.Errorf("step 1 POST /lock: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 1: expected 200, got %d", status)
	}
	if !resp.Locked {
		return fmt.Errorf("step 1: expected locked=true")
	}

	// Check GET /locks shows an entry for the key.
	since1, err := getLockSince(client, baseURL, validateKey)
	if err != nil {
		return fmt.Errorf("step 1 GET /locks: %w", err)
	}
	if since1 == "" {
		return fmt.Errorf("step 1: lock entry not found in GET /locks after acquire")
	}

	// 2. Re-acquire same key with same lockee — should succeed and since must not change.
	status, resp, err = postLock(client, baseURL, validateKey, validateLockeeA, false)
	if err != nil {
		return fmt.Errorf("step 2 POST /lock: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 2: expected 200 on re-acquire, got %d", status)
	}
	if !resp.Locked {
		return fmt.Errorf("step 2: expected locked=true on re-acquire")
	}

	since2, err := getLockSince(client, baseURL, validateKey)
	if err != nil {
		return fmt.Errorf("step 2 GET /locks: %w", err)
	}
	if since2 != since1 {
		return fmt.Errorf("step 2: locked_since changed on re-acquire (was %s, now %s)", since1, since2)
	}

	// 3. Acquire with a different lockee without force — must fail with 409.
	status, _, err = postLock(client, baseURL, validateKey, validateLockeeB, false)
	if err != nil {
		return fmt.Errorf("step 3 POST /lock: %w", err)
	}
	if status != http.StatusConflict {
		return fmt.Errorf("step 3: expected 409 when locking with different lockee, got %d", status)
	}

	// 4. Acquire with a different lockee with force=true — must succeed.
	status, resp, err = postLock(client, baseURL, validateKey, validateLockeeB, true)
	if err != nil {
		return fmt.Errorf("step 4 POST /lock (force): %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 4: expected 200 on force acquire, got %d", status)
	}
	if !resp.Locked {
		return fmt.Errorf("step 4: expected locked=true on force acquire")
	}

	// 5. Free the lock and verify no entry remains.
	status, err = deleteLock(client, baseURL, validateKey, validateLockeeB)
	if err != nil {
		return fmt.Errorf("step 5 DELETE /lock: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 5: expected 200 on release, got %d", status)
	}

	since3, err := getLockSince(client, baseURL, validateKey)
	if err != nil {
		return fmt.Errorf("step 5 GET /locks: %w", err)
	}
	if since3 != "" {
		return fmt.Errorf("step 5: lock entry still present in GET /locks after release")
	}

	return nil
}

func postLock(client *http.Client, baseURL, key, lockee string, force bool) (int, lockResponse, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"key": key, "lockee": lockee, "force": force,
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/lock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return 0, lockResponse{}, err
	}
	defer httpResp.Body.Close()
	var lr lockResponse
	json.NewDecoder(httpResp.Body).Decode(&lr)
	return httpResp.StatusCode, lr, nil
}

func deleteLock(client *http.Client, baseURL, key, lockee string) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"key": key, "lockee": lockee,
	})
	req, _ := http.NewRequest(http.MethodDelete, baseURL+"/lock", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	httpResp.Body.Close()
	return httpResp.StatusCode, nil
}

// getLockSince returns the `since` value for the given key from GET /locks,
// or "" if no entry is found.
func getLockSince(client *http.Client, baseURL, key string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/locks", nil)
	httpResp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer httpResp.Body.Close()
	var lr locksResponse
	if err := json.NewDecoder(httpResp.Body).Decode(&lr); err != nil {
		return "", err
	}
	for _, e := range lr.Locks {
		if e.Key == key {
			return e.Since, nil
		}
	}
	return "", nil
}
