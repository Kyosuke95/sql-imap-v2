package imapsql

import (
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	mathrand "math/rand"
	"strings"
	"sync"
	"time"

	imap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend"
	"github.com/pkg/errors"
	"golang.org/x/crypto/sha3"
)

var (
	ErrUserAlreadyExists = errors.New("imap: user already exists")
	ErrUserDoesntExists  = errors.New("imap: user doesn't exists")
)

type Rand interface {
	Uint32() uint32
}

// Opts structure specifies additional settings that may be set
// for backend.
//
// Please use names to reference structure members on creation,
// fields may be reordered or added without major version increment.
type Opts struct {
	// Maximum amount of bytes that backend will accept.
	// Intended for use with APPENDLIMIT extension.
	// nil value means no limit, 0 means zero limit (no new messages allowed)
	MaxMsgBytes *uint32

	// Controls when channel returned by Updates should be created.
	// If set to false - channel will be created before NewBackend returns.
	// If set to true - channel will be created upon first call to Updates.
	// Second is useful for tests that don't consume values from Updates
	// channel.
	LazyUpdatesInit bool

	// UpdatesChan allows to pass custom channel object used for unilateral
	// updates dispatching.
	//
	// You can use this to change default updates buffer size (20) or to split
	// initializaton into phases (which allows to break circular dependencies
	// if you need updates channel before database initialization).
	UpdatesChan chan backend.Update

	// Custom randomness source for UIDVALIDITY values generation.
	PRNG Rand
}

type Backend struct {
	db   db
	opts Opts

	childrenExt bool

	prng Rand

	updates chan backend.Update
	// updates channel is lazily initalized, so we need to ensure thread-safety.
	updatesLck sync.Mutex

	// Shitton of pre-compiled SQL statements.
	userCreds          *sql.Stmt
	listUsers          *sql.Stmt
	addUser            *sql.Stmt
	delUser            *sql.Stmt
	setUserPass        *sql.Stmt
	listMboxes         *sql.Stmt
	listSubbedMboxes   *sql.Stmt
	createMboxExistsOk *sql.Stmt
	createMbox         *sql.Stmt
	deleteMbox         *sql.Stmt
	renameMbox         *sql.Stmt
	renameMboxChilds   *sql.Stmt
	getMboxMark        *sql.Stmt
	setSubbed          *sql.Stmt
	uidNext            *sql.Stmt
	addUidNext         *sql.Stmt
	hasChildren        *sql.Stmt
	uidValidity        *sql.Stmt
	msgsCount          *sql.Stmt
	recentCount        *sql.Stmt
	firstUnseenSeqNum  *sql.Stmt
	deletedSeqnums     *sql.Stmt
	expungeMbox        *sql.Stmt
	mboxId             *sql.Stmt
	addMsg             *sql.Stmt
	copyMsgsUid        *sql.Stmt
	copyMsgFlagsUid    *sql.Stmt
	copyMsgsSeq        *sql.Stmt
	copyMsgFlagsSeq    *sql.Stmt
	massClearFlagsUid  *sql.Stmt
	massClearFlagsSeq  *sql.Stmt
	msgFlagsUid        *sql.Stmt
	msgFlagsSeq        *sql.Stmt
	usedFlags          *sql.Stmt

	addRecentToLast *sql.Stmt

	// 'mark' column for messages is used to keep track of messages selected
	// by sequence numbers during operations that may cause seqence numbers to
	// change (e.g. message deletion)
	//
	// Consider following request: Delete messages with seqnum 1 and 3.
	// Naive implementation will delete 1st and then 3rd messages in mailbox.
	// However, after first operation 3rd message will become 2nd and
	// code will end up deleting the wrong message (4th actually).
	//
	// Solution is to "mark" 1st and 3rd message and then delete all "marked"
	// message.
	//
	// One could use \Deleted flag for this purpose, but this
	// requires more expensive operations at SQL engine side, so 'mark' column
	// is basically a optimization.

	// For MOVE extension
	markUid   *sql.Stmt
	markSeq   *sql.Stmt
	delMarked *sql.Stmt

	markedSeqnums *sql.Stmt

	// For APPEND-LIMIT extension
	setUserMsgSizeLimit *sql.Stmt
	userMsgSizeLimit    *sql.Stmt
	setMboxMsgSizeLimit *sql.Stmt
	mboxMsgSizeLimit    *sql.Stmt

	fetchStmtsCache map[string]*sql.Stmt
}

func NewBackend(driver, dsn string, opts Opts) (*Backend, error) {
	b := &Backend{
		fetchStmtsCache: make(map[string]*sql.Stmt),
	}
	var err error

	b.opts = opts
	if !b.opts.LazyUpdatesInit {
		b.updates = b.opts.UpdatesChan
		if b.updates == nil {
			b.updates = make(chan backend.Update, 20)
		}
	}

	if b.opts.PRNG != nil {
		b.prng = opts.PRNG
	} else {
		b.prng = mathrand.New(mathrand.NewSource(time.Now().Unix()))
	}

	if driver == "sqlite3" {
		if !strings.HasPrefix(dsn, "file:") {
			dsn = "file:" + dsn
		}
		if !strings.Contains(dsn, "?") {
			dsn = dsn + "?"
		}

		dsn = dsn + "_journal=WAL&_busy_timeout=5000"
	}

	b.db.driver = driver

	b.db.DB, err = sql.Open(driver, dsn)
	if err != nil {
		return nil, errors.Wrap(err, "NewBackend")
	}

	if driver == "sqlite3" {
		if dsn == "file::memory:?_journal=WAL&_busy_timeout=5000" {
			b.db.DB.SetMaxOpenConns(1)
		}

		_, err := b.db.Exec(`PRAGMA foreign_keys = ON`)
		if err != nil {
			return nil, errors.Wrap(err, "NewBackend")
		}
	} else if driver == "mysql" {
		_, err := b.db.Exec(`SET SESSION sql_mode = 'ansi,no_backslash_escapes'`)
		if err != nil {
			return nil, errors.Wrap(err, "NewBackend")
		}
		_, err = b.db.Exec(`SET SESSION TRANSACTION ISOLATION LEVEL SERIALIZABLE`)
		if err != nil {
			return nil, errors.Wrap(err, "NewBackend")
		}
	}

	if err := b.initSchema(); err != nil {
		return nil, errors.Wrap(err, "NewBackend")
	}
	if err := b.prepareStmts(); err != nil {
		return nil, errors.Wrap(err, "NewBackend")
	}

	for _, item := range [...]imap.FetchItem{
		imap.FetchFlags, imap.FetchEnvelope,
		imap.FetchBodyStructure, "BODY[]", "BODY[HEADER.FIELDS (From To)]"} {

		if _, err := b.getFetchStmt(true, []imap.FetchItem{item}); err != nil {
			return nil, errors.Wrapf(err, "fetchStmt prime (%s, uid=true)", item)
		}
		if _, err := b.getFetchStmt(false, []imap.FetchItem{item}); err != nil {
			return nil, errors.Wrapf(err, "fetchStmt prime (%s, uid=false)", item)
		}
	}

	return b, nil
}

func (b *Backend) EnableChildrenExt() bool {
	b.childrenExt = true
	return true
}

func (b *Backend) Close() error {
	return b.db.Close()
}

func (b *Backend) Updates() <-chan backend.Update {
	if b.opts.LazyUpdatesInit && b.updates == nil {
		b.updatesLck.Lock()
		defer b.updatesLck.Unlock()

		if b.updates == nil {
			b.updates = make(chan backend.Update, 20)
		}
	}
	return b.updates
}

func (b *Backend) groupConcatFn(expr, separator string) string {
	if b.db.driver == "sqlite3" {
		return "group_concat(" + expr + ", '" + separator + "')"
	}
	if b.db.driver == "postgres" {
		return "string_agg(" + expr + ", '" + separator + "')"
	}
	if b.db.driver == "mysql" {
		return "group_concat(" + expr + " SEPARATOR '" + separator + "')"
	}
	panic("Unsupported driver")
}

func (b *Backend) UserCreds(username string) (uint64, []byte, []byte, error) {
	row := b.userCreds.QueryRow(username)
	id, passHashHex, passSaltHex := uint64(0), "", ""
	if err := row.Scan(&id, &passHashHex, &passSaltHex); err != nil {
		return 0, nil, nil, err
	}

	passHash, err := hex.DecodeString(passHashHex)
	if err != nil {
		return 0, nil, nil, err
	}
	passSalt, err := hex.DecodeString(passSaltHex)
	if err != nil {
		return 0, nil, nil, err
	}

	return id, passHash, passSalt, nil
}

func (b *Backend) CreateUser(username, password string) error {
	salt := make([]byte, 16)
	if n, err := rand.Read(salt); err != nil {
		return errors.Wrap(err, "CreateUser")
	} else if n != 16 {
		return errors.New("CreateUser: failed to read enough entropy for salt from CSPRNG")
	}

	pass := make([]byte, 0, len(password)+len(salt))
	pass = append(pass, []byte(password)...)
	pass = append(pass, salt...)
	digest := sha3.Sum512(pass)

	_, err := b.addUser.Exec(username, hex.EncodeToString(digest[:]), hex.EncodeToString(salt))
	if err != nil && (strings.Contains(err.Error(), "UNIQUE") || strings.Contains(err.Error(), "Duplicate entry") || strings.Contains(err.Error(), "unique")) {
		return ErrUserAlreadyExists
	}
	return errors.Wrap(err, "CreateUser")
}

func (b *Backend) DeleteUser(username string) error {
	stats, err := b.delUser.Exec(username)
	if err != nil {
		return errors.Wrap(err, "DeleteUser")
	}
	affected, err := stats.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "SetUserPassword")
	}
	if affected == 0 {
		return ErrUserDoesntExists
	}
	return nil
}

func (b *Backend) SetUserPassword(username, newPassword string) error {
	salt := make([]byte, 16)
	if n, err := rand.Read(salt); err != nil {
		return errors.Wrap(err, "SetUserPassword")
	} else if n != 16 {
		return errors.New("SetUserPassword: failed to read enough entropy for salt from CSPRNG")
	}

	pass := make([]byte, 0, len(newPassword)+len(salt))
	pass = append(pass, []byte(newPassword)...)
	pass = append(pass, salt...)
	digest := sha3.Sum512(pass)

	stats, err := b.setUserPass.Exec(hex.EncodeToString(digest[:]), hex.EncodeToString(salt), username)
	if err != nil {
		return errors.Wrap(err, "SetUserPassword")
	}
	affected, err := stats.RowsAffected()
	if err != nil {
		return errors.Wrap(err, "SetUserPassword")
	}
	if affected == 0 {
		return ErrUserDoesntExists
	}
	return nil
}

func (b *Backend) ListUsers() ([]string, error) {
	var res []string
	rows, err := b.listUsers.Query()
	if err != nil {
		return res, errors.Wrap(err, "ListUsers")
	}
	for rows.Next() {
		var id uint64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return res, errors.Wrap(err, "ListUsers")
		}
		res = append(res, name)
	}
	if err := rows.Err(); err != nil {
		return res, errors.Wrap(err, "ListUsers")
	}
	return res, nil
}

func (b *Backend) GetExistingUser(username string) (backend.User, error) {
	uid, _, _, err := b.UserCreds(username)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, ErrUserDoesntExists
		}
		return nil, err
	}
	return &User{id: uid, username: username, parent: b}, nil
}

func (b *Backend) GetUser(username string) (backend.User, error) {
	uid, _, _, err := b.UserCreds(username)
	if err != nil {
		if err == sql.ErrNoRows {
			if err := b.CreateUser(username, ""); err != nil {
				return nil, err
			}
			uid, _, _, err = b.UserCreds(username)
			if err != nil {
				return nil, err
			}
		} else {
			return nil, err
		}
	}
	return &User{id: uid, username: username, parent: b}, nil
}

func (b *Backend) checkUser(username, password string) (uint64, error) {
	uid, passHash, passSalt, err := b.UserCreds(username)
	if err != nil {
		return 0, backend.ErrInvalidCredentials
	}

	pass := make([]byte, 0, len(password)+len(passSalt))
	pass = append(pass, []byte(password)...)
	pass = append(pass, passSalt...)
	digest := sha3.Sum512(pass)
	if subtle.ConstantTimeCompare(digest[:], passHash) != 1 {
		return 0, backend.ErrInvalidCredentials
	}

	return uid, nil
}

func (b *Backend) CheckPlain(username, password string) bool {
	_, err := b.checkUser(username, password)
	return err == nil
}

func (b *Backend) Login(username, password string) (backend.User, error) {
	uid, err := b.checkUser(username, password)
	if err != nil {
		return nil, err
	}

	return &User{id: uid, username: username, parent: b}, nil
}

func (b *Backend) CreateMessageLimit() *uint32 {
	return b.opts.MaxMsgBytes
}

func (b *Backend) SetMessageLimit(val *uint32) error {
	b.opts.MaxMsgBytes = val
	return nil
}
