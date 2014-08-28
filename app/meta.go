// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"

	"appengine"
	"appengine/memcache"
)

type meta struct {
	JSON []byte `datastore:",noindex"`
}

// ReadMeta reads a metadata value stored in the datastore
// under the given key. The value is stored in JSON format;
// it must be possible to unmarshal the value into v.
// ReadMeta returns datastore.ErrNoSuchEntity if the value
// is missing.
//
// If an error occurs, ReadMeta returns it but also logs the error
// using ctxt.Errorf.
func ReadMeta(ctxt appengine.Context, key string, v interface{}) error {
	var m meta
	if err := ReadData(ctxt, "Meta", key, &m); err != nil {
		return err
	}
	if err := json.Unmarshal(m.JSON, v); err != nil {
		ctxt.Errorf("read meta %s: unmarshal JSON: %v", key, err)
		return err
	}
	return nil
}

// ReadMetaCached is like ReadMeta but consults memcache
// before the datastore and, if the datastore must be used,
// stores the result in memcache for future lookups.
//
// ReadMetaCached should not be used within a transaction,
// because the update of the cache may save an old value.
// More generally, it is probably only safe to use ReadMetaCached
// for values that are either immutable or can be wrong once in a while.
func ReadMetaCached(ctxt appengine.Context, key string, v interface{}) error {
	if it, err := memcache.Get(ctxt, "app.Meta."+key); err == nil {
		if err := json.Unmarshal(it.Value, v); err == nil {
			return nil
		}
	}
	var m meta
	if err := ReadData(ctxt, "Meta", key, &m); err != nil {
		return err
	}
	if err := json.Unmarshal(m.JSON, v); err != nil {
		ctxt.Errorf("read meta %s: unmarshal JSON: %v", key, err)
		return err
	}
	memcache.Set(ctxt, &memcache.Item{Key: "app.Meta." + key, Value: m.JSON})
	return nil
}

// WriteMeta writes a metadata value to the datastore under the given key.
// The value is stored in JSON format: it must be possible to marshal v into JSON.
// The value can be read back using ReadMeta.
//
// If an error occurs, WriteMeta returns it but also logs the error
// using ctxt.Errorf.
func WriteMeta(ctxt appengine.Context, key string, v interface{}) error {
	js, err := json.Marshal(v)
	if err != nil {
		ctxt.Errorf("write meta %s: marshal JSON: %v", key, err)
		return err
	}
	err = WriteData(ctxt, "Meta", key, &meta{JSON: js})
	if err == nil {
		memcache.Delete(ctxt, "app.Meta."+key)
	}
	return err
}

// DeleteMeta deletes the metadata value stored in the datastore under the given key.
//
// If an error occurs, DeleteMeta returns it but also logs the error
// using ctxt.Errorf.
func DeleteMeta(ctxt appengine.Context, key string) error {
	err := DeleteData(ctxt, "Meta", key)
	memcache.Delete(ctxt, "app.Meta."+key)
	return err
}

func init() {
	http.HandleFunc("/admin/app/metaedit", metaedit)
}

var editForm = `<html>
<h1>metadata edit</h1>

<form method="post">
Key: <input type="text" name="key" value="%s">
<br>
Value: <textarea name="value" columns=80 rows=25>%s</textarea>
<br>
<input type="submit" name="op" value="Read">
<input type="submit" name="op" value="Write">
<input type="submit" name="op" value="Delete">
</form>
`

func metaedit(w http.ResponseWriter, req *http.Request) {
	// TODO: XSRF

	ctxt := appengine.NewContext(req)
	key := req.FormValue("key")
	op := req.FormValue("op")
	var value json.RawMessage
	ReadMeta(ctxt, key, &value)

	if req.Method != "GET" {
		switch op {
		case "Read":
			// do nothing
		case "Write":
			value = json.RawMessage([]byte(req.FormValue("value")))
			if err := WriteMeta(ctxt, key, &value); err != nil {
				fmt.Fprintf(w, "failed to write: %v\n", err)
				return
			}
		case "Delete":
			value = nil
			if err := DeleteMeta(ctxt, key); err != nil {
				fmt.Fprintf(w, "failed to delete: %v\n", err)
				return
			}
		}
	}

	var buf bytes.Buffer
	if len(value) > 0 {
		if err := json.Indent(&buf, value, "", "\t"); err != nil {
			buf.Reset()
			fmt.Fprintf(&buf, "malformed JSON")
		}
	}

	fmt.Fprintf(w, editForm, html.EscapeString(key), html.EscapeString(buf.String()))
}
