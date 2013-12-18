var muting = true;

function mute(ev, dir) {
	var dirclass = "dir-" + dir.replace(/\//g, "\\/").replace(/\./g, "\\.");
	
	var outer = $(ev.delegateTarget);
	var muted = outer.text() == "mute";
	var op = "";
	if(muted) {
		outer.text("muting...");
		op = "mute";
	} else {
		outer.text("unmuting...");
		op = "unmute";
	}
	$.ajax({
		"type": "POST",
		"url": "/uiop",
		"data": {
			"dir": dir,
			"op": op
		},
		"success": function() {
			if(op == "mute") {
				$("tr." + dirclass).addClass("muted");
				if(muting) {
					$("tr."+dirclass).addClass("muting");
				}
				outer.text("unmute");
			} else {
				$("tr." + dirclass).removeClass("muted");
				$("tr."+dirclass).removeClass("muting");
				outer.text("mute");
			}
		},
		"error": function(xhr, status) {
			outer.text("failed: " + status)	
		}	
	})
}

function setreviewer(a, rev) {
	var clnumber = a.attr("id").replace("assign-", "");
	var who = rev.text();
	$.ajax({
		"type": "POST",
		"url": "/uiop",
		"data": {
			"cl": clnumber,
			"reviewer": who,
			"op": "reviewer"
		},
		"dataType": "text",
		"success": function(data) {
			a.text("edit");
			if(data.match(/^ERROR/)) {
				$("#err-" + clnumber).text(data);
				return;
			}
			rev.text(data);
		},
		"error": function(xhr, status) {
			a.text("failed: " + status)	
		}	
	})
}

$(document).ready(function() {
	$("a.mute").click(function(ev) {
		ev.preventDefault();
		var classes = $(ev.delegateTarget).attr("class").split(/\s+/);
		for(var i in classes) {
			var cl = classes[i];
			if(cl.substr(0,4) == "dir-") {
				mute(ev, cl.substr(4))
			}
		}
	})
	
	$("tr.muted").addClass("muting");
	
	$("#muteunmute").click(function(ev) {
		ev.preventDefault();
		muting = !muting;
		var a = $(ev.delegateTarget)
		if(muting) {
			a.text("show muted directories")
			$("tr.muted").addClass("muting");
		} else {
			a.text("hide muted directories")
			$("tr.muted").removeClass("muting");
		}
	})
	
	var action = false;
	$("#actionnoaction").click(function(ev) {
		ev.preventDefault();
		action = !action;
		var a = $(ev.delegateTarget)
		if(action) {
			a.text("show all issues/CLs")
			$("tr:not(.todo)").addClass("todohide");			
		} else {
			a.text("show my todo issues/CLs")
			$("tr:not(.todo)").removeClass("todohide");
		}
	})
	
	$("a.assignreviewer").click(function(ev) {
		ev.preventDefault();
		var a = $(ev.delegateTarget);
		var revid = a.attr("id").replace("assign-", "reviewer-");
		var rev = $("#" + revid);
		if(a.text() == "edit") {
			rev.attr("contenteditable", "true");
			rev.focus();
			a.addClass("big");
			a.text("save");
		} else if(a.text() == "save") {
			a.text("saving...");
			a.removeClass("big");
			rev.attr("contenteditable", "false");
			setreviewer(a, rev);
		}
	})
})
