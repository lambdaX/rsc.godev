// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package commit

import "time"

type Rev struct {
	DV int `dataversion:"1"`

	Repo   string
	Branch string
	Seq    int

	Hash      string
	ShortHash string
	Prev      []string
	Next      []string

	Author      string
	AuthorEmail string
	Time        time.Time

	Log string `datastore:",noindex"`

	Files []File
}

type File struct {
	Op   string
	Name string
}

var initialRoots = map[string]string{
	//	"main/default": "e2e0547ad087293952d76424954c0588ffd17773",
	"main/default":      "f6182e5abf5eb0c762dddbb18f8854b7e350eaeb",
	"go.crypto/default": "b50a7fb49394c272db51587d86e14c73e9b901f5",
	"go.net/default":    "b50a7fb49394c272db51587d86e14c73e9b901f5",
}
