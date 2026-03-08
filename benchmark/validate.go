package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

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
	const key = "__validate__"
	const lockeeA = "lockee-a"
	const lockeeB = "lockee-b"

	client := &http.Client{Timeout: 5 * time.Second}

	// 1. Acquire lock as lockee-a.
	status, resp, err := sendLock(client, baseURL, key, lockeeA, false)
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
	since1, err := findLockSince(client, baseURL, key)
	if err != nil {
		return fmt.Errorf("step 1 GET /locks: %w", err)
	}

	// 2. Re-acquire same key with same lockee — should succeed and since must not change.
	status, resp, err = sendLock(client, baseURL, key, lockeeA, false)
	if err != nil {
		return fmt.Errorf("step 2 POST /lock: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 2: expected 200 on re-acquire, got %d", status)
	}
	if !resp.Locked {
		return fmt.Errorf("step 2: expected locked=true on re-acquire")
	}

	since2, err := findLockSince(client, baseURL, key)
	if err != nil {
		return fmt.Errorf("step 2 GET /locks: %w", err)
	}
	if since2 != since1 {
		return fmt.Errorf("step 2: locked_since changed on re-acquire (was %q, now %q)", since1, since2)
	}

	// 3. Acquire with a different lockee without force — must fail with 409.
	status, _, err = sendLock(client, baseURL, key, lockeeB, false)
	if err != nil {
		return fmt.Errorf("step 3 POST /lock: %w", err)
	}
	if status != http.StatusConflict {
		return fmt.Errorf("step 3: expected 409 when locking with different lockee, got %d", status)
	}

	// 4. Acquire with a different lockee with force=true — must succeed.
	status, resp, err = sendLock(client, baseURL, key, lockeeB, true)
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
	status, err = sendUnlock(client, baseURL, key, lockeeB)
	if err != nil {
		return fmt.Errorf("step 5 DELETE /lock: %w", err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("step 5: expected 200 on release, got %d", status)
	}

	_, err = findLockSince(client, baseURL, key)
	if err == nil {
		return fmt.Errorf("step 5: lock entry still present in GET /locks after release")
	}
	// findLockSince returns an error when the key is absent — that's the expected outcome here.

	return nil
}

func sendLock(client *http.Client, baseURL, key, lockee string, force bool) (int, lockResponse, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"key": key, "lockee": lockee, "force": force,
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/lock/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return 0, lockResponse{}, err
	}
	defer httpResp.Body.Close()

	rawBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return httpResp.StatusCode, lockResponse{}, fmt.Errorf("read body: %w", err)
	}

	var lr lockResponse
	if err := json.Unmarshal(rawBody, &lr); err != nil {
		return httpResp.StatusCode, lockResponse{}, fmt.Errorf("decode POST /lock response (status %d, body %q): %w", httpResp.StatusCode, rawBody, err)
	}
	return httpResp.StatusCode, lr, nil
}

func sendUnlock(client *http.Client, baseURL, key, lockee string) (int, error) {
	body, _ := json.Marshal(map[string]interface{}{
		"key": key, "lockee": lockee,
	})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/unlock/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	httpResp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	httpResp.Body.Close()
	return httpResp.StatusCode, nil
}

// findLockSince returns the `since` value for the given key from GET /locks.
// Returns ("", nil) if the entry is not present, or an error that includes
// the raw response body when decoding fails or the key is simply absent.
func findLockSince(client *http.Client, baseURL, key string) (string, error) {
	req, _ := http.NewRequest(http.MethodGet, baseURL+"/locks/", nil)
	httpResp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer httpResp.Body.Close()

	rawBody, err := io.ReadAll(httpResp.Body)
	if err != nil {
		return "", fmt.Errorf("read body: %w", err)
	}

	var lr locksResponse
	if err := json.Unmarshal(rawBody, &lr); err != nil {
		return "", fmt.Errorf("decode GET /locks (status %d, body %q): %w", httpResp.StatusCode, rawBody, err)
	}

	for _, e := range lr.Locks {
		if e.Key == key {
			return e.Since, nil
		}
	}

	// Key not found — include the full body so the caller can surface it.
	return "", fmt.Errorf("key %q not in GET /locks response (status %d, body %q)", key, httpResp.StatusCode, rawBody)
}
