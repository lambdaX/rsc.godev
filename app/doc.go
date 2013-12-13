// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package app implements various convenience functionality
// for App Engine apps.
//
// This package assumes that URLs beginning with /admin have
// been restricted to administrators of the app, such as by using
//
//	handlers:
//	- url: /admin(/.*)?
//	  script: _go_app
//	  login: admin
//	  secure: always
//
// in the app.yaml file.
package app

// BUG(rsc): Eventually, this package should go somewhere importable.
// For now it is tied to the development of the Go Dashboard app.
