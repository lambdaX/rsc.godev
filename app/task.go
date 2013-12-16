// Copyright 2013 The Go Authors.  All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package app

import (
	"bytes"
	"encoding/gob"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	
	"appengine"
	"appengine/taskqueue"
)

var taskfuncs = struct {
	sync.RWMutex
	m map[string]*taskFunc
}{
	m: make(map[string]*taskFunc),
}

type taskFunc struct {
	name  string
	fn    reflect.Value
	queue string
	retry *taskqueue.RetryOptions
}

// TaskFunc registers a task-handling function.
// The name is used to identify the task in future invocations
// and must be unique across all calls to TaskFunc.
//
// The function fn must be a func with a first argument of type appengine.Context.
// The values of the remaining arguments are specified when creating a task.
// The function fn may return a single result of type error; otherwise it should
// have no return values. When invoked as part of executing a task, the task
// is considered to succeed if the function returns a nil error
// (or, in the case of a function with no result, simply returns).
//
// Tasks created with this func use the given queue and retry options.
// (See the App Engine taskqueue API reference for details.)
func TaskFunc(name string, fn interface{}, queue string, retry *taskqueue.RetryOptions) {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func {
		panic("app.TaskFunc: fn is not a function")
	}
	t := v.Type()
	if t.NumIn() < 1 || t.In(0) != reflect.TypeOf((*appengine.Context)(nil)).Elem() {
		panic("app.TaskFunc: fn's first argument is not appengine.Context")
	}
	if t.NumOut() > 1 {
		panic("app.TaskFunc: fn has too many return values")
	}
	if t.NumOut() == 1 && t.Out(0) != reflect.TypeOf((*error)(nil)).Elem() {
		panic(fmt.Sprintf("app.TaskFunc: fn's return value has type %s, need error", t.Out(0)))
	}
	taskfuncs.Lock()
	defer taskfuncs.Unlock()
	if taskfuncs.m[name] != nil {
		panic("app.TaskFunc: multiple registrations for name: " + name)
	}
	tf := &taskFunc{
		name:  name,
		fn:    v,
		queue: queue,
		retry: retry,
	}
	taskfuncs.m[name] = tf
}

// Task creates a new task named taskName.
// At most one such task can exist at a time. As soon as the task succeeds,
// the name is made available for reuse.
//
// When the task is invoked, it will call the function registered as funcName
// (see TaskFunc) and will pass the additional arguments to the function.
// The arguments are checked for validity at the time the task is created.
// If there is a type mismatch, the call to Task will panic.
//
// Task logs and returns an error if a task with the given name has already been
// created and has not yet run successfully. When a task completes successfully,
// a new task with the same name may be created immediately.
// In order to provide immediate reuse semantics, Task stores one lease entity for
// each pending task. Therefore, each call to Task consumes one of the five allowed
// transaction groups in a transaction.
func Task(ctxt appengine.Context, taskName, funcName string, args ...interface{}) error {
	taskfuncs.RLock()
	tf := taskfuncs.m[funcName]
	taskfuncs.RUnlock()
	if tf == nil {
		panic("app.Task: unknown task function name: " + funcName)
	}
	t := tf.fn.Type()
	if len(args) != t.NumIn()-1 {
		panic(fmt.Sprintf("app.TaskFunc: wrong number of arguments for func %q: have %d, want %d", funcName, len(args), t.NumIn()-1))
	}
	var buf bytes.Buffer
	enc := gob.NewEncoder(&buf)
	for i, arg := range args {
		v := reflect.ValueOf(arg)
		if !v.IsValid() {
			panic(fmt.Sprintf("app.TaskFunc: arg %d is nil, need non-nil value", i))
		}
		if !v.Type().AssignableTo(t.In(1 + i)) {
			panic(fmt.Sprintf("app.TaskFunc: arg %d has type %s, not assignable to type %s", i, v.Type(), t.In(1+i)))
		}
		v = v.Convert(t.In(1 + i))
		if err := enc.EncodeValue(v); err != nil {
			panic(fmt.Sprintf("app.TaskFunc: gob-encoding arg %d: %v", i, err))
		}
	}

	// Ideally we would just create the task in the App Engine task queue with the
	// given name, and App Engine would take care of checking the name.
	// As is often the case, however, App Engine does not provide our ideal, and
	// so we must construct it by hand. Specifically, App Engine can take up to
	// seven days from the time a task completes successfully until that task's
	// name can be reused. We let App Engine create a unique App Engine name
	// for each task, and we use datastore entries to enforce constraints on our
	// own names.
	if !Lock(ctxt, "Task."+taskName, 10*365*24*time.Hour) {
		err := fmt.Errorf("app.Task: task %q already created and not yet completed", taskName)
		ctxt.Errorf("%v", err)
		return err
	}

	task := taskqueue.NewPOSTTask("/admin/app/taskpost", url.Values{
		"task": {taskName},
		"func": {funcName},
		"gob":  {buf.String()},
	})
	task.RetryOptions = tf.retry
	if _, err := taskqueue.Add(ctxt, task, tf.queue); err != nil {
		ctxt.Errorf("app.Task: creating task %q: taskqueue.Add: %v", taskName, err)
		return err
	}
	return nil
}

func init() {
	http.HandleFunc("/admin/app/taskpost", taskpost)
}

func taskpost(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	req.ParseForm()
	taskName := req.FormValue("task")
	funcName := req.FormValue("func")
	gobenc := req.FormValue("gob")
	if taskName == "" || funcName == "" {
		ctxt.Errorf("app.Task: taskpost called with task=%q, func=%q", taskName, funcName)
		w.WriteHeader(http.StatusNotFound)
		return
	}
	
	ctxt.Infof("taskpost %q %q", taskName, funcName)

	taskfuncs.RLock()
	tf := taskfuncs.m[funcName]
	taskfuncs.RUnlock()
	if tf == nil {
		ctxt.Errorf("app.Task: taskpost[%q,%q]: unknown func name", taskName, funcName)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	// Make sure a task does not run simultaneously on two app instances.
	// App Engine is not supposed to let this happen, but an admin might
	// be poking around and it's easy to guard against.
	if !Lock(ctxt, "TaskExec."+taskName, 15*time.Minute) {
		ctxt.Errorf("app.Task: taskpost[%q,%q]: already running", taskName, funcName)
		w.WriteHeader(http.StatusNotAcceptable)
		return
	}
	defer func() {
		if err := recover(); err != nil {
			ctxt.Errorf("app.Task: taskpost[%q,%q]: function panic: %v", taskName, funcName, err)
		}
		Unlock(ctxt, "TaskExec."+taskName)
	}()

	dec := gob.NewDecoder(strings.NewReader(gobenc))
	var vargs []reflect.Value
	vargs = append(vargs, reflect.ValueOf(&ctxt).Elem())
	t := tf.fn.Type()
	for i := 1; i < t.NumIn(); i++ {
		v := reflect.New(t.In(i)).Elem()
		if err := dec.DecodeValue(v); err != nil {
			ctxt.Errorf("app.Task: taskpost[%q,%q]: arg %d: gob decode failure: %v", taskName, funcName, i, err)
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
		vargs = append(vargs, v)
	}

	ret := tf.fn.Call(vargs)
	if len(ret) > 0 {
		err := ret[0].Interface()
		if err != nil {
			ctxt.Errorf("app.Task: taskpost[%q,%q]: function returned %v", taskName, funcName, err)
			w.WriteHeader(http.StatusNotAcceptable)
			return
		}
	}

	// Success!
	Unlock(ctxt, "Task." + taskName)
	return
}

func init() {
	TaskFunc("ping", ping, "default", nil)
	TaskFunc("pong", pong, "default", nil)
	http.HandleFunc("/admin/app/pingpong", startPing)
}

func startPing(w http.ResponseWriter, req *http.Request) {
	ctxt := appengine.NewContext(req)
	n, _ := strconv.Atoi(req.FormValue("n"))
	if n < 0 {
		ctxt.Errorf("invalid pingpong %q", req.FormValue("n"))
		return
	}

	Task(ctxt, "ping", "ping", n)
}

func ping(ctxt appengine.Context, n int) error {
	ctxt.Infof("ping %d", n)
	return Task(ctxt, "pong", "pong", n)
}

func pong(ctxt appengine.Context, n int) error {
	ctxt.Infof("pong %d", n)
	if n <= 0 {
		ctxt.Errorf("done!")
		return nil
	}
	return Task(ctxt, "ping", "ping", n-1)
}
