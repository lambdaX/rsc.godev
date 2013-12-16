// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"encoding/json"

	"appengine"
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
	return WriteData(ctxt, "Meta", key, &meta{JSON: js})
}

// DeleteMeta deletes the metadata value stored in the datastore under the given key.
//
// If an error occurs, DeleteMeta returns it but also logs the error
// using ctxt.Errorf.
func DeleteMeta(ctxt appengine.Context, key string) error {
	return DeleteData(ctxt, "Meta", key)
}
