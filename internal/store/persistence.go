package store

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
	"github.com/e6qu/bleephub/internal/gitstore"
	zlog "github.com/rs/zerolog/log"
	_ "modernc.org/sqlite" // SQLite driver — pure Go, no CGO
)

type dbDialect struct {
	Name         string `json:"-"`
	PutSQL       string `json:"-"` // INSERT … ON CONFLICT upsert
	deleteSQL    string
	ListSQL      string `json:"-"`
	listRangeSQL string
	valueSQL     string
	getSQL       string
	setSQL       string
	raiseSQL     string // counter upsert that never lowers a stored value
	acquireSQL   string
	releaseSQL   string
	ReadVersion  string `json:"-"`
	WriteVersion string `json:"-"`
}

var (
	SqliteDialect = dbDialect{
		Name:         "sqlite",
		PutSQL:       `INSERT INTO kv (bucket, key, value) VALUES (?, ?, ?) ON CONFLICT(bucket, key) DO UPDATE SET value = excluded.value`,
		deleteSQL:    `DELETE FROM kv WHERE bucket = ? AND key = ?`,
		ListSQL:      `SELECT key, value FROM kv WHERE bucket = ?`,
		listRangeSQL: `SELECT key, value FROM kv WHERE bucket = ? AND key >= ? AND key < ?`,
		valueSQL:     `SELECT value FROM kv WHERE bucket = ? AND key = ?`,
		getSQL:       `SELECT value FROM counters WHERE name = ?`,
		setSQL:       `INSERT INTO counters (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = excluded.value`,
		raiseSQL:     `INSERT INTO counters (name, value) VALUES (?, ?) ON CONFLICT(name) DO UPDATE SET value = MAX(counters.value, excluded.value)`,
		acquireSQL:   `INSERT INTO locks (name, owner, expires_at) VALUES (?, ?, ?) ON CONFLICT(name) DO UPDATE SET owner = excluded.owner, expires_at = excluded.expires_at WHERE locks.expires_at <= ?`,
		releaseSQL:   `DELETE FROM locks WHERE name = ? AND owner = ?`,
		ReadVersion:  `SELECT value FROM schema_meta WHERE key = 'version'`,
		WriteVersion: `INSERT INTO schema_meta (key, value) VALUES ('version', ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
	}
)

// CurrentSchemaVersion is the schema this build writes and reads. Startup
// refuses a database stamped with a higher version.
const CurrentSchemaVersion = 3

const (
	PersistenceEncryptionKeyEnvironment = "BLEEPHUB_PERSISTENCE_ENCRYPTION_KEY"
	SealedPersistenceValuePrefix        = "bleephub:sealed:v1:"
	OpaquePersistenceKeyPrefix          = "hmac:v1:"
)

// sensitivePersistenceBuckets are sealed at the persistence boundary so no
// caller can write one as plaintext through Put, PutBatch, or a bespoke transaction.
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

// opaquePersistenceKeyBuckets key rows by bearer credentials. Store only a keyed
// digest of the key, so a database read discloses no credential while point
// lookup still works.
var opaquePersistenceKeyBuckets = map[string]struct{}{
	"installation_tokens":   {},
	"login_sessions":        {},
	"refresh_tokens":        {},
	"tokens":                {},
	"user_to_server_tokens": {},
}

// SchemaMetaDDL bootstraps the schema-version table. It predates versioning, so
// it is created unconditionally on every open.
const SchemaMetaDDL = `CREATE TABLE IF NOT EXISTS schema_meta (
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
	Db            *sql.DB    `json:"-"`
	Dialect       dbDialect  `json:"-"`
	Mu            sync.Mutex `json:"-"`
	encryptionKey []byte
	keyDigestKey  []byte
	localRevision atomic.Int64
	// keyHighWater caches the per-bucket identifier high-water mark this process
	// has written, so the common case costs no extra SQL.
	keyHighWater map[string]int64
}

func (p *Persistence) Ready(ctx context.Context) error {
	if p == nil {
		return nil
	}
	if err := p.Db.PingContext(ctx); err != nil {
		return fmt.Errorf("persistence ping: %w", err)
	}
	return nil
}

// PersistenceFailure is raised by the Must* helpers to abort the mid-write
// request rather than the process.
type PersistenceFailure struct {
	Op     string `json:"-"`
	Bucket string `json:"-"`
	Key    string `json:"-"`
	Err    error  `json:"-"`
}

func (e *PersistenceFailure) Error() string {
	return fmt.Sprintf("bleephub persistence %s %s/%s failed: %v", e.Op, e.Bucket, e.Key, e.Err)
}

func (e *PersistenceFailure) Unwrap() error { return e.Err }

type PersistencePut struct {
	Bucket string      `json:"-"`
	Key    string      `json:"-"`
	Value  interface{} `json:"-"`
}

type persistOpKind int

const (
	persistOpPut persistOpKind = iota
	persistOpDelete
)

type persistOp struct {
	kind   persistOpKind
	Bucket string `json:"-"`
	Key    string `json:"-"`
	raw    []byte
}

// PersistBatch accumulates one multi-step mutation's writes into a single
// transaction; a crash before Commit leaves the previous state intact.
type PersistBatch struct {
	p   *Persistence
	ops []persistOp
	Err error `json:"-"`
}

func NewPersistBatch(p *Persistence) *PersistBatch {
	return &PersistBatch{p: p}
}

func (b *PersistBatch) Put(bucket, key string, v interface{}) {
	if b.p == nil || b.Err != nil {
		return
	}
	raw, err := json.Marshal(v)
	if err != nil {
		b.Err = fmt.Errorf("marshal %s/%s: %w", bucket, key, err)
		return
	}
	b.ops = append(b.ops, persistOp{kind: persistOpPut, Bucket: bucket, Key: key, raw: raw})
}

func (b *PersistBatch) Delete(bucket, key string) {
	if b.p == nil || b.Err != nil {
		return
	}
	b.ops = append(b.ops, persistOp{kind: persistOpDelete, Bucket: bucket, Key: key})
}

func (b *PersistBatch) Commit() error {
	if b.Err != nil {
		return b.Err
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
	p.Mu.Lock()
	defer p.Mu.Unlock()
	tx, err := p.Db.Begin()
	if err != nil {
		return err
	}
	raised := map[string]int64{}
	for _, op := range ops {
		storageKey := p.StorageKey(op.Bucket, op.Key)
		switch op.kind {
		case persistOpPut:
			if err := p.raiseKeyHighWaterTx(tx, op.Bucket, storageKey, raised); err != nil {
				_ = tx.Rollback()
				return err
			}
			raw, err := p.sealValue(op.Bucket, storageKey, op.raw)
			if err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.Exec(p.Dialect.PutSQL, op.Bucket, storageKey, raw); err != nil {
				_ = tx.Rollback()
				return err
			}
		case persistOpDelete:
			if _, err := tx.Exec(p.Dialect.deleteSQL, op.Bucket, storageKey); err != nil {
				_ = tx.Rollback()
				return err
			}
		default:
			_ = tx.Rollback()
			return fmt.Errorf("unknown persistence operation %d for %s/%s", op.kind, op.Bucket, op.Key)
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

// PutBatch commits related records in one transaction. Callers update their
// in-memory indexes only after it returns successfully.
func (p *Persistence) PutBatch(entries ...PersistencePut) error {
	if p == nil {
		return nil
	}
	batch := NewPersistBatch(p)
	for _, entry := range entries {
		batch.Put(entry.Bucket, entry.Key, entry.Value)
	}
	return batch.Commit()
}

func (p *Persistence) DeleteBatch(entries ...PersistencePut) error {
	if p == nil {
		return nil
	}
	batch := NewPersistBatch(p)
	for _, entry := range entries {
		batch.Delete(entry.Bucket, entry.Key)
	}
	return batch.Commit()
}

// ClaimOIDCLogoutAndDeleteSessions stores a replay marker and deletes the
// matching browser sessions in one transaction. The kv primary key makes the
// claim exclusive across processes and replicas.
func (p *Persistence) ClaimOIDCLogoutAndDeleteSessions(replayKey string, expiresAt, now time.Time, provider, issuer, sid, subject string) (bool, error) {
	marker, err := json.Marshal(oidcLogoutReplayMarker{ExpiresAt: expiresAt})
	if err != nil {
		return false, fmt.Errorf("marshal OpenID Connect logout replay marker: %w", err)
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	tx, err := p.Db.Begin()
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
		if err := tx.QueryRow(p.Dialect.valueSQL, oidcLogoutClaimsBucket, replayKey).Scan(&raw); err != nil {
			return false, err
		}
		var persisted oidcLogoutReplayMarker
		if err := json.Unmarshal(raw, &persisted); err != nil {
			return false, fmt.Errorf("decode OpenID Connect logout replay marker %s: %w", replayKey, err)
		}
		if persisted.ExpiresAt.After(now) {
			return false, nil
		}
		if _, err := tx.Exec(p.Dialect.deleteSQL, oidcLogoutClaimsBucket, replayKey); err != nil {
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

	rows, err := tx.Query(p.Dialect.ListSQL, oidcLogoutClaimsBucket)
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
		if _, err := tx.Exec(p.Dialect.deleteSQL, oidcLogoutClaimsBucket, entry.key); err != nil {
			return false, err
		}
	}

	rows, err = tx.Query(p.Dialect.ListSQL, LoginSessionsBucket)
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
		raw, err = p.openValue(LoginSessionsBucket, id, raw)
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
		if _, err := tx.Exec(p.Dialect.deleteSQL, LoginSessionsBucket, id); err != nil {
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

func OpenSQLite(dataDir string) (*sql.DB, error) {
	// #nosec G703 -- dataDir is deployment configuration, not request input;
	// the only appended component is the fixed database filename.
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "bleephub.db")
	// synchronous(FULL) fsyncs the WAL at every commit, so an acknowledged write
	// survives OS crash or power loss, not only process crash (NORMAL's
	// guarantee). dqlite gets durability from Raft and does not use this path.
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite %s: %w", dbPath, err)
	}
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping sqlite %s: %w", dbPath, err)
	}
	return db, nil
}

// OpenDqlite connects to the dqlite quorum from a seed set of private addresses;
// the driver discovers the leader and refreshes membership from the quorum.
func OpenDqlite(addresses string) (*sql.DB, error) {
	addressMap, err := dqliteaddr.FromEnvironment(os.Getenv(dqliteaddr.Environment))
	if err != nil {
		return nil, PermanentErrf("parse dqlite address map: %w", err)
	}
	secret := strings.TrimSpace(os.Getenv(dqliteaddr.SecretEnvironment))
	if secret == "" {
		return nil, PermanentErrf("%s is required: it authenticates and encrypts the dqlite transport carrying statements against the whole database", dqliteaddr.SecretEnvironment)
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
		return nil, PermanentErrf("BLEEPHUB_DQLITE_SERVERS must contain at least one dqlite server address")
	}
	if err := store.Set(context.Background(), servers); err != nil {
		return nil, PermanentErrf("configure dqlite server set: %w", err)
	}

	dqliteDriver, err := driver.New(store,
		driver.WithDialFunc(DqliteDialer(addressMap, secret)),
		driver.WithAttemptTimeout(5*time.Second),
		driver.WithConnectionBackoffFactor(100*time.Millisecond),
		driver.WithConnectionBackoffCap(time.Second),
		driver.WithRetryLimit(12),
	)
	if err != nil {
		return nil, PermanentErrf("create dqlite driver: %w", err)
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

// DqliteDialer binds member address resolution and the cluster credential to
// the driver's transport.
func DqliteDialer(addresses dqliteaddr.Map, secret string) client.DialFunc {
	return func(ctx context.Context, address string) (net.Conn, error) {
		return DqliteHTTPDial(ctx, addresses.Resolve(address), secret)
	}
}

// DqliteHTTPDial opens the dqlite HTTP-upgrade transport at a private member
// address. The upgrade keeps the wire protocol private while letting ECS tasks
// keep stable advertised identities across replacement and scale-to-zero.
func DqliteHTTPDial(ctx context.Context, address, secret string) (net.Conn, error) {
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
		return nil, PermanentErrf("BLEEPHUB_DATABASE_URL is no longer supported; bleephub stores its own state in SQLite via BLEEPHUB_PERSIST=true and BLEEPHUB_DATA_DIR")
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
		db, err = OpenDqlite(addresses)
	} else {
		db, err = OpenSQLite(dataDir)
	}
	if err != nil {
		return nil, err
	}
	dialect := SqliteDialect
	if os.Getenv("BLEEPHUB_DQLITE_SERVERS") != "" {
		dialect.Name = "dqlite"
	}
	if err := migrateSchema(db, dialect); err != nil {
		_ = db.Close()
		return nil, err
	}
	p := &Persistence{
		Db:            db,
		Dialect:       dialect,
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
	encoded := strings.TrimSpace(os.Getenv(PersistenceEncryptionKeyEnvironment))
	if encoded == "" {
		return nil, fmt.Errorf("%s is required when BLEEPHUB_PERSIST=true; provide a stable base64-encoded 32-byte key", PersistenceEncryptionKeyEnvironment)
	}
	key, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("%s must be a base64-encoded 32-byte key", PersistenceEncryptionKeyEnvironment)
	}
	return key, nil
}

func derivePersistenceKey(master []byte, purpose string) []byte {
	mac := hmac.New(sha256.New, master)
	_, _ = mac.Write([]byte("bleephub:persistence:v1:" + purpose))
	return mac.Sum(nil)
}

func IsSensitivePersistenceBucket(bucket string) bool {
	_, ok := sensitivePersistenceBuckets[bucket]
	return ok
}

func isOpaquePersistenceKeyBucket(bucket string) bool {
	_, ok := opaquePersistenceKeyBuckets[bucket]
	return ok
}

func (p *Persistence) StorageKey(bucket, key string) string {
	if !isOpaquePersistenceKeyBucket(bucket) || strings.HasPrefix(key, OpaquePersistenceKeyPrefix) {
		return key
	}
	return p.opaqueDigestKey(bucket, key)
}

// opaqueLookupKey derives the storage key for a client-presented value (bearer
// token or session id). Unlike StorageKey it never passes an already
// "hmac:v1:"-prefixed value through, so a leaked row key cannot be replayed as
// the credential itself. A non-opaque bucket keeps the raw value.
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
	return OpaquePersistenceKeyPrefix + hex.EncodeToString(mac.Sum(nil))
}

func persistenceAssociatedData(bucket, key string) []byte {
	return []byte(bucket + "\x00" + key)
}

func (p *Persistence) sealValue(bucket, key string, raw []byte) ([]byte, error) {
	if !IsSensitivePersistenceBucket(bucket) {
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
	return []byte(SealedPersistenceValuePrefix + encoded), nil
}

func (p *Persistence) openValue(bucket, key string, raw []byte) ([]byte, error) {
	if !IsSensitivePersistenceBucket(bucket) {
		return raw, nil
	}
	if !strings.HasPrefix(string(raw), SealedPersistenceValuePrefix) {
		return raw, nil // legacy plaintext; migrateSensitiveRows rewrites it
	}
	envelope, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(string(raw), SealedPersistenceValuePrefix))
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

// migrateSensitiveRows encrypts legacy plaintext values and rekeys raw
// bearer-token keys to digests in one transaction. Decrypting every envelope on
// the way makes a wrong deployment key fail fast rather than lose data later.
func (p *Persistence) migrateSensitiveRows() error {
	p.Mu.Lock()
	defer p.Mu.Unlock()
	tx, err := p.Db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for bucket := range sensitivePersistenceBuckets {
		rows, err := tx.Query(p.Dialect.ListSQL, bucket)
		if err != nil {
			return fmt.Errorf("scan sensitive persistence bucket %s: %w", bucket, err)
		}
		type row struct {
			Key string `json:"-"`
			raw []byte
		}
		var entries []row
		for rows.Next() {
			var entry row
			if err := rows.Scan(&entry.Key, &entry.raw); err != nil {
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
			plain, err := p.openValue(bucket, entry.Key, entry.raw)
			if err != nil {
				return err
			}
			storageKey := p.StorageKey(bucket, entry.Key)
			alreadySealed := strings.HasPrefix(string(entry.raw), SealedPersistenceValuePrefix)
			if alreadySealed && storageKey == entry.Key {
				continue
			}
			sealed, err := p.sealValue(bucket, storageKey, plain)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(p.Dialect.PutSQL, bucket, storageKey, sealed); err != nil {
				return err
			}
			if storageKey != entry.Key {
				if _, err := tx.Exec(p.Dialect.deleteSQL, bucket, entry.Key); err != nil {
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

// migrateSchema brings the database up to CurrentSchemaVersion and refuses one
// written by a newer build, whose row layout this build would silently
// mis-decode.
func migrateSchema(db *sql.DB, dialect dbDialect) error {
	if _, err := db.Exec(SchemaMetaDDL); err != nil {
		return fmt.Errorf("persistence schema metadata: %w", err)
	}
	var stored string
	err := db.QueryRow(dialect.ReadVersion).Scan(&stored)
	switch {
	case err == sql.ErrNoRows:
		stored = "0"
	case err != nil:
		return fmt.Errorf("read persistence schema version: %w", err)
	}
	version, err := strconv.Atoi(strings.TrimSpace(stored))
	if err != nil {
		return PermanentErrf("persistence schema version %q is not a number", stored)
	}
	if version > CurrentSchemaVersion {
		return PermanentErrf("persistence schema version %d was written by a newer bleephub; this build understands up to version %d", version, CurrentSchemaVersion)
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
	if _, err := db.Exec(dialect.WriteVersion, strconv.Itoa(CurrentSchemaVersion)); err != nil {
		return fmt.Errorf("stamp persistence schema version: %w", err)
	}
	return nil
}

// permanentPersistenceError marks an init failure no retry can fix (a
// misconfiguration or a newer-bleephub schema), distinct from the transient
// failures while a quorum forms. MustNewPersistence fails fast on it.
type permanentPersistenceError struct{ err error }

func (e *permanentPersistenceError) Error() string { return e.err.Error() }
func (e *permanentPersistenceError) Unwrap() error { return e.err }

// PermanentErrf builds a permanent persistence error.
func PermanentErrf(format string, args ...any) error {
	return &permanentPersistenceError{err: fmt.Errorf(format, args...)}
}

// IsPermanentPersistenceError reports whether err (or anything it wraps) is a
// permanentPersistenceError.
func IsPermanentPersistenceError(err error) bool {
	var p *permanentPersistenceError
	return errors.As(err, &p)
}

// startupQuorumDeadline bounds MustNewPersistence's retry of transient dqlite
// failures. Quorum forms in seconds, so a wait this long means a downed peer or
// misconfiguration; failing loudly beats hanging before the listener starts.
const startupQuorumDeadline = 5 * time.Minute

func MustNewPersistence() *Persistence {
	// Honour SIGTERM during the wait: this runs before the HTTP listener exists,
	// so without it an orchestrator can only SIGKILL a still-starting process.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	giveUpAt := time.Now().Add(startupQuorumDeadline)
	for {
		p, err := NewPersistence()
		if err == nil {
			return p
		}
		// Only transient failures (dial/ping/query while the quorum forms) are
		// worth retrying; a permanent one never resolves by waiting.
		if IsPermanentPersistenceError(err) {
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

// MustPut writes a record or panics; the server's recovery middleware turns the
// panic into a 500 and the caller's deferred unlocks release the store lock.
func (p *Persistence) MustPut(bucket, key string, v interface{}) {
	if err := p.Put(bucket, key, v); err != nil {
		panic(&PersistenceFailure{Op: "write", Bucket: bucket, Key: key, Err: err})
	}
}

func (p *Persistence) MustDelete(bucket, key string) {
	if err := p.Delete(bucket, key); err != nil {
		panic(&PersistenceFailure{Op: "delete", Bucket: bucket, Key: key, Err: err})
	}
}

func (p *Persistence) Put(bucket, key string, v interface{}) error {
	if p == nil {
		return nil
	}
	batch := NewPersistBatch(p)
	batch.Put(bucket, key, v)
	return batch.Commit()
}

// OwnedExclusively reports whether this process is the only writer: true for a
// local SQLite file, false for a shared dqlite quorum this process must not
// rewrite at startup.
func (p *Persistence) OwnedExclusively() bool {
	if p == nil {
		return true
	}
	return p.Dialect.Name == "sqlite"
}

// bucketKeyCounter names the high-water counter for an identifier-keyed bucket.
func bucketKeyCounter(bucket string) string { return "kv_max_key:" + bucket }

// keyHighWaterCandidate reports the next free identifier implied by a key, or
// false for a name-keyed bucket.
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
	if _, err := tx.Exec(p.Dialect.raiseSQL, bucketKeyCounter(bucket), next); err != nil {
		return err
	}
	raised[bucket] = next
	return nil
}

// KeyHighWater returns one past the highest identifier ever written to a bucket.
// Loaders max it with surviving rows so a deleted entity's id — also its
// object-store key for attestations, package files and artifacts — is never reused.
func (p *Persistence) KeyHighWater(bucket string) (int64, error) {
	if p == nil {
		return 0, nil
	}
	return p.GetCounter(bucketKeyCounter(bucket))
}

// AcquireLock takes the named lock for owner until ttl elapses, returning false
// when another owner holds it. The expiry frees a lock stranded by a dead replica.
func (p *Persistence) AcquireLock(name, owner string, ttl time.Duration) (bool, error) {
	if p == nil {
		return false, fmt.Errorf("acquire lock %s: persistence is disabled", name)
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	now := time.Now().UnixNano()
	result, err := p.Db.Exec(p.Dialect.acquireSQL, name, owner, now+ttl.Nanoseconds(), now)
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
	p.Mu.Lock()
	defer p.Mu.Unlock()
	_, err := p.Db.Exec(p.Dialect.releaseSQL, name, owner)
	return err
}

// ClaimScheduleFiring atomically selects one replica for a cron tuple/minute.
// Claims stay outside the metadata revision feed: they coordinate a Workflow
// row's creation but are not themselves API state.
// ClaimScheduleFiring claims a firing for a schedule at minute, honoring
// GitHub's per-schedule minimum interval: the claim succeeds only if the
// schedule has no prior firing or its last firing is at least minInterval old.
// One row per schedule holds the last firing time and is updated in place, so
// this doubles as the exact-minute dedup for ticker drift and catch-up replays.
func (p *Persistence) ClaimScheduleFiring(key string, minute time.Time, minInterval time.Duration) (bool, error) {
	if p == nil {
		return false, fmt.Errorf("claim scheduled workflow: persistence is disabled")
	}
	digest := sha256.Sum256([]byte(key))
	claimKey := hex.EncodeToString(digest[:])
	at := minute.UTC().Truncate(time.Minute).Unix()
	gate := int64(minInterval / time.Second)
	p.Mu.Lock()
	defer p.Mu.Unlock()
	// One atomic statement: insert a new schedule's first firing, or advance the
	// last-firing time only when at least the gate has elapsed. A row is affected
	// (RowsAffected == 1) exactly when this call claimed the firing; a conflicting
	// insert within the interval — a concurrent replica, a replayed minute, or a
	// sub-interval cron tick — updates nothing (RowsAffected == 0).
	res, err := p.Db.Exec(
		`INSERT INTO schedule_claims (key, scheduled_at) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET scheduled_at = excluded.scheduled_at
		 WHERE excluded.scheduled_at - schedule_claims.scheduled_at >= ?`,
		claimKey, at, gate)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, nil
}

// ReleaseScheduleFiring reverts a ClaimScheduleFiring whose firing failed
// transiently, so the occurrence can be retried. It removes the schedule's row
// only while this minute is still the one recorded (we are the latest claimant).
func (p *Persistence) ReleaseScheduleFiring(key string, minute time.Time) error {
	if p == nil {
		return fmt.Errorf("release scheduled workflow: persistence is disabled")
	}
	digest := sha256.Sum256([]byte(key))
	claimKey := hex.EncodeToString(digest[:])
	at := minute.UTC().Truncate(time.Minute).Unix()
	p.Mu.Lock()
	defer p.Mu.Unlock()
	_, err := p.Db.Exec(`DELETE FROM schedule_claims WHERE key = ? AND scheduled_at = ?`, claimKey, at)
	return err
}

func (p *Persistence) Delete(bucket, key string) error {
	if p == nil {
		return nil
	}
	return p.apply([]persistOp{{kind: persistOpDelete, Bucket: bucket, Key: key}})
}

const stateRevisionCounter = "state_revision"

func (p *Persistence) bumpStateRevisionTx(tx *sql.Tx) (int64, error) {
	if _, err := tx.Exec(`INSERT INTO counters (name, value) VALUES (?, 1) ON CONFLICT(name) DO UPDATE SET value = counters.value + 1`, stateRevisionCounter); err != nil {
		return 0, err
	}
	var revision int64
	if err := tx.QueryRow(p.Dialect.getSQL, stateRevisionCounter).Scan(&revision); err != nil {
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
	// A gap means another replica committed since this process last loaded state;
	// leave the revision so the request middleware reloads the missing rows.
}

func (p *Persistence) List(bucket string) (map[string][]byte, error) {
	if p == nil {
		return nil, nil
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	rows, err := p.Db.Query(p.Dialect.ListSQL, bucket)
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

// ListPrefix returns rows whose key begins with prefix via an indexed range
// scan `[prefix, prefixSuccessor)`, never a whole-bucket scan, for composite-key
// secondary indexes (e.g. login sessions keyed by user). The bucket must not be
// sensitive: a range scan cannot recover a per-row opaque storage key.
func (p *Persistence) ListPrefix(bucket, prefix string) (map[string][]byte, error) {
	if p == nil {
		return nil, nil
	}
	hi, ok := prefixUpperBound(prefix)
	if !ok {
		// An all-0xff prefix has no finite successor; fall back to a filtered
		// full-bucket list. Never arises for the numeric grouping prefixes here.
		all, err := p.List(bucket)
		if err != nil {
			return nil, err
		}
		for k := range all {
			if !strings.HasPrefix(k, prefix) {
				delete(all, k)
			}
		}
		return all, nil
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	rows, err := p.Db.Query(p.Dialect.listRangeSQL, bucket, prefix, hi)
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

// prefixUpperBound returns the exclusive upper bound for keys starting with
// prefix (last byte incremented), or false when the prefix is empty or all 0xff.
func prefixUpperBound(prefix string) (string, bool) {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] != 0xff {
			b[i]++
			return string(b[:i+1]), true
		}
	}
	return "", false
}

func (p *Persistence) Get(bucket, key string) ([]byte, error) {
	if p == nil {
		return nil, nil
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var raw []byte
	storageKey := p.StorageKey(bucket, key)
	err := p.Db.QueryRow(p.Dialect.valueSQL, bucket, storageKey).Scan(&raw)
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
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var v int64
	err := p.Db.QueryRow(p.Dialect.getSQL, name).Scan(&v)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return v, err
}

func (p *Persistence) SetCounter(name string, value int64) error {
	if p == nil {
		return nil
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	_, err := p.Db.Exec(p.Dialect.setSQL, name, value)
	return err
}

// AllocateCounterValue atomically reserves one value (>= minimum) from a durable
// sequence. The single upsert is safe under concurrent dqlite allocators, unlike
// a GetCounter/SetCounter pair.
func (p *Persistence) AllocateCounterValue(name string, minimum int64) (int64, error) {
	if p == nil {
		return minimum, nil
	}
	p.Mu.Lock()
	defer p.Mu.Unlock()
	var next int64
	err := p.Db.QueryRow(
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
	// fails every ref update with "database is closed".
	gitstore.ClearGitObjectLocker(p)
	return p.Db.Close()
}
