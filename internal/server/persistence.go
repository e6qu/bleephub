package bleephub

import (
	"bufio"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
	"github.com/e6qu/bleephub/internal/dqliteaddr"
	zlog "github.com/rs/zerolog/log"
	_ "modernc.org/sqlite" // SQLite driver — pure Go, no CGO
)

type dbDialect struct {
	name         string
	putSQL       string // INSERT … ON CONFLICT upsert
	deleteSQL    string
	listSQL      string
	valueSQL     string
	getSQL       string
	setSQL       string
	raiseSQL     string // counter upsert that never lowers a stored value
	acquireSQL   string
	releaseSQL   string
	readVersion  string
	writeVersion string
}

var (
	sqliteDialect = dbDialect{
		name:         "sqlite",
		putSQL:       `INSERT INTO kv (bucket, key, value) VALUES (?, ?, ?) ON CONFLICT(bucket, key) DO UPDATE SET value = excluded.value`,
		deleteSQL:    `DELETE FROM kv WHERE bucket = ? AND key = ?`,
		listSQL:      `SELECT key, value FROM kv WHERE bucket = ?`,
		valueSQL:     `SELECT value FROM kv WHERE bucket = ? AND key = ?`,
		getSQL:       `SELECT value FROM counters WHERE name = ?`,
		setSQL:       `INSERT INTO counters (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value`,
		raiseSQL:     `INSERT INTO counters (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = MAX(counters.value, excluded.value)`,
		acquireSQL:   `INSERT INTO locks (name, owner, expires_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at WHERE locks.expires_at <= ?`,
		releaseSQL:   `DELETE FROM locks WHERE name = ? AND owner = ?`,
		readVersion:  `SELECT value FROM schema_meta WHERE key = 'version'`,
		writeVersion: `INSERT INTO schema_meta (key, value) VALUES ('version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	}
)

// currentSchemaVersion is the schema this build writes and reads. A database
// stamped with a higher version was written by a newer build whose row layout
// this one cannot be trusted to decode, so startup refuses it.
const currentSchemaVersion = 3

const (
	persistenceEncryptionKeyEnvironment = "BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY"
	sealedPersistenceValuePrefix        = "bleephub:sealed:v1:"
	opaquePersistenceKeyPrefix          = "hmac:v1:"
)

// sensitivePersistenceBuckets is deliberately centralized at the persistence
// boundary. A caller cannot accidentally write a newly-added value in one of
// these established credential stores as plaintext just because it used Put,
// PutBatch, or a bespoke transaction.
var sensitivePersistenceBuckets = map[string]struct{}{
	"actions_crypto":          {},
	"agents_org_secrets":      {},
	"agents_repo_secrets":     {},
	"apps":                    {},
	"codespace_secrets":       {},
	"dependabot_org_secrets":  {},
	"dependabot_secrets":      {},
	"dependabot_user_secrets": {},
	"env_secrets":             {},
	"installation_tokens":     {},
	"login_sessions":          {},
	"oauth_apps":              {},
	"org_secrets":             {},
	"refresh_tokens":          {},
	"repo_secrets":            {},
	"tokens":                  {},
	"user_to_server_tokens":   {},
}

// opaquePersistenceKeyBuckets use bearer credentials as their logical map
// keys. Persist only a keyed digest so a database read does not disclose a
// credential even though point lookup remains possible.
var opaquePersistenceKeyBuckets = map[string]struct{}{
	"installation_tokens":   {},
	"login_sessions":        {},
	"refresh_tokens":        {},
	"tokens":                {},
	"user_to_server_tokens": {},
}

// schemaMetaDDL bootstraps the table that carries the schema version itself.
// It predates versioning, so it is created unconditionally on every open.
const schemaMetaDDL = `CREATE TABLE IF NOT EXISTS schema_meta (
	key   TEXT NOT NULL PRIMARY KEY,
	value TEXT NOT NULL
);`

type schemaMigration struct {
	version    int
	statements []string
}

var schemaMigrations = []schemaMigration{
	{
		version: 1,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS kv (
	bucket TEXT NOT NULL,
	key    TEXT NOT NULL,
	value  BLOB NOT NULL,
	PRIMARY KEY (bucket, key)
);`,
			`CREATE TABLE IF NOT EXISTS counters (
	name  TEXT NOT NULL PRIMARY KEY,
	value INTEGER NOT NULL
);`,
		},
	},
	{
		version: 2,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS locks (
	name       TEXT NOT NULL PRIMARY KEY,
	owner      TEXT NOT NULL,
	expires_at INTEGER NOT NULL
);`,
		},
	},
	{
		version: 3,
		statements: []string{
			`CREATE TABLE IF NOT EXISTS schedule_claims (
	key          TEXT NOT NULL PRIMARY KEY,
	scheduled_at INTEGER NOT NULL
);`,
			`CREATE INDEX IF NOT EXISTS schedule_claims_scheduled_at ON schedule_claims (scheduled_at);`,
		},
	},
}

type Persistence struct {
	db            *sql.DB
	dialect       dbDialect
	mu            sync.Mutex
	encryptionKey []byte
	keyDigestKey  []byte
	localRevision atomic.Int64
	// keyHighWater caches the durable per-bucket identifier high-water mark
	// already written by this process, so the common case costs no extra SQL.
	keyHighWater map[string]int64
}

func (p *Persistence) Ready(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if err := p.db.PingContext(ctx); err != nil {
		return fmt.Errorf("persistence ping: %w", err)
	}
	return nil
}

// persistenceFailure is raised by the Must* helpers. It aborts the request
// that was mid-write instead of the process: killing the process from inside a
// handler leaves the mutation half-applied everywhere else with no chance to
// report it.
type persistenceFailure struct {
	op     string
	bucket string
	key    string
	err    error
}

func (e *persistenceFailure) Error() string {
	return fmt.Sprintf("bleephub persistence %s %s/%s failed: %v", e.op, e.bucket, e.key, e.err)
}

func (e *persistenceFailure) Unwrap() error { return e.err }

type persistencePut struct {
	bucket string
	key    string
	value  interface{}
}

type persistOpKind int

const (
	persistOpPut persistOpKind = iota
	persistOpDelete
)

type persistOp struct {
	kind   persistOpKind
	bucket string
	key    string
	raw    []byte
}

// persistBatch accumulates the record writes of one multi-step mutation so
// they reach the database as a single transaction. A crash before Commit
// leaves the previous state intact rather than a partially cascaded one.
type persistBatch struct {
	p   *Persistence
	ops []persistOp
	err error
}

func newPersistBatch(p *Persistence) *persistBatch {
	return &persistBatch{p: p}
}

func (b *persistBatch) Put(bucket, key string, v interface{}) {
	if b.p == nil || b.err != nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		b.err = fmt.Errorf("marshal %s/%s: %w", bucket, key, err)
		return
	}
	b.ops = append(b.ops, persistOp{kind: persistOpPut, bucket: bucket, key: key, raw: raw})
}

func (b *persistBatch) Delete(bucket, key string) {
	if b.p == nil || b.err != nil {
		return
	}
	b.ops = append(b.ops, persistOp{kind: persistOpDelete, bucket: bucket, key: key})
}

func (b *persistBatch) Commit() error {
	if b.err != nil {
		return b.err
	}
	if b.p == nil || len(b.ops) == 0 {
		return nil
	}
	return b.p.apply(b.ops)
}

// apply runs every operation in one transaction.
func (p *Persistence) apply(ops []persistOp) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	raised := map[string]int64{}
	for _, op := range ops {
		storageKey := p.storageKey(op.bucket, op.key)
		switch op.kind {
		case persistOpPut:
			if err := p.raiseKeyHighWaterTx(tx, op.bucket, storageKey, raised); err != nil {
				_ = tx.Rollback()
				return err
			}
			raw, err := p.sealValue(op.bucket, storageKey, op.raw)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.Exec(p.dialect.putSQL, op.bucket, storageKey, raw); err != nil {
				_ = tx.Rollback()
				return err
			}
		case persistOpDelete:
			if _, err := tx.Exec(p.dialect.deleteSQL, op.bucket, storageKey); err != nil {
				_ = tx.Rollback()
				return err
			}
		default:
			_ = tx.Rollback()
			return fmt.Errorf("unknown persistence operation %d for %s/%s", op.kind, op.bucket, op.key)
		}
	}
	revision, err := p.bumpStateRevisionTx(tx)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	p.observeLocalRevision(revision)
	for bucket, next := range raised {
		p.cacheKeyHighWater(bucket, next)
	}
	return nil
}

// PutBatch commits related records in one SQLite transaction. Callers update
// their in-memory indexes only after this returns successfully.
func (p *Persistence) PutBatch(entries ...persistencePut) error {
	if p == nil {
		return nil
	}
	batch := newPersistBatch(p)
	for _, entry := range entries {
		batch.Put(entry.bucket, entry.key, entry.value)
	}
	return batch.Commit()
}

func (p *Persistence) DeleteBatch(entries ...persistencePut) error {
	if p == nil {
		return nil
	}
	batch := newPersistBatch(p)
	for _, entry := range entries {
		batch.Delete(entry.bucket, entry.key)
	}
	return batch.Commit()
}

// ClaimOIDCLogoutAndDeleteSessions stores a replay marker and deletes the
// selected browser sessions in one SQLite/dqlite transaction. The kv primary
// key makes token claiming exclusive across processes and replicas.
func (p *Persistence) ClaimOIDCLogoutAndDeleteSessions(replayKey string, expiresAt, now time.Time, provider, issuer, sid, subject string) (bool, error) {
	marker, err := json.Marshal(oidcLogoutReplayMarker{ExpiresAt: expiresAt})
	if err != nil {
		return false, fmt.Errorf("marshal OpenID Connect logout replay marker: %w", err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	result, err := tx.Exec(`INSERT INTO kv (bucket, key, value) VALUES (?, ?, ?) ON CONFLICT(bucket, key) DO NOTHING`, oidcLogoutClaimsBucket, replayKey, marker)
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if inserted == 0 {
		var raw []byte
		if err := tx.QueryRow(p.dialect.valueSQL, oidcLogoutClaimsBucket, replayKey).Scan(&raw); err != nil {
			return false, err
		}
		var persisted oidcLogoutReplayMarker
		if err := json.Unmarshal(raw, &persisted); err != nil {
			return false, fmt.Errorf("decode OpenID Connect logout replay marker %s: %w", replayKey, err)
		}
		if persisted.ExpiresAt.After(now) {
			return false, nil
		}
		if _, err := tx.Exec(p.dialect.deleteSQL, oidcLogoutClaimsBucket, replayKey); err != nil {
			return false, err
		}
		result, err = tx.Exec(`INSERT INTO kv (bucket, key, value) VALUES (?, ?, ?) ON CONFLICT(bucket, key) DO NOTHING`, oidcLogoutClaimsBucket, replayKey, marker)
		if err != nil {
			return false, err
		}
		inserted, err = result.RowsAffected()
		if err != nil {
			return false, err
		}
		if inserted == 0 {
			return false, nil
		}
	}

	rows, err := tx.Query(p.dialect.listSQL, oidcLogoutClaimsBucket)
	if err != nil {
		return false, err
	}
	type expiredMarker struct{ key string }
	expired := make([]expiredMarker, 0)
	for rows.Next() {
		var key string
		var raw []byte
		if err := rows.Scan(&key, &raw); err != nil {
			_ = rows.Close()
			return false, err
		}
		var persisted oidcLogoutReplayMarker
		if err := json.Unmarshal(raw, &persisted); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("decode OpenID Connect logout replay marker %s: %w", key, err)
		}
		if !persisted.ExpiresAt.After(now) {
			expired = append(expired, expiredMarker{key: key})
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, entry := range expired {
		if _, err := tx.Exec(p.dialect.deleteSQL, oidcLogoutClaimsBucket, entry.key); err != nil {
			return false, err
		}
	}

	rows, err = tx.Query(p.dialect.listSQL, loginSessionsBucket)
	if err != nil {
		return false, err
	}
	ids := make([]string, 0)
	for rows.Next() {
		var id string
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			_ = rows.Close()
			return false, err
		}
		raw, err = p.openValue(loginSessionsBucket, id, raw)
		if err != nil {
			_ = rows.Close()
			return false, err
		}
		var session LoginSession
		if err := json.Unmarshal(raw, &session); err != nil {
			_ = rows.Close()
			return false, fmt.Errorf("decode OpenID Connect login session %s: %w", id, err)
		}
		if oidcLoginSessionMatches(&session, provider, issuer, sid, subject) {
			ids = append(ids, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	for _, id := range ids {
		if _, err := tx.Exec(p.dialect.deleteSQL, loginSessionsBucket, id); err != nil {
			return false, err
		}
	}
	revision, err := p.bumpStateRevisionTx(tx)
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	p.observeLocalRevision(revision)
	return true, nil
}

func openSQLite(dataDir string) (*sql.DB, error) {
	// #nosec G703 -- dataDir is deployment configuration, not request input;
	// the only appended component is the fixed database filename.
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "bleephub.db")
	// Durability contract for the single-node store: journal_mode(WAL) with
	// synchronous(FULL) fsyncs the write-ahead log at every commit, so a write
	// this layer has acknowledged (a returned MustPut/Commit) survives an OS
	// crash or power loss — not only a process crash, which synchronous(NORMAL)
	// alone guarantees. dqlite deployments get durability from Raft replication
	// instead and do not use this path.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite %s: %w", dbPath, err)
	}
	return db, nil
}

// openDqlite connects to the durable dqlite quorum using its stable private
// addresses. The dqlite driver discovers the current leader from this seed
// set and refreshes its membership knowledge from the quorum itself.
func openDqlite(addresses string) (*sql.DB, error) {
	addressMap, err := dqliteaddr.FromEnvironment(os.Getenv(dqliteaddr.Environment))
	if err != nil {
		return nil, permanentErrf("parse dqlite address map: %w", err)
	}
	secret := strings.TrimSpace(os.Getenv(dqliteaddr.SecretEnvironment))
	if secret == "" {
		return nil, permanentErrf("%s is required: it authenticates and encrypts the dqlite transport carrying statements against the whole database", dqliteaddr.SecretEnvironment)
	}
	store := client.NewInmemNodeStore()
	servers := make([]client.NodeInfo, 0, 3)
	for _, address := range strings.Split(addresses, ",") {
		address = strings.TrimSpace(address)
		if address == "" {
			continue
		}
		servers = append(servers, client.NodeInfo{Address: address})
	}
	if len(servers) == 0 {
		return nil, permanentErrf("BLEEPHUB_DQLITE_SERVERS must contain at least one dqlite server address")
	}
	if err := store.Set(context.Background(), servers); err != nil {
		return nil, permanentErrf("configure dqlite server set: %w", err)
	}

	dqliteDriver, err := driver.New(store,
		driver.WithDialFunc(dqliteDialer(addressMap, secret)),
		driver.WithAttemptTimeout(5*time.Second),
		driver.WithConnectionBackoffFactor(100*time.Millisecond),
		driver.WithConnectionBackoffCap(time.Second),
		driver.WithRetryLimit(12),
	)
	if err != nil {
		return nil, permanentErrf("create dqlite driver: %w", err)
	}
	connector, err := dqliteDriver.OpenConnector("bleephub")
	if err != nil {
		return nil, fmt.Errorf("open dqlite connector: %w", err)
	}
	db := sql.OpenDB(connector)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping dqlite: %w", err)
	}
	return db, nil
}

// dqliteDialer binds durable-member address resolution and the cluster
// credential to the transport used by the driver.
func dqliteDialer(addresses dqliteaddr.Map, secret string) client.DialFunc {
	return func(ctx context.Context, address string) (net.Conn, error) {
		return dqliteHTTPDial(ctx, addresses.Resolve(address), secret)
	}
}

// dqliteHTTPDial opens the dqlite HTTP-upgrade transport exposed by each
// private Cloud Map member address. The upgrade keeps the dqlite wire protocol
// private while allowing Amazon ECS tasks to retain stable advertised member
// identities across replacement and scale-to-zero restarts.
func dqliteHTTPDial(ctx context.Context, address, secret string) (net.Conn, error) {
	dialer := &net.Dialer{}
	rawConn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial dqlite %s: %w", address, err)
	}
	tlsConfig, err := dqliteaddr.TLSConfig(secret, false)
	if err != nil {
		_ = rawConn.Close()
		return nil, fmt.Errorf("configure dqlite TLS: %w", err)
	}
	conn := tls.Client(rawConn, tlsConfig)
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("authenticate dqlite TLS endpoint %s: %w", address, err)
	}

	requestURL, err := url.Parse("https://" + address + "/dqlite")
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("build dqlite upgrade request: %w", err)
	}
	request := &http.Request{Method: http.MethodGet, URL: requestURL, Host: requestURL.Host, Header: make(http.Header)}
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Upgrade", "dqlite")
	request.Header.Set(dqliteaddr.SecretHeader, secret)
	if err := request.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("write dqlite upgrade request: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(conn), request)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read dqlite upgrade response: %w", err)
	}
	if response.StatusCode != http.StatusSwitchingProtocols || !strings.EqualFold(response.Header.Get("Upgrade"), "dqlite") {
		_ = response.Body.Close()
		_ = conn.Close()
		return nil, fmt.Errorf("dqlite endpoint %s did not accept protocol upgrade: %s", address, response.Status)
	}
	return conn, nil
}

func NewPersistence() (*Persistence, error) {
	if os.Getenv("BLEEPHUB_DATABASE_URL") != "" {
		return nil, permanentErrf("BLEEPHUB_DATABASE_URL is no longer supported; bleephub stores its own state in SQLite via BLEEPHUB_PERSIST=true and BLEEPHUB_DATA_DIR")
	}

	if os.Getenv("BLEEPHUB_PERSIST") != "true" {
		return nil, nil //nolint:nilnil // intentional: nil persistence = disabled
	}
	encryptionKey, err := persistenceEncryptionKey()
	if err != nil {
		return nil, &permanentPersistenceError{err: err}
	}

	dataDir := os.Getenv("BLEEPHUB_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	var db *sql.DB
	if addresses := strings.TrimSpace(os.Getenv("BLEEPHUB_DQLITE_SERVERS")); addresses != "" {
		db, err = openDqlite(addresses)
	} else {
		db, err = openSQLite(dataDir)
	}
	if err != nil {
		return nil, err
	}
	dialect := sqliteDialect
	if os.Getenv("BLEEPHUB_DQLITE_SERVERS") != "" {
		dialect.name = "dqlite"
	}
	if err := migrateSchema(db, dialect); err != nil {
		_ = db.Close()
		return nil, err
	}
	p := &Persistence{
		db:            db,
		dialect:       dialect,
		encryptionKey: encryptionKey,
		keyDigestKey:  derivePersistenceKey(encryptionKey, "opaque-key-digests"),
		keyHighWater:  map[string]int64{},
	}
	if err := p.migrateSensitiveRows(); err != nil {
		_ = db.Close()
		return nil, err
	}
	revision, err := p.StateRevision()
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	p.localRevision.Store(revision)
	return p, nil
}

func persistenceEncryptionKey() ([]byte, error) {
	encoded := strings.TrimSpace(os.Getenv(persistenceEncryptionKeyEnvironment))
	if encoded == "" {
		return nil, fmt.Errorf("%s is required when BLEEPHUB_PERSIST=true; provide a stable base64-encoded 32-byte key", persistenceEncryptionKeyEnvironment)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be a base64-encoded 32-byte key", persistenceEncryptionKeyEnvironment)
	}
	return key, nil
}

func derivePersistenceKey(master []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("bleephub:persistence:v1:" + purpose))
	return mac.Sum(nil)
}

func isSensitivePersistenceBucket(bucket string) bool {
	_, ok := sensitivePersistenceBuckets[bucket]
	return ok
}

func isOpaquePersistenceKeyBucket(bucket string) bool {
	_, ok := opaquePersistenceKeyBuckets[bucket]
	return ok
}

func (p *Persistence) storageKey(bucket, key string) string {
	if !isOpaquePersistenceKeyBucket(bucket) || strings.HasPrefix(key, opaquePersistenceKeyPrefix) {
		return key
	}
	return p.opaqueDigestKey(bucket, key)
}

// opaqueLookupKey derives the storage key for a value presented by a client
// (a bearer token or session id). Unlike storageKey it NEVER passes an already
// "hmac:v1:"-prefixed value through: the stored row keys are those digests, so
// honoring a client-supplied digest as a key would let anyone who read a row
// key (a backup, a replica, a leaked query) use it as the credential itself.
// A non-opaque bucket keeps the raw value.
func (p *Persistence) opaqueLookupKey(bucket, value string) string {
	if !isOpaquePersistenceKeyBucket(bucket) {
		return value
	}
	return p.opaqueDigestKey(bucket, value)
}

func (p *Persistence) opaqueDigestKey(bucket, value string) string {
	mac := hmac.New(sha256.New, p.keyDigestKey)
	_, _ = mac.Write([]byte(bucket))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(value))
	return opaquePersistenceKeyPrefix + hex.EncodeToString(mac.Sum(nil))
}

func persistenceAssociatedData(bucket, key string) []byte {
	return []byte(bucket + "\x00" + key)
}

func (p *Persistence) sealValue(bucket, key string, raw []byte) ([]byte, error) {
	if !isSensitivePersistenceBucket(bucket) {
		return raw, nil
	}
	block, err := aes.NewCipher(p.encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("initialize persistence encryption: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize persistence authenticated encryption: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("generate persistence encryption nonce: %w", err)
	}
	envelope := append(nonce, gcm.Seal(nil, nonce, raw, persistenceAssociatedData(bucket, key))...)
	encoded := base64.RawStdEncoding.EncodeToString(envelope)
	return []byte(sealedPersistenceValuePrefix + encoded), nil
}

func (p *Persistence) openValue(bucket, key string, raw []byte) ([]byte, error) {
	if !isSensitivePersistenceBucket(bucket) {
		return raw, nil
	}
	if !strings.HasPrefix(string(raw), sealedPersistenceValuePrefix) {
		return raw, nil // legacy plaintext; migrateSensitiveRows rewrites it.
	}
	envelope, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(string(raw), sealedPersistenceValuePrefix))
	if err != nil {
		return nil, fmt.Errorf("decode encrypted persistence row %s/%s: %w", bucket, key, err)
	}
	block, err := aes.NewCipher(p.encryptionKey)
	if err != nil {
		return nil, fmt.Errorf("initialize persistence decryption: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("initialize persistence authenticated decryption: %w", err)
	}
	if len(envelope) < gcm.NonceSize() {
		return nil, fmt.Errorf("encrypted persistence row %s/%s is truncated", bucket, key)
	}
	nonce, ciphertext := envelope[:gcm.NonceSize()], envelope[gcm.NonceSize():]
	plain, err := gcm.Open(nil, nonce, ciphertext, persistenceAssociatedData(bucket, key))
	if err != nil {
		return nil, fmt.Errorf("decrypt persistence row %s/%s (wrong key or corrupted data): %w", bucket, key, err)
	}
	return plain, nil
}

// migrateSensitiveRows encrypts legacy plaintext values and replaces raw
// bearer-token keys with keyed digests in one transaction. It also decrypts
// every existing envelope during startup, making a wrong deployment key a
// fail-fast configuration error rather than delayed data loss.
func (p *Persistence) migrateSensitiveRows() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for bucket := range sensitivePersistenceBuckets {
		rows, err := tx.Query(p.dialect.listSQL, bucket)
		if err != nil {
			return fmt.Errorf("scan sensitive persistence bucket %s: %w", bucket, err)
		}
		type row struct {
			key string
			raw []byte
		}
		var entries []row
		for rows.Next() {
			var entry row
			if err := rows.Scan(&entry.key, &entry.raw); err != nil {
				_ = rows.Close()
				return err
			}
			entries = append(entries, entry)
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for _, entry := range entries {
			plain, err := p.openValue(bucket, entry.key, entry.raw)
			if err != nil {
				return err
			}
			storageKey := p.storageKey(bucket, entry.key)
			alreadySealed := strings.HasPrefix(string(entry.raw), sealedPersistenceValuePrefix)
			if alreadySealed && storageKey == entry.key {
				continue
			}
			sealed, err := p.sealValue(bucket, storageKey, plain)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(p.dialect.putSQL, bucket, storageKey, sealed); err != nil {
				return err
			}
			if storageKey != entry.key {
				if _, err := tx.Exec(p.dialect.deleteSQL, bucket, entry.key); err != nil {
					return err
				}
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit sensitive persistence migration: %w", err)
	}
	return nil
}

// migrateSchema brings the database up to currentSchemaVersion and refuses a
// database written by a newer build. Rows are marshalled structs, so decoding
// them against a layout this build does not know silently drops or zeroes
// fields; startup stops instead.
func migrateSchema(db *sql.DB, dialect dbDialect) error {
	if _, err := db.Exec(schemaMetaDDL); err != nil {
		return fmt.Errorf("persistence schema metadata: %w", err)
	}
	var stored string
	err := db.QueryRow(dialect.readVersion).Scan(&stored)
	switch {
	case err == sql.ErrNoRows:
		stored = "0"
	case err != nil:
		return fmt.Errorf("read persistence schema version: %w", err)
	}
	version, err := strconv.Atoi(strings.TrimSpace(stored))
	if err != nil {
		return permanentErrf("persistence schema version %q is not a number", stored)
	}
	if version > currentSchemaVersion {
		return permanentErrf("persistence schema version %d was written by a newer bleephub; this build understands up to version %d", version, currentSchemaVersion)
	}
	for _, migration := range schemaMigrations {
		if migration.version <= version {
			continue
		}
		for _, statement := range migration.statements {
			if _, err := db.Exec(statement); err != nil {
				return fmt.Errorf("persistence schema migration %d: %w", migration.version, err)
			}
		}
	}
	if _, err := db.Exec(dialect.writeVersion, strconv.Itoa(currentSchemaVersion)); err != nil {
		return fmt.Errorf("stamp persistence schema version: %w", err)
	}
	return nil
}

// permanentPersistenceError marks a persistence-initialization failure that no
// amount of retrying can fix: a misconfiguration (malformed dqlite address map,
// missing transport secret, unusable encryption key, empty server set) or a
// schema written by a newer bleephub. It is distinct from the transient
// connect/query failures that occur while a dqlite quorum is still forming, and
// MustNewPersistence fails fast on it instead of looping forever.
type permanentPersistenceError struct{ err error }

func (e *permanentPersistenceError) Error() string { return e.err.Error() }
func (e *permanentPersistenceError) Unwrap() error { return e.err }

// permanentErrf builds a permanent persistence error.
func permanentErrf(format string, args ...any) error {
	return &permanentPersistenceError{err: fmt.Errorf(format, args...)}
}

// isPermanentPersistenceError reports whether err (or anything it wraps) is a
// permanentPersistenceError.
func isPermanentPersistenceError(err error) bool {
	var p *permanentPersistenceError
	return errors.As(err, &p)
}

// startupQuorumDeadline bounds how long MustNewPersistence retries a transient
// dqlite failure. Quorum forms in seconds; a wait this long means a downed peer
// or a misconfiguration, and failing loudly beats retrying forever behind a
// listener that never starts (so a health check sees nothing).
const startupQuorumDeadline = 5 * time.Minute

func MustNewPersistence() *Persistence {
	// Honour SIGTERM during the wait: this runs before the HTTP listener
	// exists, so without it an orchestrator can only SIGKILL a still-starting
	// process. A second registration alongside main's is harmless and is
	// unregistered as soon as quorum forms.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	giveUpAt := time.Now().Add(startupQuorumDeadline)
	for {
		p, err := NewPersistence()
		if err == nil {
			return p
		}
		// A misconfiguration or an unsupported schema can never be fixed by
		// waiting; retrying it forever only hides the real cause behind an
		// endless "waiting for dqlite quorum" log. Only transient failures
		// (dial/ping/query while the quorum forms) are worth retrying.
		if isPermanentPersistenceError(err) {
			zlog.Fatal().Err(err).Msg("persistence configuration is unrecoverable")
		}
		if strings.TrimSpace(os.Getenv("BLEEPHUB_DQLITE_SERVERS")) == "" {
			zlog.Fatal().Err(err).Msg("persistence configuration failed")
		}
		if time.Now().After(giveUpAt) {
			zlog.Fatal().Err(err).Dur("deadline", startupQuorumDeadline).
				Msg("dqlite quorum did not form within the startup deadline")
		}
		zlog.Warn().Err(err).Msg("waiting for dqlite quorum")
		select {
		case <-ctx.Done():
			zlog.Fatal().Msg("startup interrupted before dqlite quorum formed")
		case <-time.After(time.Second):
		}
	}
}

// MustPut writes a record or aborts the calling request. The panic is caught
// by the server's recovery middleware and reported as a 500; the deferred
// unlocks on the way out release the store lock the caller was holding.
func (p *Persistence) MustPut(bucket, key string, v interface{}) {
	if err := p.Put(bucket, key, v); err != nil {
		panic(&persistenceFailure{op: "write", bucket: bucket, key: key, err: err})
	}
}

func (p *Persistence) MustDelete(bucket, key string) {
	if err := p.Delete(bucket, key); err != nil {
		panic(&persistenceFailure{op: "delete", bucket: bucket, key: key, err: err})
	}
}

func (p *Persistence) Put(bucket, key string, v interface{}) error {
	if p == nil {
		return nil
	}
	batch := newPersistBatch(p)
	batch.Put(bucket, key, v)
	return batch.Commit()
}

// OwnedExclusively reports whether this process is the only writer of the
// database. A local SQLite file is exclusive; a dqlite quorum is shared with
// every other replica, so state this process did not create is not its to
// rewrite at startup.
func (p *Persistence) OwnedExclusively() bool {
	if p == nil {
		return true
	}
	return p.dialect.name == "sqlite"
}

// bucketKeyCounter names the durable high-water mark for a bucket whose keys
// are allocated identifiers.
func bucketKeyCounter(bucket string) string { return "kv_max_key:" + bucket }

// keyHighWaterCandidate reports the next free identifier implied by a record
// key. Buckets keyed by names rather than identifiers have no counter.
func keyHighWaterCandidate(key string) (int64, bool) {
	id, err := strconv.ParseInt(key, 10, 64)
	if err != nil || id < 0 || id == math.MaxInt64 {
		return 0, false
	}
	return id + 1, true
}

func (p *Persistence) cacheKeyHighWater(bucket string, next int64) {
	if p.keyHighWater == nil {
		p.keyHighWater = map[string]int64{}
	}
	if p.keyHighWater[bucket] < next {
		p.keyHighWater[bucket] = next
	}
}

func (p *Persistence) raiseKeyHighWaterTx(tx *sql.Tx, bucket, key string, raised map[string]int64) error {
	next, raise := keyHighWaterCandidate(key)
	if !raise || p.keyHighWater[bucket] >= next || raised[bucket] >= next {
		return nil
	}
	if _, err := tx.Exec(p.dialect.raiseSQL, bucketKeyCounter(bucket), next); err != nil {
		return err
	}
	raised[bucket] = next
	return nil
}

// KeyHighWater returns the highest identifier ever written to a bucket plus
// one. Loaders take the maximum of this and the surviving rows so a deleted
// entity's identifier — which for attestations, package files and artifacts is
// also its object-store key — is never handed to a new entity.
func (p *Persistence) KeyHighWater(bucket string) (int64, error) {
	if p == nil {
		return 0, nil
	}
	return p.GetCounter(bucketKeyCounter(bucket))
}

// AcquireLock takes the named lock for owner until ttl elapses, returning
// false when another owner still holds it. The expiry bounds a lock stranded
// by a replica that died while holding it.
func (p *Persistence) AcquireLock(name, owner string, ttl time.Duration) (bool, error) {
	if p == nil {
		return false, fmt.Errorf("acquire lock %s: persistence is disabled", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now().UnixNano()
	result, err := p.db.Exec(p.dialect.acquireSQL, name, owner, now+ttl.Nanoseconds(), now)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// ReleaseLock drops the named lock if owner still holds it.
func (p *Persistence) ReleaseLock(name, owner string) error {
	if p == nil {
		return fmt.Errorf("release lock %s: persistence is disabled", name)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(p.dialect.releaseSQL, name, owner)
	return err
}

// ClaimScheduleFiring atomically selects one replica for a cron tuple/minute.
// Claims are intentionally outside the metadata revision feed: they coordinate
// the creation of a durable Workflow row but are not themselves API state.
func (p *Persistence) ClaimScheduleFiring(key string, minute time.Time) (bool, error) {
	if p == nil {
		return false, fmt.Errorf("claim scheduled workflow: persistence is disabled")
	}
	digest := sha256.Sum256([]byte(key + "\x00" + minute.UTC().Truncate(time.Minute).Format(time.RFC3339)))
	claimKey := hex.EncodeToString(digest[:])
	p.mu.Lock()
	defer p.mu.Unlock()
	tx, err := p.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`INSERT INTO schedule_claims (key, scheduled_at) VALUES (?, ?) ON CONFLICT(key) DO NOTHING`, claimKey, minute.UTC().Unix())
	if err != nil {
		return false, err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if _, err := tx.Exec(`DELETE FROM schedule_claims WHERE scheduled_at < ?`, minute.UTC().Add(-48*time.Hour).Unix()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return inserted == 1, nil
}

// ReleaseScheduleFiring drops a claim taken by ClaimScheduleFiring when the
// firing it guarded failed transiently, so another replica or a later attempt
// can retry rather than silently dropping the occurrence. The digest mirrors
// ClaimScheduleFiring exactly.
func (p *Persistence) ReleaseScheduleFiring(key string, minute time.Time) error {
	if p == nil {
		return fmt.Errorf("release scheduled workflow: persistence is disabled")
	}
	digest := sha256.Sum256([]byte(key + "\x00" + minute.UTC().Truncate(time.Minute).Format(time.RFC3339)))
	claimKey := hex.EncodeToString(digest[:])
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(`DELETE FROM schedule_claims WHERE key = ?`, claimKey)
	return err
}

func (p *Persistence) Delete(bucket, key string) error {
	if p == nil {
		return nil
	}
	return p.apply([]persistOp{{kind: persistOpDelete, bucket: bucket, key: key}})
}

const stateRevisionCounter = "state_revision"

func (p *Persistence) bumpStateRevisionTx(tx *sql.Tx) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO counters (name, value) VALUES (?, 1) ON CONFLICT(name) DO UPDATE SET value = counters.value + 1`, stateRevisionCounter); err != nil {
		return 0, err
	}
	var revision int64
	if err := tx.QueryRow(p.dialect.getSQL, stateRevisionCounter).Scan(&revision); err != nil {
		return 0, err
	}
	return revision, nil
}

func (p *Persistence) StateRevision() (int64, error) {
	return p.GetCounter(stateRevisionCounter)
}

func (p *Persistence) LocalRevision() int64 {
	if p == nil {
		return 0
	}
	return p.localRevision.Load()
}

func (p *Persistence) observeLocalRevision(revision int64) {
	observed := p.localRevision.Load()
	if revision == observed+1 {
		p.localRevision.Store(revision)
	}
	// A gap means another replica committed since this process last loaded
	// state. Do not bless the new revision merely because our write happened
	// to be last; the request middleware must reload the missing rows.
}

func (p *Persistence) List(bucket string) (map[string][]byte, error) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	rows, err := p.db.Query(p.dialect.listSQL, bucket)
	if err != nil {
		return nil, err
	}
	defer rows.Close() //nolint:errcheck
	out := map[string][]byte{}
	for rows.Next() {
		var k string
		var v []byte
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		plain, err := p.openValue(bucket, k, v)
		if err != nil {
			return nil, err
		}
		out[k] = plain
	}
	return out, rows.Err()
}

func (p *Persistence) Get(bucket, key string) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var raw []byte
	storageKey := p.storageKey(bucket, key)
	err := p.db.QueryRow(p.dialect.valueSQL, bucket, storageKey).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return p.openValue(bucket, storageKey, raw)
}

func (p *Persistence) GetCounter(name string) (int64, error) {
	if p == nil {
		return 0, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var v int64
	err := p.db.QueryRow(p.dialect.getSQL, name).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

func (p *Persistence) SetCounter(name string, value int64) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(p.dialect.setSQL, name, value)
	return err
}

// AllocateCounterValue atomically reserves one value from a durable sequence.
// minimum is the first value the caller is willing to accept. Unlike a
// GetCounter/SetCounter pair, the single upsert is safe when multiple dqlite
// clients allocate from the same sequence concurrently.
func (p *Persistence) AllocateCounterValue(name string, minimum int64) (int64, error) {
	if p == nil {
		return minimum, nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	var next int64
	err := p.db.QueryRow(
		`INSERT INTO counters (name, value) VALUES (?, ?)
		 ON CONFLICT(name) DO UPDATE SET value = MAX(counters.value, excluded.value - 1) + 1
		 RETURNING value`,
		name,
		minimum+1,
	).Scan(&next)
	if err != nil {
		return 0, err
	}
	return next - 1, nil
}

func (p *Persistence) Close() error {
	if p == nil {
		return nil
	}
	// A closed database cannot arbitrate git object locks; leaving it installed
	// would fail every ref update with "database is closed".
	gitObjectLockerMu.Lock()
	if gitObjectLockerV == gitObjectLocker(p) {
		gitObjectLockerV = nil
	}
	gitObjectLockerMu.Unlock()
	return p.db.Close()
}
