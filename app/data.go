// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"time"

	"appengine"
	"appengine/datastore"
	"appengine/delay"
	"appengine/taskqueue"
)

var updaters struct {
	sync.RWMutex
	m     map[string][]reflect.Value
	types map[string]reflect.Type
}

// RegisterDataUpdater registers an updater function for a specific kind of record.
// ReadData calls the updater function after reading (but before returning) a record
// from the datastore
// and WriteData calls the updater before writing a record to the datastore.
//
// The updater function must have type func(*Record) or func(*Record) error.
// The record type may be chosen by the caller, but it must be used consistently:
// the updater for a given kind and the data values passed to ReadData and WriteData
// must all use the same type.
//
// When using an updater function, the record type must be a struct, and the first
// field in the struct must be an int named DV with a "dataversion" field tag giving
// the current data version, as in:
//
//	type MyRecord struct {
//		DV int `dataversion:"1"`
//		... more fields ...
//	}
//
// The DV field is owned by package app and should not be read or written by
// clients. The dataversion tag must be a positive int value.
//
// The app polls the datastore for records written with a smaller dataversion
// than the one used by the current source code. For each such record, it reads
// the record, calls the updaters, and writes the new record. Therefore,
// if a new field is added to a struct and must be populated to a default value
// (or even must be initialized to its zero value for datastore searches),
// it suffices to increment the dataversion, define a new updater (or revise an
// existing one), and redeploy. Package app will refresh the records using the
// updater.
//
// The status of the background updating process is served in the "data updater"
// section on /admin/app/status.
//
func RegisterDataUpdater(kind string, updater interface{}) {
	f := updater
	v := reflect.ValueOf(f)
	if !v.IsValid() || v.Kind() != reflect.Func {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T), need func", kind, f))
	}
	t := v.Type()
	if t.NumIn() != 1 {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T), func must have one input", kind, f))
	}
	in := t.In(0)
	if in.Kind() != reflect.Ptr || in.Elem().Kind() != reflect.Struct {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T), func input must be *struct", kind, f))
	}
	var field reflect.StructField
	if in.Elem().NumField() > 0 {
		field = in.Elem().Field(0)
	}
	if field.Name != "DV" || field.Tag.Get("dataversion") == "" {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T), func input must have DV field with dataversion tag", kind, f))
	}
	n, err := strconv.Atoi(field.Tag.Get("dataversion"))
	if n <= 0 || err != nil {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T), func input is struct with invalid dataversion tag %q", kind, f, field.Tag.Get("dataversion")))
	}

	if t.NumOut() > 1 {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T), func must have at most one output", kind, f))
	}
	if t.NumOut() == 1 && t.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T), func output must be type 'error'", kind, f))
	}

	updaters.Lock()
	defer updaters.Unlock()
	if updaters.m == nil {
		updaters.m = make(map[string][]reflect.Value)
	}
	if updaters.types == nil {
		updaters.types = make(map[string]reflect.Type)
	}
	old := updaters.m[kind]
	if len(old) > 0 && t.In(0) != old[0].Type().In(0) {
		panic(fmt.Sprintf("RegisterDataUpdater(%q, %T) conflicts with previous RegisterDataUpdater(%q, %s)", kind, f, kind, old[0].Type()))
	}
	updaters.m[kind] = append(old, v)
	updaters.types[kind] = in.Elem()
}

func update(ctxt appengine.Context, kind string, data interface{}) error {
	updaters.RLock()
	up := updaters.m[kind]
	updaters.RUnlock()

	t := reflect.TypeOf(data)
	dv, _ := strconv.Atoi(t.Elem().Field(0).Tag.Get("dataversion"))
	if dv == 0 {
		return nil
	}
	v := reflect.ValueOf(data)
	v.Elem().Field(0).SetInt(int64(dv))
	for _, fv := range up {
		if fv.Type().In(0) != t {
			return fmt.Errorf("type mismatch for data kind %q: cannot apply updater %s to %T", kind, fv.Type(), data)
		}
		rv := fv.Call([]reflect.Value{v})
		if len(rv) > 0 {
			err := rv[0].Interface().(error)
			if err != nil {
				ctxt.Errorf("applying updater: %v", err)
				return fmt.Errorf("applying updater: %v", err)
			}
		}
	}
	return nil
}

func DeleteData(ctxt appengine.Context, kind string, key string) error {
	if key == "" {
		ctxt.Errorf("delete datastore %s[%s]: no key", kind, key)
		return fmt.Errorf("missing key")
	}
	k := datastore.NewKey(ctxt, kind, key, 0, nil)
	err := datastore.Delete(ctxt, k)
	if err != nil && err != datastore.ErrNoSuchEntity {
		ctxt.Errorf("delete datastore %s[%s]: %v", kind, key, err)
	}
	return err
}

// ReadData reads a record with the given kind and key from the datastore into data.
// It applies any registered updaters before returning. See RegisterDataUpdater.
// If there is no such record, ReadData returns datastore.ErrNoSuchEntity.
func ReadData(ctxt appengine.Context, kind string, key string, data interface{}) error {
	if key == "" {
		ctxt.Errorf("read datastore %s[%s]: no key", kind, key)
		return fmt.Errorf("missing key")
	}
	k := datastore.NewKey(ctxt, kind, key, 0, nil)
	err := datastore.Get(ctxt, k, data)
	if err == nil {
		err = update(ctxt, kind, data)
	}
	if err != nil && err != datastore.ErrNoSuchEntity {
		ctxt.Errorf("read datastore %s[%s]: %v", kind, key, err)
	}
	return err
}

// WriteData writes a record with the given kind and key to the datastore from data.
// It applies any registered updaters before the write. See RegisterDataUpdater.
func WriteData(ctxt appengine.Context, kind string, key string, data interface{}) error {
	if key == "" {
		ctxt.Errorf("read datastore %s[%s]: no key", kind, key)
		return fmt.Errorf("missing key")
	}
	err := update(ctxt, kind, data)
	if err == nil {
		k := datastore.NewKey(ctxt, kind, key, 0, nil)
		_, err = datastore.Put(ctxt, k, data)
	}
	if err != nil {
		ctxt.Errorf("write datastore %s[%s]: %v", kind, key, err)
	}
	return err
}

type kindType struct {
	kind string
	typ  reflect.Type
}

func init() {
	RegisterStatus("data updater", updateStatus)
	http.HandleFunc("/admin/app/update", startUpdate)
}

func startUpdate(w http.ResponseWriter, req *http.Request) {
	backgroundUpdate(appengine.NewContext(req))
}

func updateStatus(ctxt appengine.Context) string {
	var all []kindType
	updaters.RLock()
	for kind, typ := range updaters.types {
		all = append(all, kindType{kind, typ})
	}
	updaters.RUnlock()

	w := new(bytes.Buffer)
	const chunk = 100000
	for _, kt := range all {
		kind := kt.kind
		typ := kt.typ
		dv, _ := strconv.Atoi(typ.Field(0).Tag.Get("dataversion"))
		keys, err := datastore.NewQuery(kind).
			Filter("DV <", dv).
			KeysOnly().
			Limit(chunk).
			GetAll(ctxt, nil)
		switch {
		case err != nil:
			fmt.Fprintf(w, "%s: error checking update status: %v\n", kind, err)
		case len(keys) == chunk:
			fmt.Fprintf(w, "%s: >=%d remaining to update to DV = %d\n", kind, chunk, dv)
		case len(keys) > 0:
			fmt.Fprintf(w, "%s: %d remaining to update to DV = %d\n", kind, len(keys), dv)
		default:
			fmt.Fprintf(w, "%s: all updated to DV = %d\n", kind, dv)
		}
	}

	return "<pre>" + html.EscapeString(w.String()) + "</pre>\n"
}

func init() {
	Cron("app.update", 5*time.Minute, backgroundUpdate)
}

func backgroundUpdate(ctxt appengine.Context) error {
	var kinds []string
	updaters.RLock()
	for kind := range updaters.types {
		kinds = append(kinds, kind)
	}
	updaters.RUnlock()

	ctxt.Infof("background scan %v", kinds)
	for _, kind := range kinds {
		laterUpdateKind.Call(ctxt, kind)
	}
	return nil
}

var laterUpdateKind *delay.Function

func init() {
	laterUpdateKind = delay.Func("app.backgroundUpdateKind", backgroundUpdateKind)
}

func backgroundUpdateKind(ctxt appengine.Context, kind string) {
	ctxt.Infof("background update %v", kind)
	if !Lock(ctxt, "app.update."+kind, 15*time.Minute) {
		ctxt.Errorf("update in progress")
		return
	}
	defer Unlock(ctxt, "app.update."+kind)

	updaters.RLock()
	t := updaters.types[kind]
	updaters.RUnlock()
	dv, _ := strconv.Atoi(t.Field(0).Tag.Get("dataversion"))

	const chunk = 1000
	keys, err := datastore.NewQuery(kind).
		Filter("DV <", dv).
		KeysOnly().
		Limit(chunk).
		GetAll(ctxt, nil)
	if err != nil {
		ctxt.Errorf("search for %s with DV < %d: %v", kind, dv, err)
		return
	}

	if len(keys) == 0 {
		ctxt.Errorf("nothing to update")
	}

	numError := 0
	for _, key := range keys {
		err := Transaction(ctxt, func(ctxt appengine.Context) error {
			v := reflect.New(t)
			if err := ReadData(ctxt, kind, key.StringID(), v.Interface()); err != nil {
				return err
			}
			return WriteData(ctxt, kind, key.StringID(), v.Interface())
		})
		if err != nil {
			numError++
		}
	}

	if len(keys) == chunk && numError < chunk {
		laterUpdateKind.Call(ctxt, kind)
	}
}

var scan = struct {
	sync.RWMutex
	m map[string]func(appengine.Context, string, string) error
}{
	m: make(map[string]func(appengine.Context, string, string) error),
}

// Scan registers a datastore trigger function.
// Periodically, the app will scan the datastore for records matching
// the query q, and for each such record will create a task
// to run the function f.
func ScanData(name string, period time.Duration, q *datastore.Query, f func(ctxt appengine.Context, kind, key string) error) {
	scan.Lock()
	defer scan.Unlock()
	if scan.m[name] != nil {
		panic("app.ScanData: multiple registrations for name: " + name)
	}
	scan.m[name] = f
	Cron("app.scan."+name, period, func(ctxt appengine.Context) error {
		scanData(ctxt, name, period, q, f)
		return nil
	})
}

// The only time we make a cron task retry is if the cron function
// reports ErrMoreCron, meaning it wants more work. In that case,
// we want the retry to happen quickly. Especially if this happens
// multiple times, we don't want the backoff to grow to something huge.
// We impose a retry limit of 1000 retries to avoid true runaways.
var scanRetry = &taskqueue.RetryOptions{
	RetryLimit: 10,
	MinBackoff: 1 * time.Second,
	MaxBackoff: 1 * time.Hour,
}

func scanData(ctxt appengine.Context, name string, period time.Duration, q *datastore.Query, f func(ctxt appengine.Context, kind, key string) error) {
	// TODO: Handle even more keys by using cursor.
	const chunk = 100000

	keys, err := q.Limit(chunk).KeysOnly().GetAll(ctxt, nil)
	if err != nil {
		ctxt.Errorf("scandata %q: %v", name, err)
		return
	}

	const maxBatch = 100
	for _, key := range keys {
		Task(ctxt, fmt.Sprintf("app.scandata.%s.%s", key.Kind(), key.StringID()), "scandata", name, key.Kind(), key.StringID())
	}
}

func init() {
	TaskFunc("scandata", scanDataExec, "scandata", scanRetry)
}

func scanDataExec(ctxt appengine.Context, name, kind, key string) {
	scan.RLock()
	f := scan.m[name]
	scan.RUnlock()

	ctxt.Infof("scanData %q %q %q", name, kind, key)

	if f == nil || name == "" || kind == "" || key == "" {
		ctxt.Errorf("missing parameters")
	}

	if err := f(ctxt, kind, key); err != nil {
		ctxt.Errorf("scandata %q %q %q: %v", name, kind, key, err)
	}
}

