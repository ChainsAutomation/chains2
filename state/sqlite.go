package state

/*
#cgo pkg-config: sqlite3
#include <sqlite3.h>
#include <stdlib.h>
static int bind_string(sqlite3_stmt *s, int i, const char *value) {
 return sqlite3_bind_text(s, i, value, -1, SQLITE_TRANSIENT);
}
*/
import "C"

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"unsafe"
)

type database struct{ handle *C.sqlite3 }

func openDB(path string) (*database, error) {
	name := C.CString(path)
	defer C.free(unsafe.Pointer(name))
	d := &database{}
	if rc := C.sqlite3_open(name, &d.handle); rc != C.SQLITE_OK {
		err := d.err()
		d.close()
		return nil, err
	}
	err := d.exec(`PRAGMA journal_mode=WAL;
 CREATE TABLE IF NOT EXISTS entries (kind TEXT NOT NULL, owner TEXT NOT NULL, name TEXT NOT NULL, value TEXT NOT NULL, PRIMARY KEY(kind,owner,name));
 CREATE TABLE IF NOT EXISTS seen (id TEXT PRIMARY KEY, received INTEGER NOT NULL DEFAULT (unixepoch()));
 CREATE INDEX IF NOT EXISTS seen_received ON seen(received);
 CREATE TABLE IF NOT EXISTS dashboard_catalog (id INTEGER PRIMARY KEY CHECK(id=1), value TEXT NOT NULL);`)
	if err != nil {
		d.close()
		return nil, err
	}
	version, err := d.statement("PRAGMA user_version")
	if err != nil {
		d.close()
		return nil, err
	}
	needsMigration := C.sqlite3_step(version) == C.SQLITE_ROW && C.sqlite3_column_int(version, 0) < 3
	C.sqlite3_finalize(version)
	if needsMigration {
		if err := d.exec("BEGIN IMMEDIATE; DELETE FROM entries WHERE kind='service'; PRAGMA user_version=3; COMMIT;"); err != nil {
			d.close()
			return nil, err
		}
	}
	return d, nil
}

func (d *database) close()     { C.sqlite3_close(d.handle) }
func (d *database) err() error { return errors.New(C.GoString(C.sqlite3_errmsg(d.handle))) }

func (d *database) exec(query string) error {
	text := C.CString(query)
	defer C.free(unsafe.Pointer(text))
	if C.sqlite3_exec(d.handle, text, nil, nil, nil) != C.SQLITE_OK {
		return d.err()
	}
	return nil
}

func (d *database) statement(query string, args ...string) (*C.sqlite3_stmt, error) {
	text := C.CString(query)
	defer C.free(unsafe.Pointer(text))
	var s *C.sqlite3_stmt
	if C.sqlite3_prepare_v2(d.handle, text, -1, &s, nil) != C.SQLITE_OK {
		return nil, d.err()
	}
	for i, arg := range args {
		value := C.CString(arg)
		rc := C.bind_string(s, C.int(i+1), value)
		C.free(unsafe.Pointer(value))
		if rc != C.SQLITE_OK {
			C.sqlite3_finalize(s)
			return nil, d.err()
		}
	}
	return s, nil
}

func (d *database) run(query string, args ...string) error {
	s, err := d.statement(query, args...)
	if err != nil {
		return err
	}
	defer C.sqlite3_finalize(s)
	if C.sqlite3_step(s) != C.SQLITE_DONE {
		return d.err()
	}
	return nil
}

func (d *database) put(kind, owner, name string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return d.run("INSERT OR REPLACE INTO entries(kind,owner,name,value) VALUES(?,?,?,?)", kind, owner, name, string(raw))
}

func (d *database) commit(id string, u Update) (bool, error) {
	if err := d.exec("BEGIN IMMEDIATE"); err != nil {
		return false, err
	}
	defer d.exec("ROLLBACK")
	if err := d.run("INSERT OR IGNORE INTO seen(id) VALUES(?)", id); err != nil {
		return false, err
	}
	if C.sqlite3_changes(d.handle) == 0 {
		return false, nil
	}
	if u.registration != nil {
		if err := d.put("service", u.registration.UUID, "", u.registration); err != nil {
			return false, err
		}
	}
	if u.placement != nil {
		if err := d.put("placement", u.placement.address, "", u.placement.override); err != nil {
			return false, err
		}
	}
	for address, attributes := range u.Devices {
		for key, attribute := range attributes {
			if err := d.put("attribute", address, key, attribute); err != nil {
				return false, err
			}
		}
	}
	if err := d.put("revision", "", "", u.Revision); err != nil {
		return false, err
	}
	if u.Revision%1024 == 0 {
		if err := d.exec("DELETE FROM seen WHERE received < unixepoch()-86400"); err != nil {
			return false, err
		}
	}
	return true, d.exec("COMMIT")
}

func (d *database) load(snapshot *Snapshot, placements map[string]PlacementOverride) error {
	s, err := d.statement("SELECT kind,owner,name,value FROM entries")
	if err != nil {
		return err
	}
	defer C.sqlite3_finalize(s)
	for {
		switch C.sqlite3_step(s) {
		case C.SQLITE_DONE:
			return nil
		case C.SQLITE_ROW:
			col := func(i int) string { return C.GoString((*C.char)(unsafe.Pointer(C.sqlite3_column_text(s, C.int(i))))) }
			if col(0) == "placement" {
				var saved PlacementOverride
				if err := json.Unmarshal([]byte(col(3)), &saved); err != nil {
					return err
				}
				placements[col(1)] = saved
				continue
			}
			if err := loadEntry(snapshot, col(0), col(1), col(2), col(3)); err != nil {
				return err
			}
		default:
			return d.err()
		}
	}
}

func loadEntry(s *Snapshot, kind, owner, name, value string) error {
	switch kind {
	case "revision":
		rev, err := strconv.ParseUint(value, 10, 64)
		s.Revision = rev
		return err
	case "service":
		service := s.Services[owner]
		if err := json.Unmarshal([]byte(value), &service); err != nil {
			return err
		}
		s.Services[owner] = service
	case "attribute":
		var a Attribute
		if err := json.Unmarshal([]byte(value), &a); err != nil {
			return err
		}
		if s.Devices[owner] == nil {
			s.Devices[owner] = map[string]Attribute{}
		}
		s.Devices[owner][name] = a
	default:
		return fmt.Errorf("unknown persisted entry kind %q", kind)
	}
	return nil
}

// DashboardCatalog reads dashboard configuration outside the replicated State tree.
func (a *Authority) DashboardCatalog() (json.RawMessage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.db.dashboardCatalog()
}

func (d *database) dashboardCatalog() (json.RawMessage, error) {
	s, err := d.statement("SELECT value FROM dashboard_catalog WHERE id=1")
	if err != nil {
		return nil, err
	}
	defer C.sqlite3_finalize(s)
	switch C.sqlite3_step(s) {
	case C.SQLITE_DONE:
		return nil, nil
	case C.SQLITE_ROW:
		return json.RawMessage(C.GoString((*C.char)(unsafe.Pointer(C.sqlite3_column_text(s, 0))))), nil
	default:
		return nil, d.err()
	}
}

// EditDashboardCatalog serializes validation and replacement with other catalog edits.
func (a *Authority) EditDashboardCatalog(edit func(json.RawMessage) (json.RawMessage, error)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	current, err := a.db.dashboardCatalog()
	if err != nil {
		return err
	}
	next, err := edit(current)
	if err != nil {
		return err
	}
	return a.db.run("INSERT OR REPLACE INTO dashboard_catalog(id,value) VALUES(1,?)", string(next))
}
