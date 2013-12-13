// Copyright 2013 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Derived from github.com/bradfitz/qopher/cmd/gotasks/gotasks.go

package codereview

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"reflect"
	"regexp"
	"strings"
	"time"

	"app"

	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"
)

type jsonCL struct {
	Issue      int64          `json:"issue"`
	Desc       string         `json:"description"`
	OwnerEmail string         `json:"owner_email"`
	Owner      string         `json:"owner"`
	Created    string         `json:"created"`
	Modified   string         `json:"modified"` // just a string; more reliable to do string equality tests on it
	Messages   []*jsonMessage `json:"messages"`
	Reviewers  []string       `json:"reviewers"`
	CC         []string       `json:"cc"`
	Closed     bool           `json:"closed"`
	PatchSets  []int64        `json:"patchsets"`
}

func parseTime(ctxt appengine.Context, s string) time.Time {
	t, err := time.ParseInLocation(timeFormat, s, time.UTC)
	if err != nil {
		return time.Unix(0, 0)
	}
	return t
}

func (j *jsonCL) toCL(ctxt appengine.Context) *CL {
	cl := &CL{
		CL:         fmt.Sprint(j.Issue),
		Desc:       j.Desc,
		OwnerEmail: j.OwnerEmail,
		Owner:      j.Owner,
		Created:    parseTime(ctxt, j.Created),
		Modified:   parseTime(ctxt, j.Modified),
		Reviewers:  j.Reviewers,
		CC:         j.CC,
		Closed:     j.Closed,
	}
	for _, m := range j.Messages {
		cl.Messages = append(cl.Messages, m.toMessage(ctxt))
	}
	for _, p := range j.PatchSets {
		cl.PatchSets = append(cl.PatchSets, fmt.Sprint(p))
	}
	return cl
}

type jsonMessage struct {
	Sender string `json:"sender"`
	Text   string `json:"text"`
	Date   string `json:"date"` // "2012-04-07 00:51:58.602055"
}

func (j *jsonMessage) toMessage(ctxt appengine.Context) Message {
	return Message{
		Sender: j.Sender,
		Text:   j.Text,
		Time:   parseTime(ctxt, j.Date),
	}
}

type jsonPatch struct {
	Files       []*jsonFile `json:"files"`
	Created     string      `json:"created"`
	Owner       string      `json:"owner"`
	NumComments int         `json:"num_comments"`
	PatchSet    int64       `json:"patchset"`
	Issue       int64       `json:"issue"`
	Message     string      `json:"message"`
	Modified    string      `json:"modified"`
}

func (j *jsonPatch) toPatch(ctxt appengine.Context) *Patch {
	return &Patch{
		CL:          fmt.Sprint(j.Issue),
		PatchSet:    fmt.Sprint(j.PatchSet),
		Created:     parseTime(ctxt, j.Created),
		Modified:    parseTime(ctxt, j.Modified),
		Owner:       j.Owner,
		NumComments: j.NumComments,
		Message:     j.Message,
	}
}

type jsonFile struct {
	Status          string `json:"status"`
	NumChunks       int    `json:"num_chunks"`
	NoBaseFile      bool   `json:"no_base_file"`
	PropertyChanges string `json:"property_changes"`
	NumAdded        int    `json:"num_added"`
	NumRemoved      int    `json:"num_removed"`
	ID              int64  `json:"id"`
	IsBinary        bool   `json:"is_binary"`
}

func (j *jsonFile) toFile(ctxt appengine.Context) File {
	return File{
		Status:          j.Status,
		NumChunks:       j.NumChunks,
		NoBaseFile:      j.NoBaseFile,
		PropertyChanges: j.PropertyChanges,
		NumAdded:        j.NumAdded,
		NumRemoved:      j.NumRemoved,
		ID:              fmt.Sprint(j.ID),
		IsBinary:        j.IsBinary,
	}
}

func init() {
	http.HandleFunc("/admin/codereview/show/", show)
}

func show(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	ctxt := appengine.NewContext(req)
	var cl CL
	err := app.ReadData(ctxt, "CL", strings.TrimPrefix(req.URL.Path, "/admin/codereview/show/"), &cl)
	if err != nil {
		fmt.Fprintf(w, "loading CL: %v\n", err)
		return
	}
	js, err := json.Marshal(cl)
	if err != nil {
		fmt.Fprintf(w, "encoding CL to JSON: %v\n", err)
		return
	}
	var buf bytes.Buffer
	json.Indent(&buf, js, "", "\t")
	w.Write(buf.Bytes())
}

func init() {
	app.Cron("codereview.load", 5*time.Minute, load)
}

func load(ctxt appengine.Context) error {
	// The deadline for task invocation is 10 minutes.
	// Stop when we've run for 5 minutes and ask to be rescheduled.
	deadline := time.Now().Add(5 * time.Minute)

	for _, reviewerOrCC := range []string{"reviewer", "cc"} {
		// The stored mtime is the most recent modification time we've seen.
		// We ask for all changes since then.
		mtimeKey := "codereview.mtime." + reviewerOrCC
		var mtime string
		if appengine.IsDevAppServer() {
			mtime = "2013-12-01 00:00:00" // limit fetching in empty datastore
		}
		app.ReadMeta(ctxt, mtimeKey, &mtime)
		cursor := ""

		// Rietveld gives us back times with microseconds, but it rejects microseconds
		// in the ModifiedAfter URL parameter. Drop them. We'll see a few of the most
		// recent CLs again. No big deal.
		if i := strings.Index(mtime, "."); i >= 0 {
			mtime = mtime[:i]
		}

		const itemsPerPage = 100
		for n := 0; ; n++ {
			var q struct {
				Cursor  string    `json:"cursor"`
				Results []*jsonCL `json:"results"`
			}
			err := fetchJSON(ctxt, &q, urlWithParams(queryTmpl, map[string]string{
				"ReviewerOrCC":  reviewerOrCC,
				"ModifiedAfter": mtime,
				"Order":         "modified",
				"Cursor":        cursor,
				"Limit":         fmt.Sprint(itemsPerPage),
			}))
			if err != nil {
				ctxt.Errorf("loading codereview by %s: URL <%s>: %v", reviewerOrCC, q, err)
				break
			}
			ctxt.Infof("found %d CLs", len(q.Results))
			if len(q.Results) == 0 {
				break
			}
			cursor = q.Cursor

			for _, jcl := range q.Results {
				cl := jcl.toCL(ctxt)
				if err := writeCL(ctxt, cl, mtimeKey, jcl.Modified); err != nil {
					break // error already logged
				}
			}

			if len(q.Results) < itemsPerPage {
				ctxt.Infof("reached end of results - codereview by %s up to date", reviewerOrCC)
				break
			}

			if time.Now().After(deadline) {
				ctxt.Infof("more to do for codereview by %s - rescheduling", reviewerOrCC)
				return app.ErrMoreCron
			}
		}
	}

	ctxt.Infof("all done")
	return nil
}

func writeCL(ctxt appengine.Context, cl *CL, mtimeKey, modified string) error {
	err := app.Transaction(ctxt, func(ctxt appengine.Context) error {
		var old CL
		if err := app.ReadData(ctxt, "CL", cl.CL, &old); err != nil && err != datastore.ErrNoSuchEntity {
			return err
		}
		if old.CL == "" { // no old data
			var count int64
			app.ReadMeta(ctxt, "codereview.count", &count)
			app.WriteMeta(ctxt, "codereview.count", count+1)
		}

		// Copy CL into original structure.
		// This allows us to maintain other information in the CL structure
		// and not overwrite it when the Rietveld information is updated.
		if old.Modified.After(cl.Modified) {
			return fmt.Errorf("CL %v: have %v but Rietveld sent %v", cl.CL, old.Modified, cl.Modified)
		}
		old.CL = cl.CL
		old.Desc = cl.Desc
		old.Owner = cl.Owner
		old.OwnerEmail = cl.OwnerEmail
		old.Created = cl.Created
		old.Modified = cl.Modified
		if cl.MessagesLoaded {
			old.Messages = cl.Messages
			old.Submitted = cl.Submitted
			old.MessagesLoaded = cl.MessagesLoaded
		}
		old.Reviewers = cl.Reviewers
		old.CC = cl.CC
		old.Closed = cl.Closed
		if !reflect.DeepEqual(old.PatchSets, cl.PatchSets) {
			old.PatchSets = cl.PatchSets
			old.PatchSetsLoaded = false
		}

		if err := app.WriteData(ctxt, "CL", cl.CL, &old); err != nil {
			return err
		}
		if mtimeKey != "" {
			app.WriteMeta(ctxt, mtimeKey, modified)
		}
		return nil
	})
	if err != nil {
		ctxt.Errorf("storing CL %v: %v", cl.CL, err)
	}
	return err
}

func init() {
	app.ScanData("codereview.loadmsg", 5*time.Minute,
		datastore.NewQuery("CL").Filter("MessagesLoaded =", false),
		loadmsg)
}

func loadmsg(ctxt appengine.Context, kind, key string) error {
	var jcl jsonCL
	err := fetchJSON(ctxt, &jcl, urlWithParams(issueTmpl, map[string]string{
		"CL": key,
	}))
	if err != nil {
		return nil // error already logged
	}
	cl := jcl.toCL(ctxt)
	cl.MessagesLoaded = true
	writeCL(ctxt, cl, "", "")
	return nil
}

func fetchJSON(ctxt appengine.Context, target interface{}, url string) error {
	http := urlfetch.Client(ctxt)

	res, err := http.Get(url)
	if err != nil {
		ctxt.Errorf("fetch URL <%s>: %v", url, err)
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		ctxt.Errorf("fetch URL <%s>: %v", url, res.Status)
		return fmt.Errorf("http %v", res.Status)
	}

	err = json.NewDecoder(res.Body).Decode(target)
	if err != nil {
		ctxt.Errorf("decoding JSON from URL <%s>: %v", url, err)
		return err
	}
	return nil
}

const (
	maxItemsPerPage = 1000

	timeFormat = "2006-01-02 15:04:05"

	// closed=1 means "unknown"
	queryTmpl = "https://codereview.appspot.com/search?closed=1&owner=&{{ReviewerOrCC}}=golang-dev@googlegroups.com&repo_guid=&base=&private=1&created_before=&created_after=&modified_before=&modified_after={{ModifiedAfter}}&order={{Order}}&format=json&keys_only=False&with_messages=False&cursor={{Cursor}}&limit={{Limit}}"

	// JSON with the text of messages. e.g.
	// https://codereview.appspot.com/api/6454085?messages=true
	issueTmpl = "https://codereview.appspot.com/api/{{CL}}?messages=true"
)

// itemsPerPage is the number of items to fetch for a single page.
// Changed by tests.
var itemsPerPage = 100 // maxItemsPerPage

var urlParam = regexp.MustCompile(`{{\w+}}`)

func urlWithParams(urlTempl string, m map[string]string) string {
	return urlParam.ReplaceAllStringFunc(urlTempl, func(param string) string {
		return url.QueryEscape(m[strings.Trim(param, "{}")])
	})
}

func init() {
	app.RegisterStatus("codereview", status)
}

func status(ctxt appengine.Context) string {
	w := new(bytes.Buffer)
	var t1, t2 string
	var count int64
	app.ReadMeta(ctxt, "codereview.mtime.reviewer", &t1)
	app.ReadMeta(ctxt, "codereview.mtime.cc", &t2)
	app.ReadMeta(ctxt, "codereview.count", &count)

	fmt.Fprintf(w, "Reviewers as of %v\nCC as of %v\n%d CLs total\n", t1, t2, count)
	fmt.Fprintln(w, time.Now())

	var chunk = 20000
	if appengine.IsDevAppServer() {
		chunk = 100
	}
	q := datastore.NewQuery("CL").
		Filter("PatchSetsLoaded <=", false).
		KeysOnly().
		Limit(chunk)

	n := 0
	it := q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "%d with PatchSetsLoaded <= false\n", n)
	fmt.Fprintln(w, time.Now())

	q = datastore.NewQuery("CL").
		Filter("MessagesLoaded <=", false).
		KeysOnly().
		Limit(chunk)

	n = 0
	it = q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "%d with MessagesLoaded = false\n", n)
	fmt.Fprintln(w, time.Now())

	fmt.Fprintf(w, "\n")

	q = datastore.NewQuery("RevTodo").
		Limit(10000).
		KeysOnly()

	n = 0
	it = q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "\n%d hg heads\n", n)
	fmt.Fprintln(w, time.Now())

	q = datastore.NewQuery("Meta").
		Filter("__key__ >=", datastore.NewKey(ctxt, "Meta", "commit.count.", 0, nil)).
		Filter("__key__ <=", datastore.NewKey(ctxt, "Meta", "commit.count/", 0, nil)).
		Limit(100)

	type meta struct {
		JSON []byte `datastore:",noindex"`
	}
	it = q.Run(ctxt)
	for {
		var m meta
		key, err := it.Next(&m)
		if err != nil {
			break
		}
		fmt.Fprintf(w, "%s %s\n\n", key.StringID(), m.JSON)
	}
	fmt.Fprintln(w, time.Now())

	q = datastore.NewQuery("CL").
		Filter("Closed =", false).
		Filter("Submitted =", false).
		Filter("HasReviewers =", true).
		Order("Summary").
		KeysOnly().
		Limit(20000)

	n = 0
	it = q.Run(ctxt)
	for {
		_, err := it.Next(nil)
		if err != nil {
			break
		}
		n++
	}
	fmt.Fprintf(w, "\n%d pending CLs.\n", n)
	fmt.Fprintln(w, time.Now())

	return "<pre>" + html.EscapeString(w.String()) + "</pre>\n"
}

/*
var updatewg = new(sync.WaitGroup)

var reviewMap = map[int]*Review{}

func Update() {
	for _, to := range []string{"reviewer", "cc"} {
		updatewg.Add(1)
		go loadReviews(to, updatewg)
	}
	updatewg.Wait()

	for _, r := range allReviews() {
		reviewMap[r.Issue] = r
	}

	for _, r := range reviewMap {
		for _, patchID := range r.PatchSets {
			updatewg.Add(1)
			go func(r *Review, id int) {
				defer updatewg.Done()
				if err := r.LoadPatchMeta(id); err != nil {
					log.Fatal(err)
				}
			}(r, patchID)
		}
	}
	updatewg.Wait()
}

func loadReviews(to string, wg *sync.WaitGroup) {
	defer wg.Done()
	cursor := ""
	for {
		url := urlWithParams(queryTmpl, map[string]string{
			"CC_OR_REVIEWER": to,
			"CURSOR":         cursor,
			"LIMIT":          fmt.Sprint(itemsPerPage),
		})
		log.Printf("Fetching %s", url)
		res, err := http.Get(url)
		if err != nil {
			log.Fatal(err)
		}
		var reviews []*Review
		reviews, cursor, err = ParseReviews(res.Body)
		if err != nil {
			log.Fatal(err)
		}
		var nfetch, nold int
		for _, r := range reviews {
			old := reviewMap[r.Issue]
			if old != nil && old.Modified == r.Modified {
				nold++
			} else {
				nfetch++
				wg.Add(1)
				go updateReview(r, wg)
			}
		}
		log.Printf("for cursor %q, Got %d reviews (%d updated, %d old)", cursor, len(reviews), nfetch, nold)
		res.Body.Close()
		if cursor == "" || len(reviews) == 0 || nold > 0 {
			break
		}
	}
}

var httpGate = make(chan bool, 25)

func gate() (ungate func()) {
	httpGate <- true
	return func() {
		<-httpGate
	}
}

// updateReview checks to see if r (which lacks comments) has a higher
// modification time than the version we have on disk and if necessary
// fetches the full (with comments) version of r and puts it on disk.
func updateReview(r *Review, wg *sync.WaitGroup) {
	defer wg.Done()
	dr, err := loadDiskFullReview(r.Issue)
	if err == nil && dr.Modified == r.Modified {
		// Nothing to do.
		return
	}
	if err != nil && !os.IsNotExist(err) {
		log.Fatalf("Error loading issue %d: %v", r.Issue, err)
	}

	dstFile := issueDiskPath(r.Issue)
	dir := filepath.Dir(dstFile)
	if err := os.MkdirAll(dir, 0755); err != nil {
		log.Fatal(err)
	}

	defer gate()()

	url := urlWithParams(reviewTmpl, map[string]string{
		"CL": fmt.Sprint(r.Issue),
	})
	log.Printf("Fetching %s", url)
	res, err := http.Get(url)
	if err != nil || res.StatusCode != 200 {
		log.Fatalf("Error fetching %s: %+v, %v", url, res, err)
	}
	defer res.Body.Close()

	if err := writeReadableJSON(dstFile, res.Body); err != nil {
		log.Fatal(err)
	}
}

type Message struct {
	Sender string `json:"sender"`
	Text   string `json:"text"`
	Date   string `json:"date"` // "2012-04-07 00:51:58.602055"
}

type byMessageDate []*Message

func (s byMessageDate) Len() int           { return len(s) }
func (s byMessageDate) Less(i, j int) bool { return s[i].Date < s[j].Date }
func (s byMessageDate) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

// Time unmarshals a time in rietveld's format.
type Time time.Time

func (t *Time) UnmarshalJSON(b []byte) error {
	if len(b) < 2 || b[0] != '"' || b[len(b)-1] != '"' {
		return fmt.Errorf("types: failed to unmarshal non-string value %q as an RFC 3339 time")
	}
	// TODO: pic
	tm, err := time.Parse("2006-01-02 15:04:05", string(b[1:len(b)-1]))
	if err != nil {
		return err
	}
	*t = Time(tm)
	return nil
}

func (t Time) String() string { return time.Time(t).String() }

type Review struct {
	Issue      int        `json:"issue"`
	Desc       string     `json:"description"`
	OwnerEmail string     `json:"owner_email"`
	Owner      string     `json:"owner"`
	Created    Time       `json:"created"`
	Modified   string     `json:"modified"` // just a string; more reliable to do string equality tests on it
	Messages   []*Message `json:"messages"`
	Reviewers  []string   `json:"reviewers"`
	CC         []string   `json:"cc"`
	Closed     bool       `json:"closed"`
	PatchSets  []int      `json:"patchsets"`
}

// Reviewer returns the email address of an explicit reviewer, if any, else
// returns the empty string.
func (r *Review) Reviewer() string {
	for _, who := range r.Reviewers {
		if strings.HasSuffix(who, "@googlegroups.com") {
			continue
		}
		return who
	}
	return ""
}

func (r *Review) LoadPatchMeta(patch int) error {
	path := patchDiskPatch(r.Issue, patch)
	if fi, err := os.Stat(path); err == nil && fi.Size() != 0 {
		return nil
	}

	defer gate()()

	url := fmt.Sprintf("https://codereview.appspot.com/api/%d/%d", r.Issue, patch)
	log.Printf("Fetching patch %s", url)
	res, err := http.Get(url)
	if err != nil || res.StatusCode != 200 {
		return fmt.Errorf("Error fetching %s (issue %d, patch %d): %+v, %v", url, r.Issue, patch, res, err)
	}
	defer res.Body.Close()

	return writeReadableJSON(path, res.Body)
}
*/
