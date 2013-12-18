include $(GOROOT)/src/Make.inc

TARG=launchpad.net/goetveld/rietveld

GOFILES=\
	auth.go\
	bazaar.go\
	delta.go\
	doc.go\
	form.go\
	log.go\
	rietveld.go\
	terminal.go\

include $(GOROOT)/src/Make.pkg

runexample: _obj/launchpad.net/goetveld/rietveld.a
	$(GC) -I _obj -o example.$(O) example.go
	$(LD) -L _obj -o example example.$(O)
	./example

GOFMT=gofmt
BADFMT=$(shell $(GOFMT) -l $(GOFILES) $(wildcard *_test.go))

gofmt: $(BADFMT)
	@for F in $(BADFMT); do $(GOFMT) -w $$F && echo $$F; done

ifneq ($(BADFMT),)
ifneq ($(MAKECMDGOALS),gofmt)
#$(warning WARNING: make gofmt: $(BADFMT))
endif
endif
