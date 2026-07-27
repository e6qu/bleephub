package bleephub

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/canonical/go-dqlite/v3/client"
	"github.com/canonical/go-dqlite/v3/driver"
	"github.com/e6qu/bleephub/internal/dqliteaddr"
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
const currentSchemaVersion = 2

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
}

type Persistence struct {
	db      *sql.DB
	dialect dbDialect
	mu      sync.Mutex
	// keyHighWater caches the durable per-bucket identifier high-water mark
	// already written by this process, so the common case costs no extra SQL.
	keyHighWater map[string]int64
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
		switch op.kind {
		case persistOpPut:
			if err := p.raiseKeyHighWaterTx(tx, op.bucket, op.key, raised); err != nil {
				_ = tx.Rollback()
				return err
			}
			if _, err := tx.Exec(p.dialect.putSQL, op.bucket, op.key, op.raw); err != nil {
				_ = tx.Rollback()
				return err
			}
		case persistOpDelete:
			if _, err := tx.Exec(p.dialect.deleteSQL, op.bucket, op.key); err != nil {
				_ = tx.Rollback()
				return err
			}
		default:
			_ = tx.Rollback()
			return fmt.Errorf("unknown persistence operation %d for %s/%s", op.kind, op.bucket, op.key)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
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
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func openSQLite(dataDir string) (*sql.DB, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dataDir, err)
	}
	dbPath := filepath.Join(dataDir, "bleephub.db")
	db, err := sql.Open("sqlite", "file:"+dbPath+"?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
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
		return nil, fmt.Errorf("parse dqlite address map: %w", err)
	}
	secret := strings.TrimSpace(os.Getenv(dqliteaddr.SecretEnvironment))
	if secret == "" {
		return nil, fmt.Errorf("%s is required: the dqlite transport carries unauthenticated statements against the whole database", dqliteaddr.SecretEnvironment)
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
		return nil, fmt.Errorf("BLEEPHUB_DQLITE_SERVERS must contain at least one dqlite server address")
	}
	if err := store.Set(context.Background(), servers); err != nil {
		return nil, fmt.Errorf("configure dqlite server set: %w", err)
	}

	dqliteDriver, err := driver.New(store,
		driver.WithDialFunc(func(ctx context.Context, address string) (net.Conn, error) {
			return dqliteHTTPDial(ctx, addressMap.Resolve(address), secret)
		}),
		driver.WithAttemptTimeout(5*time.Second),
		driver.WithConnectionBackoffFactor(100*time.Millisecond),
		driver.WithConnectionBackoffCap(time.Second),
		driver.WithRetryLimit(12),
	)
	if err != nil {
		return nil, fmt.Errorf("create dqlite driver: %w", err)
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

// dqliteHTTPDial opens the dqlite HTTP-upgrade transport exposed by each
// private Cloud Map member address. The upgrade keeps the dqlite wire protocol
// private while allowing Amazon ECS tasks to retain stable advertised member
// identities across replacement and scale-to-zero restarts.
func dqliteHTTPDial(ctx context.Context, address, secret string) (net.Conn, error) {
	dialer := &net.Dialer{}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, fmt.Errorf("dial dqlite %s: %w", address, err)
	}

	requestURL, err := url.Parse("http://" + address + "/dqlite")
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
		return nil, fmt.Errorf("BLEEPHUB_DATABASE_URL is no longer supported; bleephub stores its own state in SQLite via BLEEPHUB_PERSIST=true and BLEEPHUB_DATA_DIR")
	}

	if os.Getenv("BLEEPHUB_PERSIST") != "true" {
		return nil, nil //nolint:nilnil // intentional: nil persistence = disabled
	}

	dataDir := os.Getenv("BLEEPHUB_DATA_DIR")
	if dataDir == "" {
		dataDir = "."
	}

	var (
		db  *sql.DB
		err error
	)
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
	return &Persistence{db: db, dialect: dialect, keyHighWater: map[string]int64{}}, nil
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
		return fmt.Errorf("persistence schema version %q is not a number", stored)
	}
	if version > currentSchemaVersion {
		return fmt.Errorf("persistence schema version %d was written by a newer bleephub; this build understands up to version %d", version, currentSchemaVersion)
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

func MustNewPersistence() *Persistence {
	for {
		p, err := NewPersistence()
		if err == nil {
			return p
		}
		if strings.TrimSpace(os.Getenv("BLEEPHUB_DQLITE_SERVERS")) == "" {
			log.Fatalf("bleephub persistence configuration failed: %v", err)
		}
		log.Printf("bleephub is waiting for dqlite quorum: %v", err)
		time.Sleep(time.Second)
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
	raw, err := json.Marshal(v)
	if err != nil {
		return fmt.Errorf("marshal %s/%s: %w", bucket, key, err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	next, raise := keyHighWaterCandidate(key)
	if raise && p.keyHighWater[bucket] < next {
		// Written before the row so an interrupted pair can only leave the
		// counter ahead of reality, which skips identifiers instead of reusing
		// one whose object bytes still exist.
		if _, err := p.db.Exec(p.dialect.raiseSQL, bucketKeyCounter(bucket), next); err != nil {
			return err
		}
		p.cacheKeyHighWater(bucket, next)
	}
	_, err = p.db.Exec(p.dialect.putSQL, bucket, key, raw)
	return err
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

func (p *Persistence) Delete(bucket, key string) error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	_, err := p.db.Exec(p.dialect.deleteSQL, bucket, key)
	return err
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
		out[k] = v
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
	err := p.db.QueryRow(p.dialect.valueSQL, bucket, key).Scan(&raw)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return raw, nil
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
