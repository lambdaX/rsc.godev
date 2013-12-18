package codereview

import (
	"fmt"
	"time"
	"strconv"
	"net/http"
	
	"app"
	"codereview/rietveld"
	
	"appengine"
	"appengine/urlfetch"
	"appengine/user"
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
	if who != "close" {
		rev = append(rev, who)
	}
	c := &rietveld.Comment{
		Message: "R=" + who + " (assigned by " + u.Email + ")",
		Reviewers: rev,
		Cc: issue.CcNicks,
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

func init() {
	http.HandleFunc("/admin/codereview/setreviewer", setreviewer)
}

func setreviewer(w http.ResponseWriter, req *http.Request) {
	if err := SetReviewer(appengine.NewContext(req), req.FormValue("cl"), req.FormValue("who")); err != nil {
		fmt.Fprintf(w, "ERROR: %s\n", err)
	} else {
		fmt.Fprintf(w, "OK\n")
	}
}
