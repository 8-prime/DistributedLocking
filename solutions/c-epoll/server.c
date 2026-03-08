/*
 * High-performance distributed lock server in C.
 * Uses epoll + SO_REUSEPORT + sharded spinlocks.
 * Compiled with clang -O3 for maximum throughput.
 */

#define _GNU_SOURCE
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>
#include <stdbool.h>
#include <unistd.h>
#include <errno.h>
#include <time.h>
#include <signal.h>
#include <pthread.h>
#include <sys/socket.h>
#include <sys/epoll.h>
#include <netinet/in.h>
#include <netinet/tcp.h>

/* ── Configuration ─────────────────────────────────────────────── */

#define PORT            8080
#define NUM_SHARDS      256
#define SHARD_MASK      (NUM_SHARDS - 1)
#define READ_BUF_SIZE   4096
#define WRITE_BUF_SIZE  8192
#define MAX_EVENTS      256
#define MAX_KEY_LEN     128
#define MAX_LOCKEE_LEN  64
#define HT_INITIAL_CAP  32

#define likely(x)   __builtin_expect(!!(x), 1)
#define unlikely(x) __builtin_expect(!!(x), 0)

/* Safe string-literal memcpy: copies str (excluding NUL) to dst, advances dst */
#define SCPY(dst, str) do { \
    memcpy(dst, str, sizeof(str) - 1); \
    dst += sizeof(str) - 1; \
} while (0)

/* ── Wyhash ────────────────────────────────────────────────────── */

static inline uint64_t _wymix(uint64_t a, uint64_t b) {
    __uint128_t r = (__uint128_t)a * b;
    a = (uint64_t)r;
    b = (uint64_t)(r >> 64);
    return a ^ b;
}

static inline uint64_t _wyr8(const uint8_t *p) {
    uint64_t v; memcpy(&v, p, 8); return v;
}
static inline uint64_t _wyr4(const uint8_t *p) {
    uint32_t v; memcpy(&v, p, 4); return v;
}
static inline uint64_t _wyr3(const uint8_t *p, size_t k) {
    return ((uint64_t)p[0] << 16) | ((uint64_t)p[k >> 1] << 8) | p[k - 1];
}

static uint64_t wyhash(const void *key, size_t len, uint64_t seed) {
    static const uint64_t s0 = 0xa0761d6478bd642fULL;
    static const uint64_t s1 = 0xe7037ed1a0b428dbULL;
    static const uint64_t s2 = 0x8ebc6af09c88c6e3ULL;
    static const uint64_t s3 = 0x589965cc75374cc3ULL;

    const uint8_t *p = (const uint8_t *)key;
    seed ^= _wymix(seed ^ s0, s1);
    uint64_t a, b;

    if (likely(len <= 16)) {
        if (likely(len >= 4)) {
            a = (_wyr4(p) << 32) | _wyr4(p + ((len >> 3) << 2));
            b = (_wyr4(p + len - 4) << 32) | _wyr4(p + len - 4 - ((len >> 3) << 2));
        } else if (likely(len > 0)) {
            a = _wyr3(p, len);
            b = 0;
        } else {
            a = b = 0;
        }
    } else if (likely(len <= 48)) {
        seed = _wymix(_wyr8(p) ^ s1, _wyr8(p + 8) ^ seed);
        if (len > 32)
            seed = _wymix(_wyr8(p + 16) ^ s2, _wyr8(p + 24) ^ seed);
        a = _wyr8(p + len - 16);
        b = _wyr8(p + len - 8);
    } else {
        size_t i = len;
        if (i > 48) {
            uint64_t see1 = seed, see2 = seed;
            do {
                seed = _wymix(_wyr8(p) ^ s1, _wyr8(p + 8) ^ seed);
                see1 = _wymix(_wyr8(p + 16) ^ s2, _wyr8(p + 24) ^ see1);
                see2 = _wymix(_wyr8(p + 32) ^ s3, _wyr8(p + 40) ^ see2);
                p += 48; i -= 48;
            } while (likely(i > 48));
            seed ^= see1 ^ see2;
        }
        while (i > 16) {
            seed = _wymix(_wyr8(p) ^ s1, _wyr8(p + 8) ^ seed);
            i -= 16; p += 16;
        }
        a = _wyr8(p + i - 16);
        b = _wyr8(p + i - 8);
    }

    return _wymix(s1 ^ len, _wymix(a ^ s1, b ^ seed));
}

/* ── Lock Entry ────────────────────────────────────────────────── */

typedef struct {
    char     key[MAX_KEY_LEN];
    char     lockee[MAX_LOCKEE_LEN];
    uint16_t key_len;
    uint16_t lockee_len;
    int64_t  since_sec;
    uint64_t hash;
    bool     occupied;
} lock_entry_t;

/* ── Hash Table (open addressing, linear probing, per-shard) ──── */

typedef struct {
    lock_entry_t *entries;
    uint32_t      cap;
    uint32_t      count;
} htable_t;

static void ht_init(htable_t *ht) {
    ht->cap = HT_INITIAL_CAP;
    ht->count = 0;
    ht->entries = (lock_entry_t *)calloc(ht->cap, sizeof(lock_entry_t));
}

static lock_entry_t *ht_find(htable_t *ht, const char *key, uint16_t key_len, uint64_t h) {
    uint32_t mask = ht->cap - 1;
    uint32_t idx = (uint32_t)h & mask;
    for (;;) {
        lock_entry_t *e = &ht->entries[idx];
        if (!e->occupied) return NULL;
        if (e->hash == h && e->key_len == key_len && memcmp(e->key, key, key_len) == 0)
            return e;
        idx = (idx + 1) & mask;
    }
}

static void ht_insert_entry(htable_t *ht, const lock_entry_t *src);

static void ht_grow(htable_t *ht) {
    uint32_t old_cap = ht->cap;
    lock_entry_t *old = ht->entries;
    ht->cap = old_cap * 2;
    ht->count = 0;
    ht->entries = (lock_entry_t *)calloc(ht->cap, sizeof(lock_entry_t));
    for (uint32_t i = 0; i < old_cap; i++) {
        if (old[i].occupied) ht_insert_entry(ht, &old[i]);
    }
    free(old);
}

static void ht_insert_entry(htable_t *ht, const lock_entry_t *src) {
    uint32_t mask = ht->cap - 1;
    uint32_t idx = (uint32_t)src->hash & mask;
    for (;;) {
        lock_entry_t *e = &ht->entries[idx];
        if (!e->occupied) {
            *e = *src;
            ht->count++;
            return;
        }
        idx = (idx + 1) & mask;
    }
}

static lock_entry_t *ht_insert(htable_t *ht, const char *key, uint16_t key_len,
                                const char *lockee, uint16_t lockee_len,
                                uint64_t h, int64_t since_sec) {
    if (unlikely(ht->count * 4 >= ht->cap * 3)) ht_grow(ht); /* 75% load */
    uint32_t mask = ht->cap - 1;
    uint32_t idx = (uint32_t)h & mask;
    for (;;) {
        lock_entry_t *e = &ht->entries[idx];
        if (!e->occupied) {
            memcpy(e->key, key, key_len);
            e->key_len = key_len;
            memcpy(e->lockee, lockee, lockee_len);
            e->lockee_len = lockee_len;
            e->hash = h;
            e->since_sec = since_sec;
            e->occupied = true;
            ht->count++;
            return e;
        }
        idx = (idx + 1) & mask;
    }
}

static void ht_remove(htable_t *ht, const char *key, uint16_t key_len, uint64_t h) {
    uint32_t mask = ht->cap - 1;
    uint32_t idx = (uint32_t)h & mask;
    for (;;) {
        lock_entry_t *e = &ht->entries[idx];
        if (!e->occupied) return;
        if (e->hash == h && e->key_len == key_len && memcmp(e->key, key, key_len) == 0) {
            e->occupied = false;
            /* Backward-shift deletion to maintain linear probing invariant */
            uint32_t j = (idx + 1) & mask;
            while (ht->entries[j].occupied) {
                uint32_t natural = (uint32_t)ht->entries[j].hash & mask;
                bool displaced;
                if (j >= idx)
                    displaced = (natural <= idx || natural > j);
                else
                    displaced = (natural <= idx && natural > j);
                if (displaced) {
                    ht->entries[idx] = ht->entries[j];
                    ht->entries[j].occupied = false;
                    idx = j;
                }
                j = (j + 1) & mask;
            }
            ht->count--;
            return;
        }
        idx = (idx + 1) & mask;
    }
}

/* ── Shard ─────────────────────────────────────────────────────── */

typedef struct {
    pthread_spinlock_t lock;
    htable_t           table;
} __attribute__((aligned(64))) shard_t;

static shard_t g_shards[NUM_SHARDS];

static void shards_init(void) {
    for (int i = 0; i < NUM_SHARDS; i++) {
        pthread_spin_init(&g_shards[i].lock, PTHREAD_PROCESS_PRIVATE);
        ht_init(&g_shards[i].table);
    }
}

/* ── RFC 3339 Timestamp ────────────────────────────────────────── */

static const int DAYS_IN_MONTH[] = {31,28,31,30,31,30,31,31,30,31,30,31};

static inline bool is_leap(int y) {
    return (y % 4 == 0 && y % 100 != 0) || (y % 400 == 0);
}

/* Writes "YYYY-MM-DDThh:mm:ssZ" (20 chars) into buf. Returns 20. */
static int fmt_rfc3339(int64_t epoch_sec, char *buf) {
    int64_t t = epoch_sec;
    int sec = (int)(t % 60); t /= 60;
    int min = (int)(t % 60); t /= 60;
    int hr  = (int)(t % 24); t /= 24;

    int days = (int)t;
    int y = 1970;
    for (;;) {
        int yd = is_leap(y) ? 366 : 365;
        if (days < yd) break;
        days -= yd;
        y++;
    }

    int m = 0;
    for (; m < 12; m++) {
        int md = DAYS_IN_MONTH[m];
        if (m == 1 && is_leap(y)) md = 29;
        if (days < md) break;
        days -= md;
    }
    int d = days + 1;
    m += 1;

    buf[0]  = '0' + (y / 1000);
    buf[1]  = '0' + (y / 100 % 10);
    buf[2]  = '0' + (y / 10 % 10);
    buf[3]  = '0' + (y % 10);
    buf[4]  = '-';
    buf[5]  = '0' + (m / 10);
    buf[6]  = '0' + (m % 10);
    buf[7]  = '-';
    buf[8]  = '0' + (d / 10);
    buf[9]  = '0' + (d % 10);
    buf[10] = 'T';
    buf[11] = '0' + (hr / 10);
    buf[12] = '0' + (hr % 10);
    buf[13] = ':';
    buf[14] = '0' + (min / 10);
    buf[15] = '0' + (min % 10);
    buf[16] = ':';
    buf[17] = '0' + (sec / 10);
    buf[18] = '0' + (sec % 10);
    buf[19] = 'Z';
    buf[20] = '\0';
    return 20;
}

/* ── Fast integer to string ────────────────────────────────────── */

static int uint_to_str(unsigned val, char *buf) {
    char tmp[12];
    int n = 0;
    if (val == 0) { buf[0] = '0'; return 1; }
    while (val > 0) { tmp[n++] = '0' + (val % 10); val /= 10; }
    for (int i = 0; i < n; i++) buf[i] = tmp[n - 1 - i];
    return n;
}

/* ── HTTP Request Parsing ──────────────────────────────────────── */

typedef enum { METHOD_GET, METHOD_POST, METHOD_DELETE, METHOD_UNKNOWN } http_method_t;
typedef enum { PATH_LOCK, PATH_LOCKS, PATH_HEALTHZ, PATH_UNKNOWN } http_path_t;

typedef struct {
    http_method_t method;
    http_path_t   path;
    const char   *body;
    int           body_len;
    int           total_len;
} http_request_t;

/* Returns: >0 = complete request (total bytes consumed), 0 = need more data, -1 = error */
static int parse_request(const char *buf, int len, http_request_t *req) {
    if (unlikely(len < 16)) return 0;

    switch (buf[0]) {
    case 'G': req->method = METHOD_GET; break;
    case 'P': req->method = METHOD_POST; break;
    case 'D': req->method = METHOD_DELETE; break;
    default:  return -1;
    }

    /* Find path between first two spaces */
    const char *space1 = (const char *)memchr(buf, ' ', len);
    if (unlikely(!space1)) return 0;
    const char *path_start = space1 + 1;
    int remaining = len - (int)(path_start - buf);
    const char *space2 = (const char *)memchr(path_start, ' ', remaining);
    if (unlikely(!space2)) return 0;
    int path_len = (int)(space2 - path_start);

    /* Match path — check /locks before /lock (longer prefix first) */
    if (path_len >= 8 && memcmp(path_start, "/healthz", 8) == 0) {
        req->path = PATH_HEALTHZ;
    } else if (path_len >= 6 && memcmp(path_start, "/locks", 6) == 0 &&
               (path_len == 6 || (path_len == 7 && path_start[6] == '/'))) {
        req->path = PATH_LOCKS;
    } else if (path_len >= 5 && memcmp(path_start, "/lock", 5) == 0 &&
               (path_len == 5 || (path_len == 6 && path_start[5] == '/'))) {
        req->path = PATH_LOCK;
    } else {
        req->path = PATH_UNKNOWN;
    }

    /* Find \r\n\r\n (end of headers) */
    const char *hdr_end = NULL;
    for (int i = 0; i <= len - 4; i++) {
        if (buf[i] == '\r' && buf[i+1] == '\n' && buf[i+2] == '\r' && buf[i+3] == '\n') {
            hdr_end = buf + i;
            break;
        }
    }
    if (unlikely(!hdr_end)) return 0;

    int header_size = (int)(hdr_end - buf) + 4;

    /* Parse Content-Length for POST/DELETE */
    int content_length = 0;
    if (req->method == METHOD_POST || req->method == METHOD_DELETE) {
        /* hdr_end points to the first \r of \r\n\r\n, so the last header
           line's \n is at hdr_end+1. Scan through hdr_end+2 to include it. */
        const char *scan_end = hdr_end + 2;
        const char *cl = buf;
        while (cl < scan_end) {
            const char *line = cl;
            const char *nl = (const char *)memchr(cl, '\n', scan_end - cl);
            if (!nl) break;
            int line_len = (int)(nl - line);
            cl = nl + 1;
            if (line_len > 16 && (memcmp(line, "Content-Length: ", 16) == 0 ||
                                   memcmp(line, "content-length: ", 16) == 0)) {
                for (const char *p = line + 16; p < line + line_len && *p >= '0' && *p <= '9'; p++)
                    content_length = content_length * 10 + (*p - '0');
                break;
            }
        }
    }

    if (len < header_size + content_length) return 0;

    req->body = buf + header_size;
    req->body_len = content_length;
    req->total_len = header_size + content_length;
    return req->total_len;
}

/* ── JSON Parser (zero-copy into read buffer) ──────────────────── */

typedef struct {
    const char *key;
    int         key_len;
    const char *lockee;
    int         lockee_len;
    bool        force;
} json_lock_req_t;

static bool parse_json_body(const char *body, int len, json_lock_req_t *out) {
    out->key = NULL; out->key_len = 0;
    out->lockee = NULL; out->lockee_len = 0;
    out->force = false;

    const char *end = body + len;
    const char *p = body;

    while (p < end - 4) {
        if (*p == '"') {
            p++;
            if (p + 5 <= end && memcmp(p, "key\":", 5) == 0) {
                p += 5;
                while (p < end && *p != '"') p++;
                if (p >= end) return false;
                p++; /* skip opening quote */
                out->key = p;
                while (p < end && *p != '"') p++;
                out->key_len = (int)(p - out->key);
                if (p < end) p++;
            } else if (p + 8 <= end && memcmp(p, "lockee\":", 8) == 0) {
                p += 8;
                while (p < end && *p != '"') p++;
                if (p >= end) return false;
                p++;
                out->lockee = p;
                while (p < end && *p != '"') p++;
                out->lockee_len = (int)(p - out->lockee);
                if (p < end) p++;
            } else if (p + 7 <= end && memcmp(p, "force\":", 7) == 0) {
                p += 7;
                while (p < end && (*p == ' ' || *p == '\t')) p++;
                out->force = (p < end && *p == 't');
                while (p < end && *p != ',' && *p != '}') p++;
            } else {
                while (p < end && *p != '"') p++;
                if (p < end) p++;
                while (p < end && *p != ',' && *p != '}') p++;
            }
        } else {
            p++;
        }
    }

    return out->key != NULL && out->key_len > 0;
}

/* ── Connection ────────────────────────────────────────────────── */

typedef struct {
    int      fd;
    uint16_t read_pos;
    uint16_t write_pos;
    uint16_t write_len;
    char     read_buf[READ_BUF_SIZE];
    char     write_buf[WRITE_BUF_SIZE];
} conn_t;

/* ── Response Building ─────────────────────────────────────────── */

static const char RESP_HEALTHZ[] =
    "HTTP/1.1 200 OK\r\n"
    "Content-Type: application/json\r\n"
    "Content-Length: 15\r\n"
    "Connection: keep-alive\r\n"
    "\r\n"
    "{\"status\":\"ok\"}";
#define RESP_HEALTHZ_LEN (sizeof(RESP_HEALTHZ) - 1)

static const char HTTP_200[] = "HTTP/1.1 200 OK\r\n";
static const char HTTP_400[] = "HTTP/1.1 400 Bad Request\r\n";
static const char HTTP_403[] = "HTTP/1.1 403 Forbidden\r\n";
static const char HTTP_404[] = "HTTP/1.1 404 Not Found\r\n";
static const char HTTP_409[] = "HTTP/1.1 409 Conflict\r\n";

static const char HDR_JSON_KA[] =
    "Content-Type: application/json\r\n"
    "Connection: keep-alive\r\n"
    "Content-Length: ";
#define HDR_JSON_KA_LEN (sizeof(HDR_JSON_KA) - 1)

static const char HDR_EMPTY_KA[] =
    "Content-Length: 0\r\n"
    "Connection: keep-alive\r\n"
    "\r\n";
#define HDR_EMPTY_KA_LEN (sizeof(HDR_EMPTY_KA) - 1)

static int build_response(char *buf, const char *status, int status_len,
                           const char *body, int body_len) {
    char cl_str[12];
    int cl_len = uint_to_str(body_len, cl_str);

    char *p = buf;
    memcpy(p, status, status_len); p += status_len;
    memcpy(p, HDR_JSON_KA, HDR_JSON_KA_LEN); p += HDR_JSON_KA_LEN;
    memcpy(p, cl_str, cl_len); p += cl_len;
    *p++ = '\r'; *p++ = '\n'; *p++ = '\r'; *p++ = '\n';
    memcpy(p, body, body_len); p += body_len;
    return (int)(p - buf);
}

static int build_empty_response(char *buf, const char *status, int status_len) {
    char *p = buf;
    memcpy(p, status, status_len); p += status_len;
    memcpy(p, HDR_EMPTY_KA, HDR_EMPTY_KA_LEN); p += HDR_EMPTY_KA_LEN;
    return (int)(p - buf);
}

/* ── Request Handlers ──────────────────────────────────────────── */

static int handle_healthz(conn_t *conn) {
    memcpy(conn->write_buf, RESP_HEALTHZ, RESP_HEALTHZ_LEN);
    return RESP_HEALTHZ_LEN;
}

static int handle_post_lock(conn_t *conn, const http_request_t *req) {
    json_lock_req_t jr;
    if (unlikely(!parse_json_body(req->body, req->body_len, &jr) || !jr.lockee)) {
        return build_response(conn->write_buf, HTTP_400, sizeof(HTTP_400) - 1,
                              "{\"error\":\"bad request\"}", 23);
    }

    if (unlikely(jr.key_len >= MAX_KEY_LEN)) jr.key_len = MAX_KEY_LEN - 1;
    if (unlikely(jr.lockee_len >= MAX_LOCKEE_LEN)) jr.lockee_len = MAX_LOCKEE_LEN - 1;

    uint64_t h = wyhash(jr.key, jr.key_len, 0);
    shard_t *shard = &g_shards[h & SHARD_MASK];

    int result; /* 0=acquired, 1=conflict */
    char conflict_buf[MAX_LOCKEE_LEN];
    int conflict_lockee_len = 0;

    pthread_spin_lock(&shard->lock);

    lock_entry_t *existing = ht_find(&shard->table, jr.key, jr.key_len, h);
    if (existing) {
        if (existing->lockee_len == jr.lockee_len &&
            memcmp(existing->lockee, jr.lockee, jr.lockee_len) == 0) {
            /* Same lockee: idempotent re-acquire, preserve since */
            result = 0;
        } else if (!jr.force) {
            result = 1;
            memcpy(conflict_buf, existing->lockee, existing->lockee_len);
            conflict_lockee_len = existing->lockee_len;
        } else {
            /* Force takeover */
            memcpy(existing->lockee, jr.lockee, jr.lockee_len);
            existing->lockee_len = jr.lockee_len;
            struct timespec ts;
            clock_gettime(CLOCK_REALTIME, &ts);
            existing->since_sec = ts.tv_sec;
            result = 0;
        }
    } else {
        struct timespec ts;
        clock_gettime(CLOCK_REALTIME, &ts);
        ht_insert(&shard->table, jr.key, jr.key_len, jr.lockee, jr.lockee_len,
                   h, ts.tv_sec);
        result = 0;
    }

    pthread_spin_unlock(&shard->lock);

    if (result == 0) {
        char body[384];
        char *p = body;
        SCPY(p, "{\"locked\":true,\"key\":\"");
        memcpy(p, jr.key, jr.key_len); p += jr.key_len;
        SCPY(p, "\",\"lockee\":\"");
        memcpy(p, jr.lockee, jr.lockee_len); p += jr.lockee_len;
        SCPY(p, "\"}");
        return build_response(conn->write_buf, HTTP_200, sizeof(HTTP_200) - 1,
                              body, (int)(p - body));
    } else {
        char body[384];
        char *p = body;
        SCPY(p, "{\"locked\":false,\"key\":\"");
        memcpy(p, jr.key, jr.key_len); p += jr.key_len;
        SCPY(p, "\",\"currentLockee\":\"");
        memcpy(p, conflict_buf, conflict_lockee_len); p += conflict_lockee_len;
        SCPY(p, "\"}");
        return build_response(conn->write_buf, HTTP_409, sizeof(HTTP_409) - 1,
                              body, (int)(p - body));
    }
}

static int handle_delete_lock(conn_t *conn, const http_request_t *req) {
    json_lock_req_t jr;
    if (unlikely(!parse_json_body(req->body, req->body_len, &jr) || !jr.lockee)) {
        return build_response(conn->write_buf, HTTP_400, sizeof(HTTP_400) - 1,
                              "{\"error\":\"bad request\"}", 23);
    }

    if (unlikely(jr.key_len >= MAX_KEY_LEN)) jr.key_len = MAX_KEY_LEN - 1;
    if (unlikely(jr.lockee_len >= MAX_LOCKEE_LEN)) jr.lockee_len = MAX_LOCKEE_LEN - 1;

    uint64_t h = wyhash(jr.key, jr.key_len, 0);
    shard_t *shard = &g_shards[h & SHARD_MASK];

    int status; /* 0=released, 1=not_found, 2=forbidden */

    pthread_spin_lock(&shard->lock);

    lock_entry_t *existing = ht_find(&shard->table, jr.key, jr.key_len, h);
    if (!existing) {
        status = 1;
    } else if (existing->lockee_len != jr.lockee_len ||
               memcmp(existing->lockee, jr.lockee, jr.lockee_len) != 0) {
        status = 2;
    } else {
        ht_remove(&shard->table, jr.key, jr.key_len, h);
        status = 0;
    }

    pthread_spin_unlock(&shard->lock);

    switch (status) {
    case 0:  return build_empty_response(conn->write_buf, HTTP_200, sizeof(HTTP_200) - 1);
    case 1:  return build_response(conn->write_buf, HTTP_404, sizeof(HTTP_404) - 1, "not found", 9);
    case 2:  return build_response(conn->write_buf, HTTP_403, sizeof(HTTP_403) - 1, "lockee mismatch", 15);
    default: return build_empty_response(conn->write_buf, HTTP_200, sizeof(HTTP_200) - 1);
    }
}

static int handle_get_locks(conn_t *conn) {
    char body[WRITE_BUF_SIZE - 256];
    char *p = body;
    const char *p_limit = body + sizeof(body) - 256;
    SCPY(p, "{\"locks\":[");

    int first = 1;
    char ts_buf[21];

    for (int s = 0; s < NUM_SHARDS; s++) {
        shard_t *shard = &g_shards[s];
        pthread_spin_lock(&shard->lock);

        for (uint32_t i = 0; i < shard->table.cap; i++) {
            lock_entry_t *e = &shard->table.entries[i];
            if (!e->occupied) continue;

            if (!first) *p++ = ','; else first = 0;

            SCPY(p, "{\"key\":\"");
            memcpy(p, e->key, e->key_len); p += e->key_len;
            SCPY(p, "\",\"lockee\":\"");
            memcpy(p, e->lockee, e->lockee_len); p += e->lockee_len;
            SCPY(p, "\",\"since\":\"");
            int ts_len = fmt_rfc3339(e->since_sec, ts_buf);
            memcpy(p, ts_buf, ts_len); p += ts_len;
            SCPY(p, "\"}");


            if (unlikely(p > p_limit)) {
                pthread_spin_unlock(&shard->lock);
                goto done;
            }
        }

        pthread_spin_unlock(&shard->lock);
    }

done:
    SCPY(p, "]}");

    return build_response(conn->write_buf, HTTP_200, sizeof(HTTP_200) - 1,
                          body, (int)(p - body));
}

/* ── Process one HTTP request ──────────────────────────────────── */

static int process_request(conn_t *conn, const http_request_t *req) {
    if (req->path == PATH_HEALTHZ && req->method == METHOD_GET)
        return handle_healthz(conn);
    if (req->path == PATH_LOCK) {
        if (req->method == METHOD_POST) return handle_post_lock(conn, req);
        if (req->method == METHOD_DELETE) return handle_delete_lock(conn, req);
    }
    if (req->path == PATH_LOCKS && req->method == METHOD_GET)
        return handle_get_locks(conn);
    return build_response(conn->write_buf, HTTP_404, sizeof(HTTP_404) - 1, "not found", 9);
}

/* ── Worker Thread ─────────────────────────────────────────────── */

static int create_listener(void) {
    int fd = socket(AF_INET, SOCK_STREAM | SOCK_NONBLOCK, 0);
    if (fd < 0) { perror("socket"); exit(1); }

    int one = 1;
    setsockopt(fd, SOL_SOCKET, SO_REUSEADDR, &one, sizeof(one));
    setsockopt(fd, SOL_SOCKET, SO_REUSEPORT, &one, sizeof(one));

    struct sockaddr_in addr = {
        .sin_family = AF_INET,
        .sin_port = htons(PORT),
        .sin_addr.s_addr = INADDR_ANY
    };

    if (bind(fd, (struct sockaddr *)&addr, sizeof(addr)) < 0) {
        perror("bind"); exit(1);
    }
    if (listen(fd, 1024) < 0) {
        perror("listen"); exit(1);
    }
    return fd;
}

static void close_conn(int epfd, conn_t *c) {
    epoll_ctl(epfd, EPOLL_CTL_DEL, c->fd, NULL);
    close(c->fd);
    free(c);
}

static void *worker_thread(void *arg) {
    (void)arg;

    int listen_fd = create_listener();
    int epfd = epoll_create1(0);
    if (epfd < 0) { perror("epoll_create1"); exit(1); }

    /* Register listener with NULL sentinel */
    struct epoll_event ev;
    ev.events = EPOLLIN;
    ev.data.ptr = NULL;
    epoll_ctl(epfd, EPOLL_CTL_ADD, listen_fd, &ev);

    struct epoll_event events[MAX_EVENTS];

    for (;;) {
        int n = epoll_wait(epfd, events, MAX_EVENTS, -1);

        for (int i = 0; i < n; i++) {
            /* ── Listener ── */
            if (events[i].data.ptr == NULL) {
                for (;;) {
                    int cfd = accept4(listen_fd, NULL, NULL, SOCK_NONBLOCK);
                    if (cfd < 0) break;

                    int one = 1;
                    setsockopt(cfd, IPPROTO_TCP, TCP_NODELAY, &one, sizeof(one));

                    conn_t *c = (conn_t *)malloc(sizeof(conn_t));
                    c->fd = cfd;
                    c->read_pos = 0;
                    c->write_pos = 0;
                    c->write_len = 0;

                    struct epoll_event cev;
                    cev.events = EPOLLIN;
                    cev.data.ptr = c;
                    epoll_ctl(epfd, EPOLL_CTL_ADD, cfd, &cev);
                }
                continue;
            }

            /* ── Connection ── */
            conn_t *c = (conn_t *)events[i].data.ptr;
            int fd = c->fd;

            if (events[i].events & (EPOLLERR | EPOLLHUP)) {
                close_conn(epfd, c);
                continue;
            }

            /* Flush pending writes first */
            if (events[i].events & EPOLLOUT) {
                while (c->write_pos < c->write_len) {
                    ssize_t w = send(fd, c->write_buf + c->write_pos,
                                     c->write_len - c->write_pos, MSG_NOSIGNAL);
                    if (w <= 0) {
                        if (w < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) break;
                        close_conn(epfd, c); goto next_event;
                    }
                    c->write_pos += (uint16_t)w;
                }
                if (c->write_pos < c->write_len) continue; /* still writing */
                c->write_pos = 0;
                c->write_len = 0;
                /* Done writing — switch to read-only and fall through to read */
                struct epoll_event cev;
                cev.events = EPOLLIN;
                cev.data.ptr = c;
                epoll_ctl(epfd, EPOLL_CTL_MOD, fd, &cev);
                /* Fall through to EPOLLIN to drain any buffered data */
            }

            /* Read and process — edge-triggered, drain fully */
            if ((events[i].events & EPOLLIN) || c->read_pos > 0) {
                for (;;) {
                    /* Try to parse from existing buffer first */
                    while (c->read_pos > 0) {
                        http_request_t req;
                        int parsed = parse_request(c->read_buf, c->read_pos, &req);
                        if (parsed <= 0) break;

                        int resp_len = process_request(c, &req);

                        int rem = c->read_pos - parsed;
                        if (rem > 0)
                            memmove(c->read_buf, c->read_buf + parsed, rem);
                        c->read_pos = (uint16_t)rem;

                        /* Send response */
                        c->write_len = (uint16_t)resp_len;
                        c->write_pos = 0;
                        while (c->write_pos < c->write_len) {
                            ssize_t w = send(fd, c->write_buf + c->write_pos,
                                             c->write_len - c->write_pos, MSG_NOSIGNAL);
                            if (w <= 0) {
                                if (w < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) {
                                    struct epoll_event cev;
                                    cev.events = EPOLLIN | EPOLLOUT;
                                    cev.data.ptr = c;
                                    epoll_ctl(epfd, EPOLL_CTL_MOD, fd, &cev);
                                    goto next_event;
                                }
                                close_conn(epfd, c); goto next_event;
                            }
                            c->write_pos += (uint16_t)w;
                        }
                        c->write_pos = 0;
                        c->write_len = 0;
                    }

                    /* Read more data */
                    if (c->read_pos >= READ_BUF_SIZE) {
                        close_conn(epfd, c); goto next_event;
                    }
                    ssize_t r = recv(fd, c->read_buf + c->read_pos,
                                     READ_BUF_SIZE - c->read_pos, 0);
                    if (r <= 0) {
                        if (r < 0 && (errno == EAGAIN || errno == EWOULDBLOCK)) break;
                        close_conn(epfd, c); goto next_event;
                    }
                    c->read_pos += (uint16_t)r;
                }
            }

        next_event:;
        }
    }

    return NULL;
}

/* ── Main ──────────────────────────────────────────────────────── */

int main(void) {
    signal(SIGPIPE, SIG_IGN);
    shards_init();

    const char *nt_env = getenv("THREADS");
    int nthreads = nt_env ? atoi(nt_env) : (int)sysconf(_SC_NPROCESSORS_ONLN);
    if (nthreads < 1) nthreads = 1;
    if (nthreads > 64) nthreads = 64;

    fprintf(stderr, "C epoll server on :%d (%d threads, %d shards)\n",
            PORT, nthreads, NUM_SHARDS);

    pthread_t threads[64];
    for (int i = 0; i < nthreads; i++)
        pthread_create(&threads[i], NULL, worker_thread, NULL);

    for (int i = 0; i < nthreads; i++)
        pthread_join(threads[i], NULL);

    return 0;
}
