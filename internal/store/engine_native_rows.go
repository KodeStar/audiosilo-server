//go:build sqlite_native && cgo && sqlite_fts5 && sqlite_preupdate_hook

package store

// The native engine's query path: a cached query binds, steps and reads each
// row in one C call. mattn's own rows read a row column by column, two or three
// calls from Go into C a column; a page of 50 books of 40 columns was 1.8x
// modernc's time for that alone (276 us against 155), and read here it is
// modernc's (157 us). Values come back as modernc's do (int64, float64,
// string, []byte, nil; the code is written against those), and the declared
// types upper-cased, as modernc's.
//
// The statement is mattn's (prepared, cached and closed by the statement cache
// as before); this reads its sqlite3_stmt, which mattn keeps in an unexported
// field. A mattn without that field leaves every query on mattn's own path
// (stmtHandle is nil), and TestNativeFastRows fails, so an upgrade can't drop
// it unnoticed. The C calls resolve to mattn's compiled amalgamation.

/*
#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>

typedef struct sqlite3 sqlite3;
typedef struct sqlite3_stmt sqlite3_stmt;
int sqlite3_step(sqlite3_stmt*);
int sqlite3_reset(sqlite3_stmt*);
int sqlite3_clear_bindings(sqlite3_stmt*);
int sqlite3_column_count(sqlite3_stmt*);
int sqlite3_column_type(sqlite3_stmt*, int);
int64_t sqlite3_column_int64(sqlite3_stmt*, int);
double sqlite3_column_double(sqlite3_stmt*, int);
const unsigned char *sqlite3_column_text(sqlite3_stmt*, int);
const void *sqlite3_column_blob(sqlite3_stmt*, int);
int sqlite3_column_bytes(sqlite3_stmt*, int);
const char *sqlite3_column_name(sqlite3_stmt*, int);
const char *sqlite3_column_decltype(sqlite3_stmt*, int);
int sqlite3_bind_null(sqlite3_stmt*, int);
int sqlite3_bind_int64(sqlite3_stmt*, int, int64_t);
int sqlite3_bind_double(sqlite3_stmt*, int, double);
int sqlite3_bind_text(sqlite3_stmt*, int, const char*, int, void(*)(void*));
int sqlite3_bind_blob(sqlite3_stmt*, int, const void*, int, void(*)(void*));
int sqlite3_bind_parameter_index(sqlite3_stmt*, const char*);
sqlite3 *sqlite3_db_handle(sqlite3_stmt*);
void sqlite3_interrupt(sqlite3*);
const char *sqlite3_errmsg(sqlite3*);
int sqlite3_extended_errcode(sqlite3*);

#define AS_TRANSIENT ((void(*)(void*))-1)

static const char as_empty[1] = {0};

typedef struct { int32_t type; int32_t n; int64_t i; double d; const void *p; } as_col;

// as_read reads every column of s's current row into out.
static void as_read(sqlite3_stmt *s, as_col *out, int ncol) {
	for (int i = 0; i < ncol; i++) {
		int t = sqlite3_column_type(s, i);
		out[i].type = t;
		switch (t) {
		case 1: out[i].i = sqlite3_column_int64(s, i); break;
		case 2: out[i].d = sqlite3_column_double(s, i); break;
		case 3: out[i].p = sqlite3_column_text(s, i); out[i].n = sqlite3_column_bytes(s, i); break;
		case 4: out[i].p = sqlite3_column_blob(s, i); out[i].n = sqlite3_column_bytes(s, i); break;
		default: out[i].p = NULL; out[i].n = 0; break;
		}
	}
}

// as_step_read steps s and, on a row, reads every column into out.
static int as_step_read(sqlite3_stmt *s, as_col *out, int ncol) {
	int rc = sqlite3_step(s);
	if (rc == 100) as_read(s, out, ncol); // SQLITE_ROW
	return rc;
}

// The binds copy (SQLITE_TRANSIENT), so Go memory is never kept by C.
static int as_bind_text(sqlite3_stmt *s, int i, const char *p, int n) {
	return sqlite3_bind_text(s, i, n ? p : as_empty, n, AS_TRANSIENT);
}
static int as_bind_blob(sqlite3_stmt *s, int i, const void *p, int n) {
	return sqlite3_bind_blob(s, i, n ? p : as_empty, n, AS_TRANSIENT);
}
*/
import "C"

import (
	"context"
	"database/sql/driver"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"time"
	"unsafe"

	mattn "github.com/mattn/go-sqlite3"
)

// stmtField is the index of mattn's sqlite3_stmt field in SQLiteStmt, -1 when
// there is none of the expected shape.
var stmtField = func() int {
	f, ok := reflect.TypeFor[mattn.SQLiteStmt]().FieldByName("s")
	if !ok || f.Type.Kind() != reflect.Pointer || len(f.Index) != 1 {
		return -1
	}
	return f.Index[0]
}()

// stmtHandle is s's sqlite3_stmt, nil when it isn't a mattn statement or the
// field isn't there.
func stmtHandle(s driver.Stmt) *C.sqlite3_stmt {
	ms, ok := s.(*mattn.SQLiteStmt)
	if !ok || stmtField < 0 {
		return nil
	}
	return (*C.sqlite3_stmt)(reflect.ValueOf(ms).Elem().Field(stmtField).UnsafePointer())
}

// nativeError is a failure of the fast path, with SQLite's codes (as mattn's
// Error carries them; isUnique reads both).
type nativeError struct {
	code, extended int
	msg            string
}

func (e *nativeError) Error() string { return e.msg }

func lastError(h *C.sqlite3_stmt, rc C.int) error {
	db := C.sqlite3_db_handle(h)
	ext := int(C.sqlite3_extended_errcode(db))
	if ext&0xff != int(rc)&0xff {
		ext = int(rc)
	}
	return &nativeError{code: ext & 0xff, extended: ext, msg: C.GoString(C.sqlite3_errmsg(db))}
}

// nativeQuery runs a cached statement's query on the fast path; ok is false
// when it can't (no handle, an argument type it doesn't bind), and the caller
// runs it on mattn's.
func nativeQuery(ctx context.Context, s driver.Stmt, args []driver.NamedValue) (driver.Rows, bool, error) {
	h := stmtHandle(s)
	if h == nil {
		return nil, false, nil
	}
	for _, a := range args {
		switch a.Value.(type) {
		case nil, int64, float64, bool, string, []byte, time.Time:
		default:
			return nil, false, nil
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, true, err // as modernc's: an interrupt before the first step is lost
	}
	C.sqlite3_reset(h)
	C.sqlite3_clear_bindings(h)
	for _, a := range args {
		if err := bindArg(h, a); err != nil {
			C.sqlite3_clear_bindings(h)
			return nil, true, err
		}
	}
	r := &nativeRows{h: h, ctx: ctx}
	if ctx.Done() != nil {
		db := C.sqlite3_db_handle(h)
		r.stop = context.AfterFunc(ctx, func() {
			r.mu.Lock()
			defer r.mu.Unlock()
			if !r.closed {
				C.sqlite3_interrupt(db)
			}
		})
	}
	// The first step runs here, as modernc's query runs it: SQLite re-prepares a
	// statement whose schema changed (another connection's DDL) at its step, so
	// only then are its columns the ones its rows have. Its error is the query's.
	rc := C.sqlite3_step(h)
	if rc != 100 && rc != 101 { // SQLITE_ROW, SQLITE_DONE
		err := r.stepError(rc)
		_ = r.Close()
		return nil, true, err
	}
	n := int(C.sqlite3_column_count(h))
	r.cols, r.buf = make([]string, n), make([]C.as_col, max(n, 1))
	for i := range n {
		r.cols[i] = C.GoString(C.sqlite3_column_name(h, C.int(i)))
	}
	if rc == 100 {
		C.as_read(h, &r.buf[0], C.int(n))
		r.pending = true
	} else {
		r.done = true
	}
	return r, true, nil
}

// nativeAfterExec resets a cached statement mattn's Exec ran and clears its
// bindings, as modernc's Exec does: mattn leaves both until the statement's
// next run, holding copies of its last arguments (a cover's bytes) meanwhile.
func nativeAfterExec(s driver.Stmt) {
	if h := stmtHandle(s); h != nil {
		C.sqlite3_reset(h)
		C.sqlite3_clear_bindings(h)
	}
}

// bindArg binds one argument as mattn would: by its ordinal, or a named one at
// its :name, @name or $name.
func bindArg(h *C.sqlite3_stmt, a driver.NamedValue) error {
	idx := C.int(a.Ordinal)
	if a.Name != "" {
		idx = 0
		for _, p := range []string{":", "@", "$"} {
			cs := C.CString(p + a.Name)
			idx = C.sqlite3_bind_parameter_index(h, cs)
			C.free(unsafe.Pointer(cs))
			if idx > 0 {
				break
			}
		}
		if idx == 0 {
			return fmt.Errorf("sqlite: no parameter named %q", a.Name)
		}
	}
	var rc C.int
	switch v := a.Value.(type) {
	case nil:
		rc = C.sqlite3_bind_null(h, idx)
	case int64:
		rc = C.sqlite3_bind_int64(h, idx, C.int64_t(v))
	case float64:
		rc = C.sqlite3_bind_double(h, idx, C.double(v))
	case bool:
		var b C.int64_t
		if v {
			b = 1
		}
		rc = C.sqlite3_bind_int64(h, idx, b)
	case string:
		rc = C.as_bind_text(h, idx, (*C.char)(unsafe.Pointer(unsafe.StringData(v))), C.int(len(v)))
	case []byte:
		if v == nil {
			rc = C.sqlite3_bind_null(h, idx)
		} else {
			rc = C.as_bind_blob(h, idx, unsafe.Pointer(unsafe.SliceData(v)), C.int(len(v)))
		}
	case time.Time:
		t := v.Format(mattn.SQLiteTimestampFormats[0])
		rc = C.as_bind_text(h, idx, (*C.char)(unsafe.Pointer(unsafe.StringData(t))), C.int(len(t)))
	}
	if rc != 0 {
		return lastError(h, rc)
	}
	return nil
}

// nativeRows reads a query's rows a row a C call.
type nativeRows struct {
	h    *C.sqlite3_stmt
	ctx  context.Context
	stop func() bool
	// mu and closed keep the context's interrupt off the connection once the
	// rows are closed: a late one would end the connection's next statement,
	// another request's (modernc's interruptOnDone guards the same way), or
	// reach a connection closed meanwhile.
	mu      sync.Mutex
	closed  bool
	cols    []string
	buf     []C.as_col
	pending bool // buf holds the first row, read by the query's step
	done    bool
}

func (r *nativeRows) Columns() []string { return r.cols }

// stepError is a failed step's error: the context's when it interrupted it.
func (r *nativeRows) stepError(rc C.int) error {
	if rc&0xff == 9 && r.ctx.Err() != nil { // SQLITE_INTERRUPT by the context
		return r.ctx.Err()
	}
	return lastError(r.h, rc)
}

func (r *nativeRows) Next(dest []driver.Value) error {
	if r.pending {
		r.pending = false
	} else {
		if r.done {
			return io.EOF
		}
		switch rc := C.as_step_read(r.h, &r.buf[0], C.int(len(r.cols))); rc {
		case 100: // SQLITE_ROW
		case 101: // SQLITE_DONE
			r.done = true
			return io.EOF
		default:
			r.done = true
			return r.stepError(rc)
		}
	}
	for i := range dest {
		c := &r.buf[i]
		switch c._type {
		case 1:
			dest[i] = int64(c.i)
		case 2:
			dest[i] = float64(c.d)
		case 3:
			dest[i] = string(unsafe.Slice((*byte)(c.p), int(c.n)))
		case 4:
			b := make([]byte, int(c.n)) // an empty blob is []byte{}, as mattn reads it
			copy(b, unsafe.Slice((*byte)(c.p), int(c.n)))
			dest[i] = b
		default:
			dest[i] = nil
		}
	}
	return nil
}

func (r *nativeRows) Close() error {
	if r.stop != nil && !r.stop() {
		// The interrupt has started: wait for it, or keep it from coming.
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
	}
	C.sqlite3_reset(r.h)
	C.sqlite3_clear_bindings(r.h)
	return nil
}

// ColumnTypeDatabaseTypeName is the column's declared type, upper-cased.
func (r *nativeRows) ColumnTypeDatabaseTypeName(i int) string {
	return strings.ToUpper(C.GoString(C.sqlite3_column_decltype(r.h, C.int(i))))
}
