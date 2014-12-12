package codereview

import (
	"bytes"
	"fmt"
	"html"
	"net/http"
	"strconv"
	"time"

	"app"
	"codereview/rietveld"

	"appengine"
	"appengine/datastore"
	"appengine/urlfetch"
	"appengine/user"

	"github.com/rsc/appstats"
)

type pw struct {
	User     string
	Password string
}

func (p *pw) Credentials(_, _ string) (user, passwd string, err error) {
	return p.User, p.Password, nil
}

func SetReviewer(ctxt appengine.Context, clnumber, who string) error {
	n, err := strconv.Atoi(clnumber)
	if err != nil {
		return fmt.Errorf("invalid cl number %q", clnumber)
	}
	u := user.Current(ctxt)
	if u == nil || u.Email == "" {
		return fmt.Errorf("must be logged in")
	}
	var password pw
	if err := app.ReadMeta(ctxt, "codereview.gobot.pw", &password); err != nil {
		return err
	}
	tr := &urlfetch.Transport{Context: ctxt}
	auth := rietveld.NewAuth(&password, false, "", ctxt)
	if err := auth.Login("https://codereview.appspot.com/", time.Time{}, tr); err != nil {
		ctxt.Criticalf("login: %s", err)
		return err
	}
	r := rietveld.New("https://codereview.appspot.com/", auth, tr)
	issue, err := r.Issue(n)
	if err != nil {
		ctxt.Criticalf("issue: %s", err)
		return err
	}
	rev := issue.ReviewerNicks
	if who != "close" && who != "golang-dev" {
		rev = append(rev, who)
	}
	c := &rietveld.Comment{
		Message:   "R=" + who + " (assigned by " + u.Email + ")",
		Reviewers: rev,
		Cc:        issue.CcNicks,
	}
	if err := r.AddComment(issue, c); err != nil {
		ctxt.Criticalf("addcomment: %s", err)
		return err
	}

	loadmsg(ctxt, "CL", clnumber)
	return nil
}

func RefreshCL(ctxt appengine.Context, clnumber string) {
	loadmsg(ctxt, "CL", clnumber)
}

func refresh(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	RefreshCL(ctxt, req.FormValue("cl"))
}

func init() {
	http.Handle("/admin/codereview/setreviewer", appstats.NewHandler(setreviewer))
	http.Handle("/admin/codereview/fixone", appstats.NewHandler(fixone))
	http.Handle("/admin/codereview/refresh", appstats.NewHandler(refresh))

	app.RegisterStatus("codereview golang-dev ⇒ golang-codereviews conversion", fixgolangstatus)

	app.ScanData("codereview.fixgolang-reviewer", 5*time.Minute,
		datastore.NewQuery("CL").
			Filter("Active =", true).
			Filter("Reviewers =", "golang-dev@googlegroups.com"),
		fixgolang)

	app.ScanData("codereview.fixgolang-cc", 5*time.Minute,
		datastore.NewQuery("CL").
			Filter("Active =", true).
			Filter("CC =", "golang-dev@googlegroups.com"),
		fixgolang)
}

func fixgolangstatus(ctxt appengine.Context) string {
	w := new(bytes.Buffer)

	const chunk = 1000
	keys, err := datastore.NewQuery("CL").
		Filter("Active =", true).
		Filter("Reviewers =", "golang-dev@googlegroups.com").
		KeysOnly().
		Limit(chunk).
		GetAll(ctxt, nil)
	if err != nil {
		fmt.Fprintf(w, "searching for active R=golang-dev: %v\n", err)
	} else {
		var ids []string
		for i, key := range keys {
			if i >= 10 {
				break
			}
			ids = append(ids, key.StringID())
		}
		fmt.Fprintf(w, "found %d active CLs with R=golang-dev: %v\n", len(keys), ids)
	}

	keys, err = datastore.NewQuery("CL").
		Filter("Active =", true).
		Filter("CC =", "golang-dev@googlegroups.com").
		KeysOnly().
		Limit(chunk).
		GetAll(ctxt, nil)
	if err != nil {
		fmt.Fprintf(w, "searching for active CC=golang-dev: %v\n", err)
	} else {
		var ids []string
		for i, key := range keys {
			if i >= 10 {
				break
			}
			ids = append(ids, key.StringID())
		}
		fmt.Fprintf(w, "found %d active CLs with CC=golang-dev: %v\n", len(keys), ids)
	}

	return "<pre>" + html.EscapeString(w.String()) + "</pre>\n"
}

func setreviewer(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	if err := SetReviewer(ctxt, req.FormValue("cl"), req.FormValue("who")); err != nil {
		fmt.Fprintf(w, "ERROR: %s\n", err)
	} else {
		fmt.Fprintf(w, "OK\n")
	}
}

func fixone(ctxt appengine.Context, w http.ResponseWriter, req *http.Request) {
	if err := fixgolang(ctxt, "CL", req.FormValue("cl")); err != nil {
		fmt.Fprintf(w, "ERROR: %s\n", err)
	} else {
		fmt.Fprintf(w, "OK\n")
	}
}

func fixgolang(ctxt appengine.Context, kind, key string) error {
	ctxt.Infof("fixgolang %s", key)
	n, err := strconv.Atoi(key)
	if err != nil {
		return fmt.Errorf("invalid cl number %q", key)
	}
	var password pw
	if err := app.ReadMeta(ctxt, "codereview.gobot.pw", &password); err != nil {
		return err
	}
	tr := &urlfetch.Transport{Context: ctxt}
	auth := rietveld.NewAuth(&password, false, "", ctxt)
	if err := auth.Login("https://codereview.appspot.com/", time.Time{}, tr); err != nil {
		ctxt.Criticalf("login: %s", err)
		return err
	}
	defer loadmsg(ctxt, "CL", key)
	r := rietveld.New("https://codereview.appspot.com/", auth, tr)
	issue, err := r.Issue(n)
	if err != nil {
		ctxt.Criticalf("issue: %s", err)
		return err
	}
	fixed := false
	for i, addr := range issue.ReviewerMails {
		if addr == "golang-dev@googlegroups.com" {
			issue.ReviewerMails[i] = "golang-codereviews@googlegroups.com"
			fixed = true
		}
	}
	for i, addr := range issue.CcMails {
		if addr == "golang-dev@googlegroups.com" {
			issue.CcMails[i] = "golang-codereviews@googlegroups.com"
			fixed = true
		}
	}
	if !fixed {
		return nil // already good
	}
	c := &rietveld.Comment{
		Message:   golangCodereviewMessage,
		Reviewers: issue.ReviewerMails,
		Cc:        issue.CcMails,
	}
	if err := r.AddComment(issue, c); err != nil {
		ctxt.Criticalf("addcomment: %s", err)
		return err
	}

	return nil
}

var golangCodereviewMessage = `Replacing golang-dev with golang-codereviews.

To the author of this CL: 

If you are using 'hg mail -r golang-dev' to mail the CL, use simply 'hg mail' instead.

If you did not name golang-dev explicitly and it was still added to the CL,
it means your working copy of the repo has a stale codereview.cfg
(or lib/codereview/codereview.cfg).
Please run 'hg sync' to update your client to the most recent codereview.cfg.
If the most recent codereview.cfg has defaultcc set to golang-dev instead of
golang-codereviews, please send a CL correcting it.

Thanks very much.
`
