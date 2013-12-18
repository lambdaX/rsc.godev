/*
Package rietveld is a copy of launchpad.net/goetveld/rietveld,
changed to avoid use of package unsafe and Linux-specific tty settings.

Package rietveld provides the ability for applications to communicate with
a Rietveld code review server to upload and manage patches.

The following example will create a new issue and send the difference between
the two Bazaar branches for review.

	delta, err := rietveld.BazaarDiffBranches(parentPath, branchPath)
	if err != nil {
		panic(err)
	}

	issue := &rietveld.Issue{
		Subject: "Change subject",
		Description: "The change description.",
	}

	err = rietveld.CodeReview.SendDelta(issue, delta, false)
	if err != nil {
		panic(err)
	}
	fmt.Printf("Created new issue: %s\n", rietveld.CodeReview.IssueURL(issue.Id))
*/
package rietveld
