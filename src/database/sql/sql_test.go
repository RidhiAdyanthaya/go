// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package sql

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func init() {
	type dbConn struct {
		db *DB
		c  *driverConn
	}
	freedFrom := make(map[dbConn]string)
	var mu sync.Mutex
	getFreedFrom := func(c dbConn) string {
		mu.Lock()
		defer mu.Unlock()
		return freedFrom[c]
	}
	setFreedFrom := func(c dbConn, s string) {
		mu.Lock()
		defer mu.Unlock()
		freedFrom[c] = s
	}
	putConnHook = func(db *DB, c *driverConn) {
		idx := -1
		for i, v := range db.freeConn {
			if v == c {
				idx = i
				break
			}
		}
		if idx >= 0 {
			// print before panic, as panic may get lost due to conflicting panic
			// (all goroutines asleep) elsewhere, since we might not unlock
			// the mutex in freeConn here.
			println("double free of conn. conflicts are:\nA) " + getFreedFrom(dbConn{db, c}) + "\n\nand\nB) " + stack())
			panic("double free of conn.")
		}
		setFreedFrom(dbConn{db, c}, stack())
	}
}

const fakeDBName = "foo"

var chrisBirthday = time.Unix(123456789, 0)

func newTestDB(t testing.TB, name string) *DB {
	db, err := Open("test", fakeDBName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec("WIPE"); err != nil {
		t.Fatalf("exec wipe: %v", err)
	}
	if name == "people" {
		exec(t, db, "CREATE|people|name=string,age=int32,photo=blob,dead=bool,bdate=datetime")
		exec(t, db, "INSERT|people|name=Alice,age=?,photo=APHOTO", 1)
		exec(t, db, "INSERT|people|name=Bob,age=?,photo=BPHOTO", 2)
		exec(t, db, "INSERT|people|name=Chris,age=?,photo=CPHOTO,bdate=?", 3, chrisBirthday)
	}
	if name == "magicquery" {
		// Magic table name and column, known by fakedb_test.go.
		exec(t, db, "CREATE|magicquery|op=string,millis=int32")
		exec(t, db, "INSERT|magicquery|op=sleep,millis=10")
	}
	return db
}

func TestDriverPanic(t *testing.T) {
	// Test that if driver panics, database/sql does not deadlock.
	db, err := Open("test", fakeDBName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	expectPanic := func(name string, f func()) {
		defer func() {
			err := recover()
			if err == nil {
				t.Fatalf("%s did not panic", name)
			}
		}()
		f()
	}

	expectPanic("Exec Exec", func() { db.Exec("PANIC|Exec|WIPE") })
	exec(t, db, "WIPE") // check not deadlocked
	expectPanic("Exec NumInput", func() { db.Exec("PANIC|NumInput|WIPE") })
	exec(t, db, "WIPE") // check not deadlocked
	expectPanic("Exec Close", func() { db.Exec("PANIC|Close|WIPE") })
	exec(t, db, "WIPE")             // check not deadlocked
	exec(t, db, "PANIC|Query|WIPE") // should run successfully: Exec does not call Query
	exec(t, db, "WIPE")             // check not deadlocked

	exec(t, db, "CREATE|people|name=string,age=int32,photo=blob,dead=bool,bdate=datetime")

	expectPanic("Query Query", func() { db.Query("PANIC|Query|SELECT|people|age,name|") })
	expectPanic("Query NumInput", func() { db.Query("PANIC|NumInput|SELECT|people|age,name|") })
	expectPanic("Query Close", func() {
		rows, err := db.Query("PANIC|Close|SELECT|people|age,name|")
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	})
	db.Query("PANIC|Exec|SELECT|people|age,name|") // should run successfully: Query does not call Exec
	exec(t, db, "WIPE")                            // check not deadlocked
}

func exec(t testing.TB, db *DB, query string, args ...interface{}) {
	_, err := db.Exec(query, args...)
	if err != nil {
		t.Fatalf("Exec of %q: %v", query, err)
	}
}

func closeDB(t testing.TB, db *DB) {
	if e := recover(); e != nil {
		fmt.Printf("Panic: %v\n", e)
		panic(e)
	}
	defer setHookpostCloseConn(nil)
	setHookpostCloseConn(func(_ *fakeConn, err error) {
		if err != nil {
			t.Errorf("Error closing fakeConn: %v", err)
		}
	})
	for i, dc := range db.freeConn {
		if n := len(dc.openStmt); n > 0 {
			// Just a sanity check. This is legal in
			// general, but if we make the tests clean up
			// their statements first, then we can safely
			// verify this is always zero here, and any
			// other value is a leak.
			t.Errorf("while closing db, freeConn %d/%d had %d open stmts; want 0", i, len(db.freeConn), n)
		}
	}
	err := db.Close()
	if err != nil {
		t.Fatalf("error closing DB: %v", err)
	}
	if count := db.numOpenConns(); count != 0 {
		t.Fatalf("%d connections still open after closing DB", count)
	}
}

// numPrepares assumes that db has exactly 1 idle conn and returns
// its count of calls to Prepare
func numPrepares(t *testing.T, db *DB) int {
	if n := len(db.freeConn); n != 1 {
		t.Fatalf("free conns = %d; want 1", n)
	}
	return db.freeConn[0].ci.(*fakeConn).numPrepare
}

func (db *DB) numDeps() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.dep)
}

// Dependencies are closed via a goroutine, so this polls waiting for
// numDeps to fall to want, waiting up to d.
func (db *DB) numDepsPollUntil(want int, d time.Duration) int {
	deadline := time.Now().Add(d)
	for {
		n := db.numDeps()
		if n <= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (db *DB) numFreeConns() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return len(db.freeConn)
}

func (db *DB) numOpenConns() int {
	db.mu.Lock()
	defer db.mu.Unlock()
	return db.numOpen
}

// clearAllConns closes all connections in db.
func (db *DB) clearAllConns(t *testing.T) {
	db.SetMaxIdleConns(0)

	if g, w := db.numFreeConns(), 0; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	if n := db.numDepsPollUntil(0, time.Second); n > 0 {
		t.Errorf("number of dependencies = %d; expected 0", n)
		db.dumpDeps(t)
	}
}

func (db *DB) dumpDeps(t *testing.T) {
	for fc := range db.dep {
		db.dumpDep(t, 0, fc, map[finalCloser]bool{})
	}
}

func (db *DB) dumpDep(t *testing.T, depth int, dep finalCloser, seen map[finalCloser]bool) {
	seen[dep] = true
	indent := strings.Repeat("  ", depth)
	ds := db.dep[dep]
	for k := range ds {
		t.Logf("%s%T (%p) waiting for -> %T (%p)", indent, dep, dep, k, k)
		if fc, ok := k.(finalCloser); ok {
			if !seen[fc] {
				db.dumpDep(t, depth+1, fc, seen)
			}
		}
	}
}

func TestQuery(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	prepares0 := numPrepares(t, db)
	rows, err := db.Query("SELECT|people|age,name|")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	type row struct {
		age  int
		name string
	}
	got := []row{}
	for rows.Next() {
		var r row
		err = rows.Scan(&r.age, &r.name)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []row{
		{age: 1, name: "Alice"},
		{age: 2, name: "Bob"},
		{age: 3, name: "Chris"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatch.\n got: %#v\nwant: %#v", got, want)
	}

	// And verify that the final rows.Next() call, which hit EOF,
	// also closed the rows connection.
	if n := db.numFreeConns(); n != 1 {
		t.Fatalf("free conns after query hitting EOF = %d; want 1", n)
	}
	if prepares := numPrepares(t, db) - prepares0; prepares != 1 {
		t.Errorf("executed %d Prepare statements; want 1", prepares)
	}
}

func TestQueryContext(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	prepares0 := numPrepares(t, db)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	rows, err := db.QueryContext(ctx, "SELECT|people|age,name|")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	type row struct {
		age  int
		name string
	}
	got := []row{}
	index := 0
	for rows.Next() {
		if index == 2 {
			cancel()
			time.Sleep(10 * time.Millisecond)
		}
		var r row
		err = rows.Scan(&r.age, &r.name)
		if err != nil {
			if index == 2 {
				break
			}
			t.Fatalf("Scan: %v", err)
		}
		if index == 2 && err == nil {
			t.Fatal("expected an error on last scan")
		}
		got = append(got, r)
		index++
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []row{
		{age: 1, name: "Alice"},
		{age: 2, name: "Bob"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatch.\n got: %#v\nwant: %#v", got, want)
	}

	// And verify that the final rows.Next() call, which hit EOF,
	// also closed the rows connection.
	waitForFree(t, db, 5*time.Second, 1)
	if prepares := numPrepares(t, db) - prepares0; prepares != 1 {
		t.Errorf("executed %d Prepare statements; want 1", prepares)
	}
}

func waitCondition(waitFor, checkEvery time.Duration, fn func() bool) bool {
	deadline := time.Now().Add(waitFor)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(checkEvery)
	}
	return false
}

// waitForFree checks db.numFreeConns until either it equals want or
// the maxWait time elapses.
func waitForFree(t *testing.T, db *DB, maxWait time.Duration, want int) {
	var numFree int
	if !waitCondition(maxWait, 5*time.Millisecond, func() bool {
		numFree = db.numFreeConns()
		return numFree == want
	}) {
		t.Fatalf("free conns after hitting EOF = %d; want %d", numFree, want)
	}
}

func TestQueryContextWait(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	prepares0 := numPrepares(t, db)

	ctx, _ := context.WithTimeout(context.Background(), time.Millisecond*15)

	// This will trigger the *fakeConn.Prepare method which will take time
	// performing the query. The ctxDriverPrepare func will check the context
	// after this and close the rows and return an error.
	_, err := db.QueryContext(ctx, "WAIT|1s|SELECT|people|age,name|")
	if err != context.DeadlineExceeded {
		t.Fatalf("expected QueryContext to error with context deadline exceeded but returned %v", err)
	}

	// Verify closed rows connection after error condition.
	waitForFree(t, db, 5*time.Second, 1)
	if prepares := numPrepares(t, db) - prepares0; prepares != 1 {
		t.Errorf("executed %d Prepare statements; want 1", prepares)
	}
}

func TestTxContextWait(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	ctx, _ := context.WithTimeout(context.Background(), time.Millisecond*15)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}

	// This will trigger the *fakeConn.Prepare method which will take time
	// performing the query. The ctxDriverPrepare func will check the context
	// after this and close the rows and return an error.
	_, err = tx.QueryContext(ctx, "WAIT|1s|SELECT|people|age,name|")
	if err != context.DeadlineExceeded {
		t.Fatalf("expected QueryContext to error with context deadline exceeded but returned %v", err)
	}

	waitForFree(t, db, 5*time.Second, 0)

	// Ensure the dropped connection allows more connections to be made.
	// Checked on DB Close.
	waitCondition(5*time.Second, 5*time.Millisecond, func() bool {
		return db.numOpenConns() == 0
	})
}

func TestMultiResultSetQuery(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	prepares0 := numPrepares(t, db)
	rows, err := db.Query("SELECT|people|age,name|;SELECT|people|name|")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	type row1 struct {
		age  int
		name string
	}
	type row2 struct {
		name string
	}
	got1 := []row1{}
	for rows.Next() {
		var r row1
		err = rows.Scan(&r.age, &r.name)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got1 = append(got1, r)
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want1 := []row1{
		{age: 1, name: "Alice"},
		{age: 2, name: "Bob"},
		{age: 3, name: "Chris"},
	}
	if !reflect.DeepEqual(got1, want1) {
		t.Errorf("mismatch.\n got1: %#v\nwant: %#v", got1, want1)
	}

	if !rows.NextResultSet() {
		t.Errorf("expected another result set")
	}

	got2 := []row2{}
	for rows.Next() {
		var r row2
		err = rows.Scan(&r.name)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got2 = append(got2, r)
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want2 := []row2{
		{name: "Alice"},
		{name: "Bob"},
		{name: "Chris"},
	}
	if !reflect.DeepEqual(got2, want2) {
		t.Errorf("mismatch.\n got: %#v\nwant: %#v", got2, want2)
	}
	if rows.NextResultSet() {
		t.Errorf("expected no more result sets")
	}

	// And verify that the final rows.Next() call, which hit EOF,
	// also closed the rows connection.
	waitForFree(t, db, 5*time.Second, 1)
	if prepares := numPrepares(t, db) - prepares0; prepares != 1 {
		t.Errorf("executed %d Prepare statements; want 1", prepares)
	}
}

func TestQueryNamedArg(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	prepares0 := numPrepares(t, db)
	rows, err := db.Query(
		// Ensure the name and age parameters only match on placeholder name, not position.
		"SELECT|people|age,name|name=?name,age=?age",
		Named("age", 2),
		Named("name", "Bob"),
	)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	type row struct {
		age  int
		name string
	}
	got := []row{}
	for rows.Next() {
		var r row
		err = rows.Scan(&r.age, &r.name)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	err = rows.Err()
	if err != nil {
		t.Fatalf("Err: %v", err)
	}
	want := []row{
		{age: 2, name: "Bob"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatch.\n got: %#v\nwant: %#v", got, want)
	}

	// And verify that the final rows.Next() call, which hit EOF,
	// also closed the rows connection.
	if n := db.numFreeConns(); n != 1 {
		t.Fatalf("free conns after query hitting EOF = %d; want 1", n)
	}
	if prepares := numPrepares(t, db) - prepares0; prepares != 1 {
		t.Errorf("executed %d Prepare statements; want 1", prepares)
	}
}

func TestByteOwnership(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	rows, err := db.Query("SELECT|people|name,photo|")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	type row struct {
		name  []byte
		photo RawBytes
	}
	got := []row{}
	for rows.Next() {
		var r row
		err = rows.Scan(&r.name, &r.photo)
		if err != nil {
			t.Fatalf("Scan: %v", err)
		}
		got = append(got, r)
	}
	corruptMemory := []byte("\xffPHOTO")
	want := []row{
		{name: []byte("Alice"), photo: corruptMemory},
		{name: []byte("Bob"), photo: corruptMemory},
		{name: []byte("Chris"), photo: corruptMemory},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("mismatch.\n got: %#v\nwant: %#v", got, want)
	}

	var photo RawBytes
	err = db.QueryRow("SELECT|people|photo|name=?", "Alice").Scan(&photo)
	if err == nil {
		t.Error("want error scanning into RawBytes from QueryRow")
	}
}

func TestRowsColumns(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	rows, err := db.Query("SELECT|people|age,name|")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("Columns: %v", err)
	}
	want := []string{"age", "name"}
	if !reflect.DeepEqual(cols, want) {
		t.Errorf("got %#v; want %#v", cols, want)
	}
	if err := rows.Close(); err != nil {
		t.Errorf("error closing rows: %s", err)
	}
}

func TestRowsColumnTypes(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	rows, err := db.Query("SELECT|people|age,name|")
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	tt, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("ColumnTypes: %v", err)
	}

	types := make([]reflect.Type, len(tt))
	for i, tp := range tt {
		st := tp.ScanType()
		if st == nil {
			t.Errorf("scantype is null for column %q", tp.Name())
			continue
		}
		types[i] = st
	}
	values := make([]interface{}, len(tt))
	for i := range values {
		values[i] = reflect.New(types[i]).Interface()
	}
	ct := 0
	for rows.Next() {
		err = rows.Scan(values...)
		if err != nil {
			t.Fatalf("failed to scan values in %v", err)
		}
		ct++
		if ct == 0 {
			if values[0].(string) != "Bob" {
				t.Errorf("Expected Bob, got %v", values[0])
			}
			if values[1].(int) != 2 {
				t.Errorf("Expected 2, got %v", values[1])
			}
		}
	}
	if ct != 3 {
		t.Errorf("expected 3 rows, got %d", ct)
	}

	if err := rows.Close(); err != nil {
		t.Errorf("error closing rows: %s", err)
	}
}

func TestQueryRow(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	var name string
	var age int
	var birthday time.Time

	err := db.QueryRow("SELECT|people|age,name|age=?", 3).Scan(&age)
	if err == nil || !strings.Contains(err.Error(), "expected 2 destination arguments") {
		t.Errorf("expected error from wrong number of arguments; actually got: %v", err)
	}

	err = db.QueryRow("SELECT|people|bdate|age=?", 3).Scan(&birthday)
	if err != nil || !birthday.Equal(chrisBirthday) {
		t.Errorf("chris birthday = %v, err = %v; want %v", birthday, err, chrisBirthday)
	}

	err = db.QueryRow("SELECT|people|age,name|age=?", 2).Scan(&age, &name)
	if err != nil {
		t.Fatalf("age QueryRow+Scan: %v", err)
	}
	if name != "Bob" {
		t.Errorf("expected name Bob, got %q", name)
	}
	if age != 2 {
		t.Errorf("expected age 2, got %d", age)
	}

	err = db.QueryRow("SELECT|people|age,name|name=?", "Alice").Scan(&age, &name)
	if err != nil {
		t.Fatalf("name QueryRow+Scan: %v", err)
	}
	if name != "Alice" {
		t.Errorf("expected name Alice, got %q", name)
	}
	if age != 1 {
		t.Errorf("expected age 1, got %d", age)
	}

	var photo []byte
	err = db.QueryRow("SELECT|people|photo|name=?", "Alice").Scan(&photo)
	if err != nil {
		t.Fatalf("photo QueryRow+Scan: %v", err)
	}
	want := []byte("APHOTO")
	if !reflect.DeepEqual(photo, want) {
		t.Errorf("photo = %q; want %q", photo, want)
	}
}

func TestTxRollbackCommitErr(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = tx.Rollback()
	if err != nil {
		t.Errorf("expected nil error from Rollback; got %v", err)
	}
	err = tx.Commit()
	if err != ErrTxDone {
		t.Errorf("expected %q from Commit; got %q", ErrTxDone, err)
	}

	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	err = tx.Commit()
	if err != nil {
		t.Errorf("expected nil error from Commit; got %v", err)
	}
	err = tx.Rollback()
	if err != ErrTxDone {
		t.Errorf("expected %q from Rollback; got %q", ErrTxDone, err)
	}
}

func TestStatementErrorAfterClose(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	stmt, err := db.Prepare("SELECT|people|age|name=?")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	err = stmt.Close()
	if err != nil {
		t.Fatalf("Close: %v", err)
	}
	var name string
	err = stmt.QueryRow("foo").Scan(&name)
	if err == nil {
		t.Errorf("expected error from QueryRow.Scan after Stmt.Close")
	}
}

func TestStatementQueryRow(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	stmt, err := db.Prepare("SELECT|people|age|name=?")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()
	var age int
	for n, tt := range []struct {
		name string
		want int
	}{
		{"Alice", 1},
		{"Bob", 2},
		{"Chris", 3},
	} {
		if err := stmt.QueryRow(tt.name).Scan(&age); err != nil {
			t.Errorf("%d: on %q, QueryRow/Scan: %v", n, tt.name, err)
		} else if age != tt.want {
			t.Errorf("%d: age=%d, want %d", n, age, tt.want)
		}
	}
}

type stubDriverStmt struct {
	err error
}

func (s stubDriverStmt) Close() error {
	return s.err
}

func (s stubDriverStmt) NumInput() int {
	return -1
}

func (s stubDriverStmt) Exec(args []driver.Value) (driver.Result, error) {
	return nil, nil
}

func (s stubDriverStmt) Query(args []driver.Value) (driver.Rows, error) {
	return nil, nil
}

// golang.org/issue/12798
func TestStatementClose(t *testing.T) {
	want := errors.New("STMT ERROR")

	tests := []struct {
		stmt *Stmt
		msg  string
	}{
		{&Stmt{stickyErr: want}, "stickyErr not propagated"},
		{&Stmt{tx: &Tx{}, txds: &driverStmt{Locker: &sync.Mutex{}, si: stubDriverStmt{want}}}, "driverStmt.Close() error not propagated"},
	}
	for _, test := range tests {
		if err := test.stmt.Close(); err != want {
			t.Errorf("%s. Got stmt.Close() = %v, want = %v", test.msg, err, want)
		}
	}
}

// golang.org/issue/3734
func TestStatementQueryRowConcurrent(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	stmt, err := db.Prepare("SELECT|people|age|name=?")
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	defer stmt.Close()

	const n = 10
	ch := make(chan error, n)
	for i := 0; i < n; i++ {
		go func() {
			var age int
			err := stmt.QueryRow("Alice").Scan(&age)
			if err == nil && age != 1 {
				err = fmt.Errorf("unexpected age %d", age)
			}
			ch <- err
		}()
	}
	for i := 0; i < n; i++ {
		if err := <-ch; err != nil {
			t.Error(err)
		}
	}
}

// just a test of fakedb itself
func TestBogusPreboundParameters(t *testing.T) {
	db := newTestDB(t, "foo")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")
	_, err := db.Prepare("INSERT|t1|name=?,age=bogusconversion")
	if err == nil {
		t.Fatalf("expected error")
	}
	if err.Error() != `fakedb: invalid conversion to int32 from "bogusconversion"` {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestExec(t *testing.T) {
	db := newTestDB(t, "foo")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")
	stmt, err := db.Prepare("INSERT|t1|name=?,age=?")
	if err != nil {
		t.Errorf("Stmt, err = %v, %v", stmt, err)
	}
	defer stmt.Close()

	type execTest struct {
		args    []interface{}
		wantErr string
	}
	execTests := []execTest{
		// Okay:
		{[]interface{}{"Brad", 31}, ""},
		{[]interface{}{"Brad", int64(31)}, ""},
		{[]interface{}{"Bob", "32"}, ""},
		{[]interface{}{7, 9}, ""},

		// Invalid conversions:
		{[]interface{}{"Brad", int64(0xFFFFFFFF)}, "sql: converting argument $2 type: sql/driver: value 4294967295 overflows int32"},
		{[]interface{}{"Brad", "strconv fail"}, `sql: converting argument $2 type: sql/driver: value "strconv fail" can't be converted to int32`},

		// Wrong number of args:
		{[]interface{}{}, "sql: expected 2 arguments, got 0"},
		{[]interface{}{1, 2, 3}, "sql: expected 2 arguments, got 3"},
	}
	for n, et := range execTests {
		_, err := stmt.Exec(et.args...)
		errStr := ""
		if err != nil {
			errStr = err.Error()
		}
		if errStr != et.wantErr {
			t.Errorf("stmt.Execute #%d: for %v, got error %q, want error %q",
				n, et.args, errStr, et.wantErr)
		}
	}
}

func TestTxPrepare(t *testing.T) {
	db := newTestDB(t, "")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin = %v", err)
	}
	stmt, err := tx.Prepare("INSERT|t1|name=?,age=?")
	if err != nil {
		t.Fatalf("Stmt, err = %v, %v", stmt, err)
	}
	defer stmt.Close()
	_, err = stmt.Exec("Bobby", 7)
	if err != nil {
		t.Fatalf("Exec = %v", err)
	}
	err = tx.Commit()
	if err != nil {
		t.Fatalf("Commit = %v", err)
	}
	// Commit() should have closed the statement
	if !stmt.closed {
		t.Fatal("Stmt not closed after Commit")
	}
}

func TestTxStmt(t *testing.T) {
	db := newTestDB(t, "")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")
	stmt, err := db.Prepare("INSERT|t1|name=?,age=?")
	if err != nil {
		t.Fatalf("Stmt, err = %v, %v", stmt, err)
	}
	defer stmt.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin = %v", err)
	}
	txs := tx.Stmt(stmt)
	defer txs.Close()
	_, err = txs.Exec("Bobby", 7)
	if err != nil {
		t.Fatalf("Exec = %v", err)
	}
	err = tx.Commit()
	if err != nil {
		t.Fatalf("Commit = %v", err)
	}
	// Commit() should have closed the statement
	if !txs.closed {
		t.Fatal("Stmt not closed after Commit")
	}
}

// Issue: https://golang.org/issue/2784
// This test didn't fail before because we got lucky with the fakedb driver.
// It was failing, and now not, in github.com/bradfitz/go-sql-test
func TestTxQuery(t *testing.T) {
	db := newTestDB(t, "")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")
	exec(t, db, "INSERT|t1|name=Alice")

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	r, err := tx.Query("SELECT|t1|name|")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()

	if !r.Next() {
		if r.Err() != nil {
			t.Fatal(r.Err())
		}
		t.Fatal("expected one row")
	}

	var x string
	err = r.Scan(&x)
	if err != nil {
		t.Fatal(err)
	}
}

func TestTxQueryInvalid(t *testing.T) {
	db := newTestDB(t, "")
	defer closeDB(t, db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	_, err = tx.Query("SELECT|t1|name|")
	if err == nil {
		t.Fatal("Error expected")
	}
}

// Tests fix for issue 4433, that retries in Begin happen when
// conn.Begin() returns ErrBadConn
func TestTxErrBadConn(t *testing.T) {
	db, err := Open("test", fakeDBName+";badConn")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := db.Exec("WIPE"); err != nil {
		t.Fatalf("exec wipe: %v", err)
	}
	defer closeDB(t, db)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")
	stmt, err := db.Prepare("INSERT|t1|name=?,age=?")
	if err != nil {
		t.Fatalf("Stmt, err = %v, %v", stmt, err)
	}
	defer stmt.Close()
	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("Begin = %v", err)
	}
	txs := tx.Stmt(stmt)
	defer txs.Close()
	_, err = txs.Exec("Bobby", 7)
	if err != nil {
		t.Fatalf("Exec = %v", err)
	}
	err = tx.Commit()
	if err != nil {
		t.Fatalf("Commit = %v", err)
	}
}

// Tests fix for issue 2542, that we release a lock when querying on
// a closed connection.
func TestIssue2542Deadlock(t *testing.T) {
	db := newTestDB(t, "people")
	closeDB(t, db)
	for i := 0; i < 2; i++ {
		_, err := db.Query("SELECT|people|age,name|")
		if err == nil {
			t.Fatalf("expected error")
		}
	}
}

// From golang.org/issue/3865
func TestCloseStmtBeforeRows(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	s, err := db.Prepare("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}

	r, err := s.Query()
	if err != nil {
		s.Close()
		t.Fatal(err)
	}

	err = s.Close()
	if err != nil {
		t.Fatal(err)
	}

	r.Close()
}

// Tests fix for issue 2788, that we bind nil to a []byte if the
// value in the column is sql null
func TestNullByteSlice(t *testing.T) {
	db := newTestDB(t, "")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t|id=int32,name=nullstring")
	exec(t, db, "INSERT|t|id=10,name=?", nil)

	var name []byte

	err := db.QueryRow("SELECT|t|name|id=?", 10).Scan(&name)
	if err != nil {
		t.Fatal(err)
	}
	if name != nil {
		t.Fatalf("name []byte should be nil for null column value, got: %#v", name)
	}

	exec(t, db, "INSERT|t|id=11,name=?", "bob")
	err = db.QueryRow("SELECT|t|name|id=?", 11).Scan(&name)
	if err != nil {
		t.Fatal(err)
	}
	if string(name) != "bob" {
		t.Fatalf("name []byte should be bob, got: %q", string(name))
	}
}

func TestPointerParamsAndScans(t *testing.T) {
	db := newTestDB(t, "")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t|id=int32,name=nullstring")

	bob := "bob"
	var name *string

	name = &bob
	exec(t, db, "INSERT|t|id=10,name=?", name)
	name = nil
	exec(t, db, "INSERT|t|id=20,name=?", name)

	err := db.QueryRow("SELECT|t|name|id=?", 10).Scan(&name)
	if err != nil {
		t.Fatalf("querying id 10: %v", err)
	}
	if name == nil {
		t.Errorf("id 10's name = nil; want bob")
	} else if *name != "bob" {
		t.Errorf("id 10's name = %q; want bob", *name)
	}

	err = db.QueryRow("SELECT|t|name|id=?", 20).Scan(&name)
	if err != nil {
		t.Fatalf("querying id 20: %v", err)
	}
	if name != nil {
		t.Errorf("id 20 = %q; want nil", *name)
	}
}

func TestQueryRowClosingStmt(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	var name string
	var age int
	err := db.QueryRow("SELECT|people|age,name|age=?", 3).Scan(&age, &name)
	if err != nil {
		t.Fatal(err)
	}
	if len(db.freeConn) != 1 {
		t.Fatalf("expected 1 free conn")
	}
	fakeConn := db.freeConn[0].ci.(*fakeConn)
	if made, closed := fakeConn.stmtsMade, fakeConn.stmtsClosed; made != closed {
		t.Errorf("statement close mismatch: made %d, closed %d", made, closed)
	}
}

// Test issue 6651
func TestIssue6651(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	var v string

	want := "error in rows.Next"
	rowsCursorNextHook = func(dest []driver.Value) error {
		return fmt.Errorf(want)
	}
	defer func() { rowsCursorNextHook = nil }()
	err := db.QueryRow("SELECT|people|name|").Scan(&v)
	if err == nil || err.Error() != want {
		t.Errorf("error = %q; want %q", err, want)
	}
	rowsCursorNextHook = nil

	want = "error in rows.Close"
	rowsCloseHook = func(rows *Rows, err *error) {
		*err = fmt.Errorf(want)
	}
	defer func() { rowsCloseHook = nil }()
	err = db.QueryRow("SELECT|people|name|").Scan(&v)
	if err == nil || err.Error() != want {
		t.Errorf("error = %q; want %q", err, want)
	}
}

type nullTestRow struct {
	nullParam    interface{}
	notNullParam interface{}
	scanNullVal  interface{}
}

type nullTestSpec struct {
	nullType    string
	notNullType string
	rows        [6]nullTestRow
}

func TestNullStringParam(t *testing.T) {
	spec := nullTestSpec{"nullstring", "string", [6]nullTestRow{
		{NullString{"aqua", true}, "", NullString{"aqua", true}},
		{NullString{"brown", false}, "", NullString{"", false}},
		{"chartreuse", "", NullString{"chartreuse", true}},
		{NullString{"darkred", true}, "", NullString{"darkred", true}},
		{NullString{"eel", false}, "", NullString{"", false}},
		{"foo", NullString{"black", false}, nil},
	}}
	nullTestRun(t, spec)
}

func TestNullInt64Param(t *testing.T) {
	spec := nullTestSpec{"nullint64", "int64", [6]nullTestRow{
		{NullInt64{31, true}, 1, NullInt64{31, true}},
		{NullInt64{-22, false}, 1, NullInt64{0, false}},
		{22, 1, NullInt64{22, true}},
		{NullInt64{33, true}, 1, NullInt64{33, true}},
		{NullInt64{222, false}, 1, NullInt64{0, false}},
		{0, NullInt64{31, false}, nil},
	}}
	nullTestRun(t, spec)
}

func TestNullFloat64Param(t *testing.T) {
	spec := nullTestSpec{"nullfloat64", "float64", [6]nullTestRow{
		{NullFloat64{31.2, true}, 1, NullFloat64{31.2, true}},
		{NullFloat64{13.1, false}, 1, NullFloat64{0, false}},
		{-22.9, 1, NullFloat64{-22.9, true}},
		{NullFloat64{33.81, true}, 1, NullFloat64{33.81, true}},
		{NullFloat64{222, false}, 1, NullFloat64{0, false}},
		{10, NullFloat64{31.2, false}, nil},
	}}
	nullTestRun(t, spec)
}

func TestNullBoolParam(t *testing.T) {
	spec := nullTestSpec{"nullbool", "bool", [6]nullTestRow{
		{NullBool{false, true}, true, NullBool{false, true}},
		{NullBool{true, false}, false, NullBool{false, false}},
		{true, true, NullBool{true, true}},
		{NullBool{true, true}, false, NullBool{true, true}},
		{NullBool{true, false}, true, NullBool{false, false}},
		{true, NullBool{true, false}, nil},
	}}
	nullTestRun(t, spec)
}

func nullTestRun(t *testing.T, spec nullTestSpec) {
	db := newTestDB(t, "")
	defer closeDB(t, db)
	exec(t, db, fmt.Sprintf("CREATE|t|id=int32,name=string,nullf=%s,notnullf=%s", spec.nullType, spec.notNullType))

	// Inserts with db.Exec:
	exec(t, db, "INSERT|t|id=?,name=?,nullf=?,notnullf=?", 1, "alice", spec.rows[0].nullParam, spec.rows[0].notNullParam)
	exec(t, db, "INSERT|t|id=?,name=?,nullf=?,notnullf=?", 2, "bob", spec.rows[1].nullParam, spec.rows[1].notNullParam)

	// Inserts with a prepared statement:
	stmt, err := db.Prepare("INSERT|t|id=?,name=?,nullf=?,notnullf=?")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	if _, err := stmt.Exec(3, "chris", spec.rows[2].nullParam, spec.rows[2].notNullParam); err != nil {
		t.Errorf("exec insert chris: %v", err)
	}
	if _, err := stmt.Exec(4, "dave", spec.rows[3].nullParam, spec.rows[3].notNullParam); err != nil {
		t.Errorf("exec insert dave: %v", err)
	}
	if _, err := stmt.Exec(5, "eleanor", spec.rows[4].nullParam, spec.rows[4].notNullParam); err != nil {
		t.Errorf("exec insert eleanor: %v", err)
	}

	// Can't put null val into non-null col
	if _, err := stmt.Exec(6, "bob", spec.rows[5].nullParam, spec.rows[5].notNullParam); err == nil {
		t.Errorf("expected error inserting nil val with prepared statement Exec")
	}

	_, err = db.Exec("INSERT|t|id=?,name=?,nullf=?", 999, nil, nil)
	if err == nil {
		// TODO: this test fails, but it's just because
		// fakeConn implements the optional Execer interface,
		// so arguably this is the correct behavior. But
		// maybe I should flesh out the fakeConn.Exec
		// implementation so this properly fails.
		// t.Errorf("expected error inserting nil name with Exec")
	}

	paramtype := reflect.TypeOf(spec.rows[0].nullParam)
	bindVal := reflect.New(paramtype).Interface()

	for i := 0; i < 5; i++ {
		id := i + 1
		if err := db.QueryRow("SELECT|t|nullf|id=?", id).Scan(bindVal); err != nil {
			t.Errorf("id=%d Scan: %v", id, err)
		}
		bindValDeref := reflect.ValueOf(bindVal).Elem().Interface()
		if !reflect.DeepEqual(bindValDeref, spec.rows[i].scanNullVal) {
			t.Errorf("id=%d got %#v, want %#v", id, bindValDeref, spec.rows[i].scanNullVal)
		}
	}
}

// golang.org/issue/4859
func TestQueryRowNilScanDest(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	var name *string // nil pointer
	err := db.QueryRow("SELECT|people|name|").Scan(name)
	want := "sql: Scan error on column index 0: destination pointer is nil"
	if err == nil || err.Error() != want {
		t.Errorf("error = %q; want %q", err.Error(), want)
	}
}

func TestIssue4902(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	driver := db.driver.(*fakeDriver)
	opens0 := driver.openCount

	var stmt *Stmt
	var err error
	for i := 0; i < 10; i++ {
		stmt, err = db.Prepare("SELECT|people|name|")
		if err != nil {
			t.Fatal(err)
		}
		err = stmt.Close()
		if err != nil {
			t.Fatal(err)
		}
	}

	opens := driver.openCount - opens0
	if opens > 1 {
		t.Errorf("opens = %d; want <= 1", opens)
		t.Logf("db = %#v", db)
		t.Logf("driver = %#v", driver)
		t.Logf("stmt = %#v", stmt)
	}
}

// Issue 3857
// This used to deadlock.
func TestSimultaneousQueries(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	r1, err := tx.Query("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Close()

	r2, err := tx.Query("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
}

func TestMaxIdleConns(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if got := len(db.freeConn); got != 1 {
		t.Errorf("freeConns = %d; want 1", got)
	}

	db.SetMaxIdleConns(0)

	if got := len(db.freeConn); got != 0 {
		t.Errorf("freeConns after set to zero = %d; want 0", got)
	}

	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	if got := len(db.freeConn); got != 0 {
		t.Errorf("freeConns = %d; want 0", got)
	}
}

func TestMaxOpenConns(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	defer setHookpostCloseConn(nil)
	setHookpostCloseConn(func(_ *fakeConn, err error) {
		if err != nil {
			t.Errorf("Error closing fakeConn: %v", err)
		}
	})

	db := newTestDB(t, "magicquery")
	defer closeDB(t, db)

	driver := db.driver.(*fakeDriver)

	// Force the number of open connections to 0 so we can get an accurate
	// count for the test
	db.clearAllConns(t)

	driver.mu.Lock()
	opens0 := driver.openCount
	closes0 := driver.closeCount
	driver.mu.Unlock()

	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(10)

	stmt, err := db.Prepare("SELECT|magicquery|op|op=?,millis=?")
	if err != nil {
		t.Fatal(err)
	}

	// Start 50 parallel slow queries.
	const (
		nquery      = 50
		sleepMillis = 25
		nbatch      = 2
	)
	var wg sync.WaitGroup
	for batch := 0; batch < nbatch; batch++ {
		for i := 0; i < nquery; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var op string
				if err := stmt.QueryRow("sleep", sleepMillis).Scan(&op); err != nil && err != ErrNoRows {
					t.Error(err)
				}
			}()
		}
		// Sleep for twice the expected length of time for the
		// batch of 50 queries above to finish before starting
		// the next round.
		time.Sleep(2 * sleepMillis * time.Millisecond)
	}
	wg.Wait()

	if g, w := db.numFreeConns(), 10; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	if n := db.numDepsPollUntil(20, time.Second); n > 20 {
		t.Errorf("number of dependencies = %d; expected <= 20", n)
		db.dumpDeps(t)
	}

	driver.mu.Lock()
	opens := driver.openCount - opens0
	closes := driver.closeCount - closes0
	driver.mu.Unlock()

	if opens > 10 {
		t.Logf("open calls = %d", opens)
		t.Logf("close calls = %d", closes)
		t.Errorf("db connections opened = %d; want <= 10", opens)
		db.dumpDeps(t)
	}

	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}

	if g, w := db.numFreeConns(), 10; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	if n := db.numDepsPollUntil(10, time.Second); n > 10 {
		t.Errorf("number of dependencies = %d; expected <= 10", n)
		db.dumpDeps(t)
	}

	db.SetMaxOpenConns(5)

	if g, w := db.numFreeConns(), 5; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	if n := db.numDepsPollUntil(5, time.Second); n > 5 {
		t.Errorf("number of dependencies = %d; expected 0", n)
		db.dumpDeps(t)
	}

	db.SetMaxOpenConns(0)

	if g, w := db.numFreeConns(), 5; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	if n := db.numDepsPollUntil(5, time.Second); n > 5 {
		t.Errorf("number of dependencies = %d; expected 0", n)
		db.dumpDeps(t)
	}

	db.clearAllConns(t)
}

// Issue 9453: tests that SetMaxOpenConns can be lowered at runtime
// and affects the subsequent release of connections.
func TestMaxOpenConnsOnBusy(t *testing.T) {
	defer setHookpostCloseConn(nil)
	setHookpostCloseConn(func(_ *fakeConn, err error) {
		if err != nil {
			t.Errorf("Error closing fakeConn: %v", err)
		}
	})

	db := newTestDB(t, "magicquery")
	defer closeDB(t, db)

	db.SetMaxOpenConns(3)

	ctx := context.Background()

	conn0, err := db.conn(ctx, cachedOrNewConn)
	if err != nil {
		t.Fatalf("db open conn fail: %v", err)
	}

	conn1, err := db.conn(ctx, cachedOrNewConn)
	if err != nil {
		t.Fatalf("db open conn fail: %v", err)
	}

	conn2, err := db.conn(ctx, cachedOrNewConn)
	if err != nil {
		t.Fatalf("db open conn fail: %v", err)
	}

	if g, w := db.numOpen, 3; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	db.SetMaxOpenConns(2)
	if g, w := db.numOpen, 3; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	conn0.releaseConn(nil)
	conn1.releaseConn(nil)
	if g, w := db.numOpen, 2; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	conn2.releaseConn(nil)
	if g, w := db.numOpen, 2; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}
}

// Issue 10886: tests that all connection attempts return when more than
// DB.maxOpen connections are in flight and the first DB.maxOpen fail.
func TestPendingConnsAfterErr(t *testing.T) {
	const (
		maxOpen = 2
		tryOpen = maxOpen*2 + 2
	)

	// No queries will be run.
	db, err := Open("test", fakeDBName)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer closeDB(t, db)
	defer func() {
		for k, v := range db.lastPut {
			t.Logf("%p: %v", k, v)
		}
	}()

	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(0)

	errOffline := errors.New("db offline")

	defer func() { setHookOpenErr(nil) }()

	errs := make(chan error, tryOpen)

	var opening sync.WaitGroup
	opening.Add(tryOpen)

	setHookOpenErr(func() error {
		// Wait for all connections to enqueue.
		opening.Wait()
		return errOffline
	})

	for i := 0; i < tryOpen; i++ {
		go func() {
			opening.Done() // signal one connection is in flight
			_, err := db.Exec("will never run")
			errs <- err
		}()
	}

	opening.Wait() // wait for all workers to begin running

	const timeout = 5 * time.Second
	to := time.NewTimer(timeout)
	defer to.Stop()

	// check that all connections fail without deadlock
	for i := 0; i < tryOpen; i++ {
		select {
		case err := <-errs:
			if got, want := err, errOffline; got != want {
				t.Errorf("unexpected err: got %v, want %v", got, want)
			}
		case <-to.C:
			t.Fatalf("orphaned connection request(s), still waiting after %v", timeout)
		}
	}

	// Wait a reasonable time for the database to close all connections.
	tick := time.NewTicker(3 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			db.mu.Lock()
			if db.numOpen == 0 {
				db.mu.Unlock()
				return
			}
			db.mu.Unlock()
		case <-to.C:
			// Closing the database will check for numOpen and fail the test.
			return
		}
	}
}

func TestSingleOpenConn(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	db.SetMaxOpenConns(1)

	rows, err := db.Query("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	// shouldn't deadlock
	rows, err = db.Query("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStats(t *testing.T) {
	db := newTestDB(t, "people")
	stats := db.Stats()
	if got := stats.OpenConnections; got != 1 {
		t.Errorf("stats.OpenConnections = %d; want 1", got)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()

	closeDB(t, db)
	stats = db.Stats()
	if got := stats.OpenConnections; got != 0 {
		t.Errorf("stats.OpenConnections = %d; want 0", got)
	}
}

func TestConnMaxLifetime(t *testing.T) {
	t0 := time.Unix(1000000, 0)
	offset := time.Duration(0)

	nowFunc = func() time.Time { return t0.Add(offset) }
	defer func() { nowFunc = time.Now }()

	db := newTestDB(t, "magicquery")
	defer closeDB(t, db)

	driver := db.driver.(*fakeDriver)

	// Force the number of open connections to 0 so we can get an accurate
	// count for the test
	db.clearAllConns(t)

	driver.mu.Lock()
	opens0 := driver.openCount
	closes0 := driver.closeCount
	driver.mu.Unlock()

	db.SetMaxIdleConns(10)
	db.SetMaxOpenConns(10)

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	offset = time.Second
	tx2, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}

	tx.Commit()
	tx2.Commit()

	driver.mu.Lock()
	opens := driver.openCount - opens0
	closes := driver.closeCount - closes0
	driver.mu.Unlock()

	if opens != 2 {
		t.Errorf("opens = %d; want 2", opens)
	}
	if closes != 0 {
		t.Errorf("closes = %d; want 0", closes)
	}
	if g, w := db.numFreeConns(), 2; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	// Expire first conn
	offset = time.Second * 11
	db.SetConnMaxLifetime(time.Second * 10)
	if err != nil {
		t.Fatal(err)
	}

	tx, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx2, err = db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	tx2.Commit()

	driver.mu.Lock()
	opens = driver.openCount - opens0
	closes = driver.closeCount - closes0
	driver.mu.Unlock()

	if opens != 3 {
		t.Errorf("opens = %d; want 3", opens)
	}
	if closes != 1 {
		t.Errorf("closes = %d; want 1", closes)
	}
}

// golang.org/issue/5323
func TestStmtCloseDeps(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping in short mode")
	}
	defer setHookpostCloseConn(nil)
	setHookpostCloseConn(func(_ *fakeConn, err error) {
		if err != nil {
			t.Errorf("Error closing fakeConn: %v", err)
		}
	})

	db := newTestDB(t, "magicquery")
	defer closeDB(t, db)

	driver := db.driver.(*fakeDriver)

	driver.mu.Lock()
	opens0 := driver.openCount
	closes0 := driver.closeCount
	driver.mu.Unlock()
	openDelta0 := opens0 - closes0

	stmt, err := db.Prepare("SELECT|magicquery|op|op=?,millis=?")
	if err != nil {
		t.Fatal(err)
	}

	// Start 50 parallel slow queries.
	const (
		nquery      = 50
		sleepMillis = 25
		nbatch      = 2
	)
	var wg sync.WaitGroup
	for batch := 0; batch < nbatch; batch++ {
		for i := 0; i < nquery; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				var op string
				if err := stmt.QueryRow("sleep", sleepMillis).Scan(&op); err != nil && err != ErrNoRows {
					t.Error(err)
				}
			}()
		}
		// Sleep for twice the expected length of time for the
		// batch of 50 queries above to finish before starting
		// the next round.
		time.Sleep(2 * sleepMillis * time.Millisecond)
	}
	wg.Wait()

	if g, w := db.numFreeConns(), 2; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	if n := db.numDepsPollUntil(4, time.Second); n > 4 {
		t.Errorf("number of dependencies = %d; expected <= 4", n)
		db.dumpDeps(t)
	}

	driver.mu.Lock()
	opens := driver.openCount - opens0
	closes := driver.closeCount - closes0
	openDelta := (driver.openCount - driver.closeCount) - openDelta0
	driver.mu.Unlock()

	if openDelta > 2 {
		t.Logf("open calls = %d", opens)
		t.Logf("close calls = %d", closes)
		t.Logf("open delta = %d", openDelta)
		t.Errorf("db connections opened = %d; want <= 2", openDelta)
		db.dumpDeps(t)
	}

	if len(stmt.css) > nquery {
		t.Errorf("len(stmt.css) = %d; want <= %d", len(stmt.css), nquery)
	}

	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}

	if g, w := db.numFreeConns(), 2; g != w {
		t.Errorf("free conns = %d; want %d", g, w)
	}

	if n := db.numDepsPollUntil(2, time.Second); n > 2 {
		t.Errorf("number of dependencies = %d; expected <= 2", n)
		db.dumpDeps(t)
	}

	db.clearAllConns(t)
}

// golang.org/issue/5046
func TestCloseConnBeforeStmts(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	defer setHookpostCloseConn(nil)
	setHookpostCloseConn(func(_ *fakeConn, err error) {
		if err != nil {
			t.Errorf("Error closing fakeConn: %v; from %s", err, stack())
			db.dumpDeps(t)
			t.Errorf("DB = %#v", db)
		}
	})

	stmt, err := db.Prepare("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}

	if len(db.freeConn) != 1 {
		t.Fatalf("expected 1 freeConn; got %d", len(db.freeConn))
	}
	dc := db.freeConn[0]
	if dc.closed {
		t.Errorf("conn shouldn't be closed")
	}

	if n := len(dc.openStmt); n != 1 {
		t.Errorf("driverConn num openStmt = %d; want 1", n)
	}
	err = db.Close()
	if err != nil {
		t.Errorf("db Close = %v", err)
	}
	if !dc.closed {
		t.Errorf("after db.Close, driverConn should be closed")
	}
	if n := len(dc.openStmt); n != 0 {
		t.Errorf("driverConn num openStmt = %d; want 0", n)
	}

	err = stmt.Close()
	if err != nil {
		t.Errorf("Stmt close = %v", err)
	}

	if !dc.closed {
		t.Errorf("conn should be closed")
	}
	if dc.ci != nil {
		t.Errorf("after Stmt Close, driverConn's Conn interface should be nil")
	}
}

// golang.org/issue/5283: don't release the Rows' connection in Close
// before calling Stmt.Close.
func TestRowsCloseOrder(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	db.SetMaxIdleConns(0)
	setStrictFakeConnClose(t)
	defer setStrictFakeConnClose(nil)

	rows, err := db.Query("SELECT|people|age,name|")
	if err != nil {
		t.Fatal(err)
	}
	err = rows.Close()
	if err != nil {
		t.Fatal(err)
	}
}

func TestRowsImplicitClose(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	rows, err := db.Query("SELECT|people|age,name|")
	if err != nil {
		t.Fatal(err)
	}

	want, fail := 2, errors.New("fail")
	r := rows.rowsi.(*rowsCursor)
	r.errPos, r.err = want, fail

	got := 0
	for rows.Next() {
		got++
	}
	if got != want {
		t.Errorf("got %d rows, want %d", got, want)
	}
	if err := rows.Err(); err != fail {
		t.Errorf("got error %v, want %v", err, fail)
	}
	if !r.closed {
		t.Errorf("r.closed is false, want true")
	}
}

func TestStmtCloseOrder(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	db.SetMaxIdleConns(0)
	setStrictFakeConnClose(t)
	defer setStrictFakeConnClose(nil)

	_, err := db.Query("SELECT|non_existent|name|")
	if err == nil {
		t.Fatal("Querying non-existent table should fail")
	}
}

// Test cases where there's more than maxBadConnRetries bad connections in the
// pool (issue 8834)
func TestManyErrBadConn(t *testing.T) {
	manyErrBadConnSetup := func() *DB {
		db := newTestDB(t, "people")

		nconn := maxBadConnRetries + 1
		db.SetMaxIdleConns(nconn)
		db.SetMaxOpenConns(nconn)
		// open enough connections
		func() {
			for i := 0; i < nconn; i++ {
				rows, err := db.Query("SELECT|people|age,name|")
				if err != nil {
					t.Fatal(err)
				}
				defer rows.Close()
			}
		}()

		db.mu.Lock()
		defer db.mu.Unlock()
		if db.numOpen != nconn {
			t.Fatalf("unexpected numOpen %d (was expecting %d)", db.numOpen, nconn)
		} else if len(db.freeConn) != nconn {
			t.Fatalf("unexpected len(db.freeConn) %d (was expecting %d)", len(db.freeConn), nconn)
		}
		for _, conn := range db.freeConn {
			conn.ci.(*fakeConn).stickyBad = true
		}
		return db
	}

	// Query
	db := manyErrBadConnSetup()
	defer closeDB(t, db)
	rows, err := db.Query("SELECT|people|age,name|")
	if err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}

	// Exec
	db = manyErrBadConnSetup()
	defer closeDB(t, db)
	_, err = db.Exec("INSERT|people|name=Julia,age=19")
	if err != nil {
		t.Fatal(err)
	}

	// Begin
	db = manyErrBadConnSetup()
	defer closeDB(t, db)
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	// Prepare
	db = manyErrBadConnSetup()
	defer closeDB(t, db)
	stmt, err := db.Prepare("SELECT|people|age,name|")
	if err != nil {
		t.Fatal(err)
	}
	if err = stmt.Close(); err != nil {
		t.Fatal(err)
	}
}

// golang.org/issue/5718
func TestErrBadConnReconnect(t *testing.T) {
	db := newTestDB(t, "foo")
	defer closeDB(t, db)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")

	simulateBadConn := func(name string, hook *func() bool, op func() error) {
		broken, retried := false, false
		numOpen := db.numOpen

		// simulate a broken connection on the first try
		*hook = func() bool {
			if !broken {
				broken = true
				return true
			}
			retried = true
			return false
		}

		if err := op(); err != nil {
			t.Errorf(name+": %v", err)
			return
		}

		if !broken || !retried {
			t.Error(name + ": Failed to simulate broken connection")
		}
		*hook = nil

		if numOpen != db.numOpen {
			t.Errorf(name+": leaked %d connection(s)!", db.numOpen-numOpen)
			numOpen = db.numOpen
		}
	}

	// db.Exec
	dbExec := func() error {
		_, err := db.Exec("INSERT|t1|name=?,age=?,dead=?", "Gordon", 3, true)
		return err
	}
	simulateBadConn("db.Exec prepare", &hookPrepareBadConn, dbExec)
	simulateBadConn("db.Exec exec", &hookExecBadConn, dbExec)

	// db.Query
	dbQuery := func() error {
		rows, err := db.Query("SELECT|t1|age,name|")
		if err == nil {
			err = rows.Close()
		}
		return err
	}
	simulateBadConn("db.Query prepare", &hookPrepareBadConn, dbQuery)
	simulateBadConn("db.Query query", &hookQueryBadConn, dbQuery)

	// db.Prepare
	simulateBadConn("db.Prepare", &hookPrepareBadConn, func() error {
		stmt, err := db.Prepare("INSERT|t1|name=?,age=?,dead=?")
		if err != nil {
			return err
		}
		stmt.Close()
		return nil
	})

	// Provide a way to force a re-prepare of a statement on next execution
	forcePrepare := func(stmt *Stmt) {
		stmt.css = nil
	}

	// stmt.Exec
	stmt1, err := db.Prepare("INSERT|t1|name=?,age=?,dead=?")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt1.Close()
	// make sure we must prepare the stmt first
	forcePrepare(stmt1)

	stmtExec := func() error {
		_, err := stmt1.Exec("Gopher", 3, false)
		return err
	}
	simulateBadConn("stmt.Exec prepare", &hookPrepareBadConn, stmtExec)
	simulateBadConn("stmt.Exec exec", &hookExecBadConn, stmtExec)

	// stmt.Query
	stmt2, err := db.Prepare("SELECT|t1|age,name|")
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt2.Close()
	// make sure we must prepare the stmt first
	forcePrepare(stmt2)

	stmtQuery := func() error {
		rows, err := stmt2.Query()
		if err == nil {
			err = rows.Close()
		}
		return err
	}
	simulateBadConn("stmt.Query prepare", &hookPrepareBadConn, stmtQuery)
	simulateBadConn("stmt.Query exec", &hookQueryBadConn, stmtQuery)
}

// golang.org/issue/11264
func TestTxEndBadConn(t *testing.T) {
	db := newTestDB(t, "foo")
	defer closeDB(t, db)
	db.SetMaxIdleConns(0)
	exec(t, db, "CREATE|t1|name=string,age=int32,dead=bool")
	db.SetMaxIdleConns(1)

	simulateBadConn := func(name string, hook *func() bool, op func() error) {
		broken := false
		numOpen := db.numOpen

		*hook = func() bool {
			if !broken {
				broken = true
			}
			return broken
		}

		if err := op(); err != driver.ErrBadConn {
			t.Errorf(name+": %v", err)
			return
		}

		if !broken {
			t.Error(name + ": Failed to simulate broken connection")
		}
		*hook = nil

		if numOpen != db.numOpen {
			t.Errorf(name+": leaked %d connection(s)!", db.numOpen-numOpen)
		}
	}

	// db.Exec
	dbExec := func(endTx func(tx *Tx) error) func() error {
		return func() error {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			_, err = tx.Exec("INSERT|t1|name=?,age=?,dead=?", "Gordon", 3, true)
			if err != nil {
				return err
			}
			return endTx(tx)
		}
	}
	simulateBadConn("db.Tx.Exec commit", &hookCommitBadConn, dbExec((*Tx).Commit))
	simulateBadConn("db.Tx.Exec rollback", &hookRollbackBadConn, dbExec((*Tx).Rollback))

	// db.Query
	dbQuery := func(endTx func(tx *Tx) error) func() error {
		return func() error {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			rows, err := tx.Query("SELECT|t1|age,name|")
			if err == nil {
				err = rows.Close()
			} else {
				return err
			}
			return endTx(tx)
		}
	}
	simulateBadConn("db.Tx.Query commit", &hookCommitBadConn, dbQuery((*Tx).Commit))
	simulateBadConn("db.Tx.Query rollback", &hookRollbackBadConn, dbQuery((*Tx).Rollback))
}

type concurrentTest interface {
	init(t testing.TB, db *DB)
	finish(t testing.TB)
	test(t testing.TB) error
}

type concurrentDBQueryTest struct {
	db *DB
}

func (c *concurrentDBQueryTest) init(t testing.TB, db *DB) {
	c.db = db
}

func (c *concurrentDBQueryTest) finish(t testing.TB) {
	c.db = nil
}

func (c *concurrentDBQueryTest) test(t testing.TB) error {
	rows, err := c.db.Query("SELECT|people|name|")
	if err != nil {
		t.Error(err)
		return err
	}
	var name string
	for rows.Next() {
		rows.Scan(&name)
	}
	rows.Close()
	return nil
}

type concurrentDBExecTest struct {
	db *DB
}

func (c *concurrentDBExecTest) init(t testing.TB, db *DB) {
	c.db = db
}

func (c *concurrentDBExecTest) finish(t testing.TB) {
	c.db = nil
}

func (c *concurrentDBExecTest) test(t testing.TB) error {
	_, err := c.db.Exec("NOSERT|people|name=Chris,age=?,photo=CPHOTO,bdate=?", 3, chrisBirthday)
	if err != nil {
		t.Error(err)
		return err
	}
	return nil
}

type concurrentStmtQueryTest struct {
	db   *DB
	stmt *Stmt
}

func (c *concurrentStmtQueryTest) init(t testing.TB, db *DB) {
	c.db = db
	var err error
	c.stmt, err = db.Prepare("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}
}

func (c *concurrentStmtQueryTest) finish(t testing.TB) {
	if c.stmt != nil {
		c.stmt.Close()
		c.stmt = nil
	}
	c.db = nil
}

func (c *concurrentStmtQueryTest) test(t testing.TB) error {
	rows, err := c.stmt.Query()
	if err != nil {
		t.Errorf("error on query:  %v", err)
		return err
	}

	var name string
	for rows.Next() {
		rows.Scan(&name)
	}
	rows.Close()
	return nil
}

type concurrentStmtExecTest struct {
	db   *DB
	stmt *Stmt
}

func (c *concurrentStmtExecTest) init(t testing.TB, db *DB) {
	c.db = db
	var err error
	c.stmt, err = db.Prepare("NOSERT|people|name=Chris,age=?,photo=CPHOTO,bdate=?")
	if err != nil {
		t.Fatal(err)
	}
}

func (c *concurrentStmtExecTest) finish(t testing.TB) {
	if c.stmt != nil {
		c.stmt.Close()
		c.stmt = nil
	}
	c.db = nil
}

func (c *concurrentStmtExecTest) test(t testing.TB) error {
	_, err := c.stmt.Exec(3, chrisBirthday)
	if err != nil {
		t.Errorf("error on exec:  %v", err)
		return err
	}
	return nil
}

type concurrentTxQueryTest struct {
	db *DB
	tx *Tx
}

func (c *concurrentTxQueryTest) init(t testing.TB, db *DB) {
	c.db = db
	var err error
	c.tx, err = c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
}

func (c *concurrentTxQueryTest) finish(t testing.TB) {
	if c.tx != nil {
		c.tx.Rollback()
		c.tx = nil
	}
	c.db = nil
}

func (c *concurrentTxQueryTest) test(t testing.TB) error {
	rows, err := c.db.Query("SELECT|people|name|")
	if err != nil {
		t.Error(err)
		return err
	}
	var name string
	for rows.Next() {
		rows.Scan(&name)
	}
	rows.Close()
	return nil
}

type concurrentTxExecTest struct {
	db *DB
	tx *Tx
}

func (c *concurrentTxExecTest) init(t testing.TB, db *DB) {
	c.db = db
	var err error
	c.tx, err = c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
}

func (c *concurrentTxExecTest) finish(t testing.TB) {
	if c.tx != nil {
		c.tx.Rollback()
		c.tx = nil
	}
	c.db = nil
}

func (c *concurrentTxExecTest) test(t testing.TB) error {
	_, err := c.tx.Exec("NOSERT|people|name=Chris,age=?,photo=CPHOTO,bdate=?", 3, chrisBirthday)
	if err != nil {
		t.Error(err)
		return err
	}
	return nil
}

type concurrentTxStmtQueryTest struct {
	db   *DB
	tx   *Tx
	stmt *Stmt
}

func (c *concurrentTxStmtQueryTest) init(t testing.TB, db *DB) {
	c.db = db
	var err error
	c.tx, err = c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	c.stmt, err = c.tx.Prepare("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}
}

func (c *concurrentTxStmtQueryTest) finish(t testing.TB) {
	if c.stmt != nil {
		c.stmt.Close()
		c.stmt = nil
	}
	if c.tx != nil {
		c.tx.Rollback()
		c.tx = nil
	}
	c.db = nil
}

func (c *concurrentTxStmtQueryTest) test(t testing.TB) error {
	rows, err := c.stmt.Query()
	if err != nil {
		t.Errorf("error on query:  %v", err)
		return err
	}

	var name string
	for rows.Next() {
		rows.Scan(&name)
	}
	rows.Close()
	return nil
}

type concurrentTxStmtExecTest struct {
	db   *DB
	tx   *Tx
	stmt *Stmt
}

func (c *concurrentTxStmtExecTest) init(t testing.TB, db *DB) {
	c.db = db
	var err error
	c.tx, err = c.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	c.stmt, err = c.tx.Prepare("NOSERT|people|name=Chris,age=?,photo=CPHOTO,bdate=?")
	if err != nil {
		t.Fatal(err)
	}
}

func (c *concurrentTxStmtExecTest) finish(t testing.TB) {
	if c.stmt != nil {
		c.stmt.Close()
		c.stmt = nil
	}
	if c.tx != nil {
		c.tx.Rollback()
		c.tx = nil
	}
	c.db = nil
}

func (c *concurrentTxStmtExecTest) test(t testing.TB) error {
	_, err := c.stmt.Exec(3, chrisBirthday)
	if err != nil {
		t.Errorf("error on exec:  %v", err)
		return err
	}
	return nil
}

type concurrentRandomTest struct {
	tests []concurrentTest
}

func (c *concurrentRandomTest) init(t testing.TB, db *DB) {
	c.tests = []concurrentTest{
		new(concurrentDBQueryTest),
		new(concurrentDBExecTest),
		new(concurrentStmtQueryTest),
		new(concurrentStmtExecTest),
		new(concurrentTxQueryTest),
		new(concurrentTxExecTest),
		new(concurrentTxStmtQueryTest),
		new(concurrentTxStmtExecTest),
	}
	for _, ct := range c.tests {
		ct.init(t, db)
	}
}

func (c *concurrentRandomTest) finish(t testing.TB) {
	for _, ct := range c.tests {
		ct.finish(t)
	}
}

func (c *concurrentRandomTest) test(t testing.TB) error {
	ct := c.tests[rand.Intn(len(c.tests))]
	return ct.test(t)
}

func doConcurrentTest(t testing.TB, ct concurrentTest) {
	maxProcs, numReqs := 1, 500
	if testing.Short() {
		maxProcs, numReqs = 4, 50
	}
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(maxProcs))

	db := newTestDB(t, "people")
	defer closeDB(t, db)

	ct.init(t, db)
	defer ct.finish(t)

	var wg sync.WaitGroup
	wg.Add(numReqs)

	reqs := make(chan bool)
	defer close(reqs)

	for i := 0; i < maxProcs*2; i++ {
		go func() {
			for range reqs {
				err := ct.test(t)
				if err != nil {
					wg.Done()
					continue
				}
				wg.Done()
			}
		}()
	}

	for i := 0; i < numReqs; i++ {
		reqs <- true
	}

	wg.Wait()
}

func TestIssue6081(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	drv := db.driver.(*fakeDriver)
	drv.mu.Lock()
	opens0 := drv.openCount
	closes0 := drv.closeCount
	drv.mu.Unlock()

	stmt, err := db.Prepare("SELECT|people|name|")
	if err != nil {
		t.Fatal(err)
	}
	rowsCloseHook = func(rows *Rows, err *error) {
		*err = driver.ErrBadConn
	}
	defer func() { rowsCloseHook = nil }()
	for i := 0; i < 10; i++ {
		rows, err := stmt.Query()
		if err != nil {
			t.Fatal(err)
		}
		rows.Close()
	}
	if n := len(stmt.css); n > 1 {
		t.Errorf("len(css slice) = %d; want <= 1", n)
	}
	stmt.Close()
	if n := len(stmt.css); n != 0 {
		t.Errorf("len(css slice) after Close = %d; want 0", n)
	}

	drv.mu.Lock()
	opens := drv.openCount - opens0
	closes := drv.closeCount - closes0
	drv.mu.Unlock()
	if opens < 9 {
		t.Errorf("opens = %d; want >= 9", opens)
	}
	if closes < 9 {
		t.Errorf("closes = %d; want >= 9", closes)
	}
}

// TestIssue18429 attempts to stress rolling back the transaction from a
// context cancel while simultaneously calling Tx.Rollback. Rolling back from a
// context happens concurrently so tx.rollback and tx.Commit must guard against
// double entry.
//
// In the test, a context is canceled while the query is in process so
// the internal rollback will run concurrently with the explicitly called
// Tx.Rollback.
func TestIssue18429(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)

	ctx := context.Background()
	sem := make(chan bool, 20)
	var wg sync.WaitGroup

	const milliWait = 30

	for i := 0; i < 100; i++ {
		sem <- true
		wg.Add(1)
		go func() {
			defer func() {
				<-sem
				wg.Done()
			}()
			qwait := (time.Duration(rand.Intn(milliWait)) * time.Millisecond).String()

			ctx, cancel := context.WithTimeout(ctx, time.Duration(rand.Intn(milliWait))*time.Millisecond)
			defer cancel()

			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				return
			}
			rows, err := tx.QueryContext(ctx, "WAIT|"+qwait+"|SELECT|people|name|")
			if rows != nil {
				rows.Close()
			}
			// This call will race with the context cancel rollback to complete
			// if the rollback itself isn't guarded.
			tx.Rollback()
		}()
	}
	wg.Wait()
	time.Sleep(milliWait * 3 * time.Millisecond)
}

func TestConcurrency(t *testing.T) {
	doConcurrentTest(t, new(concurrentDBQueryTest))
	doConcurrentTest(t, new(concurrentDBExecTest))
	doConcurrentTest(t, new(concurrentStmtQueryTest))
	doConcurrentTest(t, new(concurrentStmtExecTest))
	doConcurrentTest(t, new(concurrentTxQueryTest))
	doConcurrentTest(t, new(concurrentTxExecTest))
	doConcurrentTest(t, new(concurrentTxStmtQueryTest))
	doConcurrentTest(t, new(concurrentTxStmtExecTest))
	doConcurrentTest(t, new(concurrentRandomTest))
}

func TestConnectionLeak(t *testing.T) {
	db := newTestDB(t, "people")
	defer closeDB(t, db)
	// Start by opening defaultMaxIdleConns
	rows := make([]*Rows, defaultMaxIdleConns)
	// We need to SetMaxOpenConns > MaxIdleConns, so the DB can open
	// a new connection and we can fill the idle queue with the released
	// connections.
	db.SetMaxOpenConns(len(rows) + 1)
	for ii := range rows {
		r, err := db.Query("SELECT|people|name|")
		if err != nil {
			t.Fatal(err)
		}
		r.Next()
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		rows[ii] = r
	}
	// Now we have defaultMaxIdleConns busy connections. Open
	// a new one, but wait until the busy connections are released
	// before returning control to DB.
	drv := db.driver.(*fakeDriver)
	drv.waitCh = make(chan struct{}, 1)
	drv.waitingCh = make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		r, err := db.Query("SELECT|people|name|")
		if err != nil {
			t.Error(err)
			return
		}
		r.Close()
		wg.Done()
	}()
	// Wait until the goroutine we've just created has started waiting.
	<-drv.waitingCh
	// Now close the busy connections. This provides a connection for
	// the blocked goroutine and then fills up the idle queue.
	for _, v := range rows {
		v.Close()
	}
	// At this point we give the new connection to DB. This connection is
	// now useless, since the idle queue is full and there are no pending
	// requests. DB should deal with this situation without leaking the
	// connection.
	drv.waitCh <- struct{}{}
	wg.Wait()
}

// badConn implements a bad driver.Conn, for TestBadDriver.
// The Exec method panics.
type badConn struct{}

func (bc badConn) Prepare(query string) (driver.Stmt, error) {
	return nil, errors.New("badConn Prepare")
}

func (bc badConn) Close() error {
	return nil
}

func (bc badConn) Begin() (driver.Tx, error) {
	return nil, errors.New("badConn Begin")
}

func (bc badConn) Exec(query string, args []driver.Value) (driver.Result, error) {
	panic("badConn.Exec")
}

// badDriver is a driver.Driver that uses badConn.
type badDriver struct{}

func (bd badDriver) Open(name string) (driver.Conn, error) {
	return badConn{}, nil
}

// Issue 15901.
func TestBadDriver(t *testing.T) {
	Register("bad", badDriver{})
	db, err := Open("bad", "ignored")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic")
		} else {
			if want := "badConn.Exec"; r.(string) != want {
				t.Errorf("panic was %v, expected %v", r, want)
			}
		}
	}()
	defer db.Close()
	db.Exec("ignored")
}

type pingDriver struct {
	fails bool
}

type pingConn struct {
	badConn
	driver *pingDriver
}

var pingError = errors.New("Ping failed")

func (pc pingConn) Ping(ctx context.Context) error {
	if pc.driver.fails {
		return pingError
	}
	return nil
}

var _ driver.Pinger = pingConn{}

func (pd *pingDriver) Open(name string) (driver.Conn, error) {
	return pingConn{driver: pd}, nil
}

func TestPing(t *testing.T) {
	driver := &pingDriver{}
	Register("ping", driver)

	db, err := Open("ping", "ignored")
	if err != nil {
		t.Fatal(err)
	}

	if err := db.Ping(); err != nil {
		t.Errorf("err was %#v, expected nil", err)
		return
	}

	driver.fails = true
	if err := db.Ping(); err != pingError {
		t.Errorf("err was %#v, expected pingError", err)
	}
}

func BenchmarkConcurrentDBExec(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentDBExecTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkConcurrentStmtQuery(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentStmtQueryTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkConcurrentStmtExec(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentStmtExecTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkConcurrentTxQuery(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentTxQueryTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkConcurrentTxExec(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentTxExecTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkConcurrentTxStmtQuery(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentTxStmtQueryTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkConcurrentTxStmtExec(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentTxStmtExecTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkConcurrentRandom(b *testing.B) {
	b.ReportAllocs()
	ct := new(concurrentRandomTest)
	for i := 0; i < b.N; i++ {
		doConcurrentTest(b, ct)
	}
}

func BenchmarkManyConcurrentQueries(b *testing.B) {
	b.ReportAllocs()
	// To see lock contention in Go 1.4, 16~ cores and 128~ goroutines are required.
	const parallelism = 16

	db := newTestDB(b, "magicquery")
	defer closeDB(b, db)
	db.SetMaxIdleConns(runtime.GOMAXPROCS(0) * parallelism)

	stmt, err := db.Prepare("SELECT|magicquery|op|op=?,millis=?")
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()

	b.SetParallelism(parallelism)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			rows, err := stmt.Query("sleep", 1)
			if err != nil {
				b.Error(err)
				return
			}
			rows.Close()
		}
	})
}
