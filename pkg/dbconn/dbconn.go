// Package dbconn contains a series of database-related utility functions.
package dbconn

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/pingcap/tidb/pkg/util/sqlescape"
)

const (
	errLockWaitTimeout  = 1205
	errDeadlock         = 1213
	errCannotConnect    = 2003
	errConnLost         = 2013
	errReadOnly         = 1290
	errQueryKilled      = 1836
	errCapacityExceeded = 3170
	errFoundDuppKey     = 1062 // yes I know there's a typo
)

type DBConfig struct {
	LockWaitTimeout          int
	InnodbLockWaitTimeout    int
	MaxRetries               int
	MaxOpenConnections       int
	Aurora20Compatible       bool // use tx_isolation instead of transaction_isolation
	RangeOptimizerMaxMemSize int64
}

func NewDBConfig() *DBConfig {
	return &DBConfig{
		LockWaitTimeout:          30,
		InnodbLockWaitTimeout:    3,
		MaxRetries:               5,
		MaxOpenConnections:       32, // default is high for historical tests. It's overwritten by the user threads count + 2 for headroom.
		RangeOptimizerMaxMemSize: 0,  // default is 8M, we set to unlimited. Not user configurable (may reconsider in the future).
	}
}

// canRetryError looks at the MySQL error and decides if it is considered
// a permanent failure or not. For simplicity a "retryable" error means
// rollback the transaction and start the transaction again.
// This is because it gets complicated in cases where the statement could
// succeed but then there is a deadlock later on.
func canRetryError(err error) bool {
	var errNumber uint16
	if val, ok := err.(*mysql.MySQLError); ok {
		errNumber = val.Number
	}
	switch errNumber {
	case errLockWaitTimeout, errDeadlock, errCannotConnect,
		errConnLost, errReadOnly, errQueryKilled:
		return true
	default:
		return false
	}
}

// RetryableTransaction retries all statements in a transaction, retrying if a statement
// errors, or there is a deadlock. It will retry up to maxRetries times.
func RetryableTransaction(ctx context.Context, db *sql.DB, ignoreDupKeyWarnings bool, config *DBConfig, stmts ...string) (int64, error) {
	var (
		err          error
		trx          *sql.Tx
		rowsAffected int64
		isFatal      bool
	)
	for i := 0; i < config.MaxRetries; i++ {
		func() {
			// Start a transaction
			if trx, err = db.BeginTx(ctx, nil); err != nil {
				return
			}
			// If anything was non successful as we exit
			// then rollback before either retrying or finishing up
			// If we are going to retry, then backoff first.
			defer func() {
				if err != nil {
					_ = trx.Rollback()
					if i < config.MaxRetries-1 && !isFatal {
						backoff(i)
					}
				}
			}()
			// Execute all statements.
			for _, stmt := range stmts {
				if stmt == "" {
					continue
				}
				var res sql.Result
				if res, err = trx.ExecContext(ctx, stmt); err != nil {
					if !canRetryError(err) {
						isFatal = true
					}
					return
				}
				// Even though there was no ERROR we still need to inspect SHOW WARNINGS
				// This is because many of the statements use INSERT IGNORE.
				var warningRes *sql.Rows
				warningRes, err = trx.QueryContext(ctx, "SHOW WARNINGS") //nolint: execinquery
				if err != nil {
					return
				}
				defer warningRes.Close()
				var level, message string
				var code int
				for warningRes.Next() {
					err = warningRes.Scan(&level, &code, &message)
					if err != nil {
						return
					}
					// We won't receive out of range warnings (1264)
					// because the SQL mode has been unset. This is important
					// because a historical value like 0000-00-00 00:00:00
					// might exist in the table and needs to be copied.
					if code == errFoundDuppKey && ignoreDupKeyWarnings {
						continue // ignore duplicate key warnings
					} else if code == errCapacityExceeded {
						// "Memory capacity of 8388608 bytes for 'range_optimizer_max_mem_size' exceeded.
						// Range optimization was not done for this query."
						// i.e. the query can still execute, but it won't be efficient. Prior to
						// https://github.com/cashapp/spirit/issues/239 we allowed this warning
						// to be ignored. *However* if range optimization is disabled the query is going to
						// tablescan, so it's better to just bail out and present a useful error message.
						isFatal = true
						err = fmt.Errorf("MySQL refused to optimize a statement because the value of 'range_optimizer_max_mem_size' is too low. Please decrease the target-chunk-time, or increase the value of 'range_optimizer_max_mem_size'")
						return
					} else {
						isFatal = true
						err = fmt.Errorf("unsafe warning: %s", message)
						return
					}
				}
				if warningRes.Err() != nil {
					err = warningRes.Err()
					return
				}
				// As long as it is a statement that supports affected rows (err == nil)
				// Get the number of rows affected and add it to the total balance.
				// This uses errC because some statements don't support affected rows,
				// and that's absolutely fine!
				count, errC := res.RowsAffected()
				if errC == nil { // affectedRows is supported
					rowsAffected += count
				}
			} // end for each statement
			// Commit it!
			if err = trx.Commit(); err != nil {
				return
			}
		}()
		if isFatal { // don't retry loop if fatal
			return rowsAffected, err
		}
		// If error is nil, break the loop and return
		// The transaction was successful
		if err == nil {
			return rowsAffected, nil
		}
	} // end of retry loop
	// We've exhausted retries and the error is non-nil
	// return the last error
	return rowsAffected, err
}

// backoff sleeps a few milliseconds before retrying.
func backoff(i int) {
	randFactor := i * rand.Intn(10) * int(time.Millisecond)
	time.Sleep(time.Duration(randFactor))
}

// Exec is like db.Exec but only returns an error.
// This makes it a little bit easier to use in error handling.
// It accepts args which are escaped client side using the TiDB escape library.
// i.e. %n is an identifier, %? is automatic type conversion on a variable.
func Exec(ctx context.Context, db *sql.DB, stmt string, args ...interface{}) error {
	stmt, err := sqlescape.EscapeSQL(stmt, args...)
	if err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, stmt)
	return err
}

// BeginStandardTrx is like db.BeginTx but returns the connection id.
func BeginStandardTrx(ctx context.Context, db *sql.DB, opts *sql.TxOptions) (*sql.Tx, int, error) {
	trx, err := db.BeginTx(ctx, opts)
	if err != nil {
		return nil, 0, err
	}
	// get the connection id.
	var connectionID int
	err = trx.QueryRowContext(ctx, "SELECT CONNECTION_ID()").Scan(&connectionID)
	if err != nil {
		return nil, 0, err
	}
	return trx, connectionID, nil
}
